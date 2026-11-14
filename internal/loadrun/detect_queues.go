package loadrun

import (
	"fmt"
	"sort"
)

// syncWrites are operations that write to a backend synchronously, so a
// burst of traffic hits the backend at the burst's rate.
var syncWrites = map[opKey]bool{
	{"dynamodb", "PutItem"}: true, {"dynamodb", "UpdateItem"}: true, {"dynamodb", "DeleteItem"}: true,
	{"dynamodb", "BatchWriteItem"}: true, {"dynamodb", "TransactWriteItems"}: true,
	{"lambda", "Invoke"}:     true,
	{"kinesis", "PutRecord"}: true, {"kinesis", "PutRecords"}: true,
	{"stepfunctions", "StartSyncExecution"}: true,
}

// queueOps are how work gets buffered instead of written synchronously.
var queueOps = []opKey{
	{"sqs", "SendMessage"}, {"sqs", "SendMessageBatch"},
	{"sns", "Publish"}, {"sns", "PublishBatch"},
	{"events", "PutEvents"}, {"firehose", "PutRecord"}, {"firehose", "PutRecordBatch"},
}

// Missing queue thresholds: throttling that lasts, not a blip.
const (
	queueMinThrottledSeconds = 5
	queueMinThrottleShare    = 0.05
	queueMaxQueuedShare      = 0.01
)

// queues finds synchronous writes that stay throttled through the run
// while little or no work goes through a queue: the backend is taking the
// load at the load's rate rather than at a rate it can sustain.
type queues struct {
	// throttledSeconds holds, per write operation, the seconds of the run
	// in which it was throttled. A run's timeline is at most an hour, so
	// each set is bounded.
	throttledSeconds map[opKey]map[int]bool
	// asyncInvokes counts Lambda invokes that were queued (Event).
	asyncInvokes int
}

func newQueues() *queues { return &queues{throttledSeconds: map[opKey]map[int]bool{}} }

func (q *queues) observe(o Observation, oc outcome, sec int) {
	k := opKey{o.Service, o.Operation}
	if k == (opKey{"lambda", "Invoke"}) && o.InvocationType != "" && o.InvocationType != "RequestResponse" {
		q.asyncInvokes++
		return
	}
	if !syncWrites[k] || oc != outcomeThrottled || sec >= maxTimeline {
		return
	}
	set := q.throttledSeconds[k]
	if set == nil {
		set = map[int]bool{}
		q.throttledSeconds[k] = set
	}
	set[sec] = true
}

func (q *queues) findings(f runFacts) []Finding {
	queued := q.asyncInvokes
	for _, k := range queueOps {
		queued += f.count(k.service, k.operation)
	}
	keys := make([]opKey, 0, len(q.throttledSeconds))
	for k := range q.throttledSeconds {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].service+keys[i].operation < keys[j].service+keys[j].operation })

	var out []Finding
	for _, k := range keys {
		c := f.operations[k]
		secs := len(q.throttledSeconds[k])
		if c == nil || secs < queueMinThrottledSeconds || float64(c.throttled) < queueMinThrottleShare*float64(c.requests) {
			continue
		}
		if float64(queued) > queueMaxQueuedShare*float64(c.requests) {
			continue
		}
		share := float64(c.throttled) / float64(c.requests)
		fd := Finding{
			Detector: DetectorMissingQueue, Severity: SeverityHigh, Service: k.service, Operation: k.operation,
			Title:  fmt.Sprintf("%s %s throttled for %d seconds with no queue in front", k.service, k.operation, secs),
			Detail: fmt.Sprintf("%d of %d %s calls (%s) were throttled, in %d separate seconds of the run, and almost no work went through a queue.", c.throttled, c.requests, k.operation, pct(share), secs),
			Suggestion: "Writes arrive at the load's rate and the backend refuses what it cannot take. Put an SQS queue in front: " +
				"accept the request, enqueue it, and let a consumer with capped concurrency (a Lambda event source mapping with maximum concurrency) write at a rate the backend sustains.",
			Evidence: map[string]any{"requests": c.requests, "throttled": c.throttled, "throttled_seconds": secs, "queued_requests": queued},
		}
		if k.service == "lambda" {
			fd.Suggestion = "Synchronous invokes beyond the function's concurrency are rejected. If the caller does not need the result, " +
				"invoke with InvocationType Event or send the work to SQS, so Lambda queues it and retries throttles for you."
		}
		out = append(out, fd)
	}
	return out
}
