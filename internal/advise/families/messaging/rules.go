package messaging

import (
	"fmt"
	"math"
	"strconv"

	"github.com/stratus/internal/advise"
)

// quotaReader reads documented quotas for one region and turns missing
// ones into "not checked" reasons rather than silent passes.
type quotaReader struct {
	src    advise.QuotaSource
	region string
}

func (q quotaReader) get(id string) (advise.Quota, bool) {
	if q.src == nil {
		return advise.Quota{}, false
	}
	return q.src.Quota(id, q.region)
}

func unchecked(id string) advise.Reason {
	return advise.Reason{Kind: advise.ReasonUnchecked, Text: "Quota " + id + " is not modelled, so it was not checked."}
}

func reason(kind advise.ReasonKind, q advise.Quota, format string, args ...any) advise.Reason {
	return advise.Reason{Kind: kind, Text: fmt.Sprintf(format, args...), Citation: &advise.Citation{QuotaID: q.ID, URL: q.Source}}
}

func plain(kind advise.ReasonKind, format string, args ...any) advise.Reason {
	return advise.Reason{Kind: kind, Text: fmt.Sprintf(format, args...)}
}

// num prints a number with thousands separators and no trailing zeros.
func num(f float64) string {
	if f != math.Trunc(f) {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	s := strconv.FormatFloat(f, 'f', 0, 64)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// kib prints a size in KiB, or in MiB when it is a whole number of them.
func kib(bytes float64) string {
	const bytesPerMiB = bytesPerKiB * bytesPerKiB
	if bytes >= bytesPerMiB && math.Mod(bytes, bytesPerMiB) == 0 {
		return num(bytes/bytesPerMiB) + " MiB"
	}
	return num(bytes/bytesPerKiB) + " KiB"
}

// sizeCheck rules an option out when a message is larger than its maximum.
func sizeCheck(w workload, q quotaReader, id, service string) []advise.Reason {
	limit, ok := q.get(id)
	if !ok {
		return []advise.Reason{unchecked(id)}
	}
	if w.sizeBytes > limit.Value {
		return []advise.Reason{reason(advise.ReasonRuledOut, limit, "A %s message is larger than %s's %s maximum.", kib(w.sizeBytes), service, kib(limit.Value))}
	}
	return nil
}

func retentionCheck(w workload, q quotaReader, id, service string) []advise.Reason {
	limit, ok := q.get(id)
	if !ok {
		return []advise.Reason{unchecked(id)}
	}
	if w.retentionDays > limit.Value {
		return []advise.Reason{reason(advise.ReasonRuledOut, limit, "Keeping messages %s days is longer than %s's %s-day maximum.", num(w.retentionDays), service, num(limit.Value))}
	}
	return nil
}

func kafkaCheck(w workload, name string) []advise.Reason {
	if w.kafka {
		return []advise.Reason{plain(advise.ReasonRuledOut, "Producers or consumers need the Kafka API, which %s does not speak.", name)}
	}
	return nil
}

func atLeastOnceCheck(w workload, name string) []advise.Reason {
	if w.exactlyOnce {
		return []advise.Reason{plain(advise.ReasonCaveat, "%s delivers at least once, so exactly-once processing needs consumers that deduplicate.", name)}
	}
	return nil
}

func routingCheck(w workload, name string) []advise.Reason {
	if w.routing {
		return []advise.Reason{plain(advise.ReasonCaveat, "%s does not route by content; every consumer gets everything and filters it.", name)}
	}
	return nil
}

func singleConsumer(w workload, name string) []advise.Reason {
	if w.consumers > 1 {
		return []advise.Reason{plain(advise.ReasonRuledOut, "%s hands each message to one consumer; %d independent consumers need fan-out (SNS or EventBridge to a queue each) or a stream.", name, w.consumers)}
	}
	return nil
}

func sqsStandard(w workload, q quotaReader) []advise.Reason {
	var rs []advise.Reason
	if w.ordering != orderNone {
		rs = append(rs, plain(advise.ReasonRuledOut, "Standard queues make a best effort at order but do not guarantee it."))
	}
	if w.replay {
		rs = append(rs, plain(advise.ReasonRuledOut, "A consumed message is deleted, so it cannot be replayed."))
	}
	rs = append(rs, singleConsumer(w, "A queue")...)
	rs = append(rs, kafkaCheck(w, "SQS")...)
	rs = append(rs, sizeCheck(w, q, "sqs-message-size", "SQS")...)
	rs = append(rs, retentionCheck(w, q, "sqs-retention", "SQS")...)
	rs = append(rs, atLeastOnceCheck(w, "A standard queue")...)
	rs = append(rs, routingCheck(w, "A queue")...)
	rs = append(rs, plain(advise.ReasonStrength, "Standard queues have no documented per-action throughput quota, so %s messages/s needs no capacity planning.", num(w.peak)))
	return rs
}

func sqsFIFO(w workload, q quotaReader) []advise.Reason {
	var rs []advise.Reason
	if w.replay {
		rs = append(rs, plain(advise.ReasonRuledOut, "A consumed message is deleted, so it cannot be replayed."))
	}
	rs = append(rs, singleConsumer(w, "A FIFO queue")...)
	rs = append(rs, kafkaCheck(w, "SQS")...)
	rs = append(rs, sizeCheck(w, q, "sqs-message-size", "SQS")...)
	rs = append(rs, retentionCheck(w, q, "sqs-retention", "SQS")...)
	rs = append(rs, fifoThroughput(w, q)...)
	if w.ordering != orderNone {
		rs = append(rs, plain(advise.ReasonStrength, "Keeps messages in order within each message group."))
	}
	if w.exactlyOnce {
		rs = append(rs, plain(advise.ReasonStrength, "Deduplicates sends by deduplication ID, so processing can be exactly once."))
	}
	rs = append(rs, routingCheck(w, "A queue")...)
	if !w.needsStream() && !w.exactlyOnce {
		rs = append(rs, plain(advise.ReasonCaveat, "Nothing here needs order or deduplication; a standard queue does the same without a throughput quota."))
	}
	return rs
}

// fifoThroughput checks the peak rate against FIFO quotas. One global order
// means one message group, which cannot use high-throughput mode's spread.
func fifoThroughput(w workload, q quotaReader) []advise.Reason {
	unbatched, ok1 := q.get("sqs-fifo-unbatched-tps")
	batched, ok2 := q.get("sqs-fifo-batched-tps")
	if !ok1 || !ok2 {
		return []advise.Reason{unchecked("sqs-fifo-batched-tps")}
	}
	if w.ordering == orderGlobal {
		switch {
		case w.peak > batched.Value:
			return []advise.Reason{reason(advise.ReasonRuledOut, batched, "One global order means one message group, capped at %s messages/s even with batching; the peak is %s.", num(batched.Value), num(w.peak))}
		case w.peak > unbatched.Value:
			return []advise.Reason{reason(advise.ReasonCaveat, unbatched, "%s messages/s is above the %s unbatched transactions/s quota; producers and consumers must batch.", num(w.peak), num(unbatched.Value))}
		}
		return nil
	}
	high, ok := q.get("sqs-fifo-high-throughput-batched-tps")
	switch {
	case !ok && w.peak > batched.Value:
		return []advise.Reason{unchecked("sqs-fifo-high-throughput-batched-tps")}
	case ok && w.peak > high.Value:
		return []advise.Reason{reason(advise.ReasonRuledOut, high, "%s messages/s is above even high-throughput FIFO's %s in %s.", num(w.peak), num(high.Value), q.region)}
	case w.peak > batched.Value:
		return []advise.Reason{reason(advise.ReasonCaveat, high, "%s messages/s needs high-throughput mode (up to %s in %s) and many message groups.", num(w.peak), num(high.Value), q.region)}
	case w.peak > unbatched.Value:
		return []advise.Reason{reason(advise.ReasonCaveat, unbatched, "%s messages/s is above the %s unbatched transactions/s quota; batch sends or turn on high-throughput mode.", num(w.peak), num(unbatched.Value))}
	}
	return nil
}

func snsFanout(w workload, q quotaReader) []advise.Reason {
	var rs []advise.Reason
	if w.ordering != orderNone {
		rs = append(rs, plain(advise.ReasonRuledOut, "Standard topics do not keep order. (FIFO topics with FIFO queues do, at FIFO throughput; not modelled here.)"))
	}
	if w.replay {
		rs = append(rs, plain(advise.ReasonRuledOut, "Queues delete what is consumed, so messages cannot be replayed."))
	}
	rs = append(rs, kafkaCheck(w, "SNS")...)
	rs = append(rs, sizeCheck(w, q, "sns-message-size", "SNS")...)
	rs = append(rs, retentionCheck(w, q, "sqs-retention", "SQS")...)
	if publish, ok := q.get("sns-publish-rate"); !ok {
		rs = append(rs, unchecked("sns-publish-rate"))
	} else if w.peak > publish.Value {
		rs = append(rs, reason(advise.ReasonCaveat, publish, "%s messages/s is above SNS's default %s published messages/s in %s; request an increase (the quota is adjustable).", num(w.peak), num(publish.Value), q.region))
	}
	rs = append(rs, atLeastOnceCheck(w, "SNS")...)
	switch {
	case w.consumers > 1:
		rs = append(rs, plain(advise.ReasonStrength, "Each of the %d consumers gets its own queue and its own copy, and can fail without affecting the others.", w.consumers))
	case !w.routing:
		rs = append(rs, plain(advise.ReasonCaveat, "With one consumer and no routing, fan-out adds a hop and a second bill for nothing."))
	}
	if w.routing {
		rs = append(rs, plain(advise.ReasonStrength, "Subscription filter policies route messages by their attributes or body."))
	}
	return rs
}

func eventBridge(w workload, q quotaReader) []advise.Reason {
	var rs []advise.Reason
	if w.ordering != orderNone {
		rs = append(rs, plain(advise.ReasonRuledOut, "EventBridge does not deliver events in order."))
	}
	rs = append(rs, kafkaCheck(w, "EventBridge")...)
	if put, ok := q.get("eventbridge-putevents-rate"); !ok {
		rs = append(rs, unchecked("eventbridge-putevents-rate"))
	} else if w.peak > put.Value {
		rs = append(rs, reason(advise.ReasonCaveat, put, "%s events/s is above the default %s PutEvents requests/s in %s; send several entries per request or request an increase.", num(w.peak), num(put.Value), q.region))
	}
	if targets, ok := q.get("eventbridge-targets-per-rule"); ok && float64(w.consumers) > targets.Value {
		rs = append(rs, reason(advise.ReasonCaveat, targets, "%d consumers is more than %s targets per rule; split them across rules.", w.consumers, num(targets.Value)))
	}
	if w.sizeBytes > 256*bytesPerKiB {
		rs = append(rs, plain(advise.ReasonUnchecked, "EventBridge's per-event size limit is not modelled; check it for %s events.", kib(w.sizeBytes)))
	}
	if w.replay {
		rs = append(rs, plain(advise.ReasonCaveat, "Replay works through archives, which re-send past events to rules; consumers cannot re-read from a position of their own."))
	}
	rs = append(rs, atLeastOnceCheck(w, "EventBridge")...)
	switch {
	case w.routing:
		rs = append(rs, plain(advise.ReasonStrength, "Rules match events by content and send each to the targets that want it."))
	case w.consumers > 1:
		rs = append(rs, plain(advise.ReasonStrength, "One rule can send every event to up to five targets."))
	default:
		rs = append(rs, plain(advise.ReasonCaveat, "With one consumer and no routing, an event bus adds a hop for nothing."))
	}
	return rs
}

func kinesis(w workload, q quotaReader) []advise.Reason {
	var rs []advise.Reason
	rs = append(rs, kafkaCheck(w, "Kinesis")...)
	rs = append(rs, sizeCheck(w, q, "kinesis-record-size", "Kinesis")...)
	rs = append(rs, retentionCheck(w, q, "kinesis-retention", "Kinesis")...)
	records, ok1 := q.get("kinesis-shard-write-records")
	bytes, ok2 := q.get("kinesis-shard-write-bytes")
	if !ok1 || !ok2 {
		rs = append(rs, unchecked("kinesis-shard-write-records"))
	} else {
		shards := shardsFor(w, records.Value, bytes.Value)
		if w.ordering == orderGlobal {
			if shards > 1 {
				rs = append(rs, reason(advise.ReasonRuledOut, records, "One global order means one shard, capped at %s records/s and %s/s; the peak needs %d shards.", num(records.Value), kib(bytes.Value), shards))
			} else {
				rs = append(rs, reason(advise.ReasonCaveat, records, "One global order means one shard, which caps the stream at %s records/s for good.", num(records.Value)))
			}
		} else {
			rs = append(rs, reason(advise.ReasonStrength, records, "The peak needs %d shard(s) in provisioned mode (%s records/s and %s/s each), or on-demand mode.", shards, num(records.Value), kib(bytes.Value)))
		}
		if read, ok := q.get("kinesis-shard-read-bytes"); ok && w.consumers > 1 {
			if need := float64(w.consumers) * w.peak * w.sizeBytes; need > float64(shards)*read.Value {
				rs = append(rs, reason(advise.ReasonCaveat, read, "%d consumers reading every record exceed %s/s of shared reads per shard; use enhanced fan-out or more shards.", w.consumers, kib(read.Value)))
			}
		}
	}
	if w.ordering == orderPerKey {
		rs = append(rs, plain(advise.ReasonStrength, "Keeps records in order per partition key."))
	}
	if w.replay {
		rs = append(rs, plain(advise.ReasonStrength, "Consumers can re-read from any point within the retention period."))
	}
	if w.consumers > 1 {
		rs = append(rs, plain(advise.ReasonStrength, "Each of the %d consumers reads the whole stream at its own pace.", w.consumers))
	}
	rs = append(rs, atLeastOnceCheck(w, "Kinesis")...)
	rs = append(rs, routingCheck(w, "A stream")...)
	if !w.needsStream() {
		rs = append(rs, plain(advise.ReasonCaveat, "Nothing here needs a stream (ordering, replay or several consumers); a queue is simpler to run."))
	}
	return rs
}

// provisionedShards is how many shards provisioned mode needs for the
// peak, from the per-shard write quotas; one, and false, when those
// quotas are not modelled.
func provisionedShards(w workload, q quotaReader) (int, bool) {
	records, ok1 := q.get("kinesis-shard-write-records")
	bytes, ok2 := q.get("kinesis-shard-write-bytes")
	if !ok1 || !ok2 {
		return 1, false
	}
	return shardsFor(w, records.Value, bytes.Value), true
}

func shardsFor(w workload, recordsPerShard, bytesPerShard float64) int {
	need := math.Max(w.peak/recordsPerShard, w.peak*w.sizeBytes/bytesPerShard)
	return int(math.Max(1, math.Ceil(need)))
}

func msk(w workload) []advise.Reason {
	var rs []advise.Reason
	if w.kafka {
		rs = append(rs, plain(advise.ReasonStrength, "Speaks the Kafka API, so existing Kafka clients, connectors and tools work unchanged."))
	} else {
		rs = append(rs, plain(advise.ReasonCaveat, "Nothing here needs Kafka; a managed queue or stream does the job with no cluster to run."))
	}
	switch w.ordering {
	case orderPerKey:
		rs = append(rs, plain(advise.ReasonStrength, "Keeps records in order per partition key."))
	case orderGlobal:
		rs = append(rs, plain(advise.ReasonCaveat, "One global order means one partition, which caps throughput at what one partition takes."))
	}
	if w.replay {
		rs = append(rs, plain(advise.ReasonStrength, "Consumers can re-read from any offset within retention, which can be long or unlimited."))
	}
	if w.consumers > 1 {
		rs = append(rs, plain(advise.ReasonStrength, "Each consumer group reads the whole topic independently."))
	}
	if w.exactlyOnce {
		rs = append(rs, plain(advise.ReasonStrength, "Idempotent producers and transactions support exactly-once processing."))
	}
	rs = append(rs, routingCheck(w, "Kafka")...)
	return rs
}
