package loadrun

import (
	"fmt"
	"sort"
)

// batchAPIs maps single-item operations to the batch operation that does
// the same work for many items in one call.
var batchAPIs = map[opKey]string{
	{"dynamodb", "GetItem"}:            "BatchGetItem",
	{"dynamodb", "PutItem"}:            "BatchWriteItem",
	{"dynamodb", "DeleteItem"}:         "BatchWriteItem",
	{"sqs", "SendMessage"}:             "SendMessageBatch",
	{"sqs", "DeleteMessage"}:           "DeleteMessageBatch",
	{"sqs", "ChangeMessageVisibility"}: "ChangeMessageVisibilityBatch",
	{"sns", "Publish"}:                 "PublishBatch",
	{"kinesis", "PutRecord"}:           "PutRecords",
	{"firehose", "PutRecord"}:          "PutRecordBatch",
}

// Chatty call thresholds: a sustained rate of single-item calls with a
// batch API that the run barely used.
const (
	chattyMinRate     = 20.0 // per second
	chattyMinRequests = 200
	chattyBatchShare  = 0.1
)

// batchableCalls finds single-item operations called at a high rate when
// a batch operation exists and is barely used. It reads the run's
// per-operation counts, so it keeps no state of its own.
type batchableCalls struct{}

func (batchableCalls) observe(Observation, outcome, int) {}

func (batchableCalls) findings(f runFacts) []Finding {
	var out []Finding
	for _, k := range sortedOps(f) {
		batch := batchAPIs[k]
		c := f.operations[k]
		if batch == "" || c.requests < chattyMinRequests || f.rate(c.requests) < chattyMinRate {
			continue
		}
		batched := f.count(k.service, batch)
		if float64(batched) >= chattyBatchShare*float64(c.requests) {
			continue
		}
		fd := Finding{
			Detector: DetectorChattyCalls, Severity: SeverityLow, Service: k.service, Operation: k.operation,
			Title:  fmt.Sprintf("%d %s %s calls a second, %s barely used", int(f.rate(c.requests)), k.service, k.operation, batch),
			Detail: fmt.Sprintf("%s was called %d times (%.0f/s) and %s %d times.", k.operation, c.requests, f.rate(c.requests), batch, batched),
			Suggestion: fmt.Sprintf("If these calls come from a loop over items, send them with %s: fewer requests, "+
				"less time waiting on the network and fewer chances to be throttled. One call per incoming request is fine.", batch),
			Evidence: map[string]any{"requests": c.requests, "per_second": round(f.rate(c.requests)), "batch_operation": batch, "batch_requests": batched},
		}
		if c.throttled > 0 {
			fd.Severity = SeverityMedium
			fd.Evidence["throttled"] = c.throttled
		}
		out = append(out, fd)
	}
	return out
}

// cacheableOps return data that rarely changes: configuration, secrets,
// identities and resource metadata. Fetching them on every request costs
// latency and runs into low quotas (SSM GetParameter allows 40 a second
// by default).
var cacheableOps = map[opKey]bool{
	{"ssm", "GetParameter"}: true, {"ssm", "GetParameters"}: true, {"ssm", "GetParametersByPath"}: true,
	{"secretsmanager", "GetSecretValue"}: true, {"secretsmanager", "BatchGetSecretValue"}: true,
	{"sts", "GetCallerIdentity"}: true, {"sts", "AssumeRole"}: true,
	{"dynamodb", "DescribeTable"}: true, {"dynamodb", "ListTables"}: true,
	{"sqs", "GetQueueUrl"}: true, {"sqs", "GetQueueAttributes"}: true, {"sqs", "ListQueues"}: true,
	{"sns", "ListTopics"}: true, {"lambda", "GetFunction"}: true, {"lambda", "ListFunctions"}: true,
	{"s3", "ListBuckets"}: true, {"kms", "DescribeKey"}: true, {"appconfig", "GetConfiguration"}: true,
	{"cognitoidp", "DescribeUserPool"}: true,
}

// Cacheable read thresholds.
const (
	cacheMinRate     = 5.0
	cacheMinRequests = 50
)

// cacheableReads finds configuration and metadata reads made at a rate
// that suggests they are fetched per request instead of cached.
type cacheableReads struct{}

func (cacheableReads) observe(Observation, outcome, int) {}

func (cacheableReads) findings(f runFacts) []Finding {
	var out []Finding
	for _, k := range sortedOps(f) {
		c := f.operations[k]
		if !cacheableOps[k] || c.requests < cacheMinRequests || f.rate(c.requests) < cacheMinRate {
			continue
		}
		fd := Finding{
			Detector: DetectorCacheableReads, Severity: SeverityMedium, Service: k.service, Operation: k.operation,
			Title:  fmt.Sprintf("%s %s called %.0f times a second", k.service, k.operation, f.rate(c.requests)),
			Detail: fmt.Sprintf("%s returns data that rarely changes, but was called %d times during the run.", k.operation, c.requests),
			Suggestion: "Fetch it once at startup or cold start and cache it with a TTL (the AWS Parameters and Secrets " +
				"Lambda extension does this for SSM and Secrets Manager). Per-request lookups add latency and hit low quotas first.",
			Evidence: map[string]any{"requests": c.requests, "per_second": round(f.rate(c.requests))},
		}
		if c.throttled > 0 {
			fd.Severity = SeverityHigh
			fd.Evidence["throttled"] = c.throttled
		}
		out = append(out, fd)
	}
	return out
}

func sortedOps(f runFacts) []opKey {
	keys := make([]opKey, 0, len(f.operations))
	for k := range f.operations {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].service+keys[i].operation < keys[j].service+keys[j].operation
	})
	return keys
}
