package limits

import (
	"maps"
	"sort"
)

// Unit says what a documented quota's value counts.
type Unit string

const (
	UnitPerSecond     Unit = "per_second"
	UnitBytes         Unit = "bytes"
	UnitBytesPerSec   Unit = "bytes_per_second"
	UnitDays          Unit = "days"
	UnitCount         Unit = "count"
	UnitEntriesPerReq Unit = "entries_per_request"
)

// Quota is one AWS default quota as AWS documents it: a number with its
// unit, what it applies to and the page it comes from. Rate rules that
// Stratus enforces and quotas that only inform advice (record sizes,
// retention, per-shard throughput) live here together, so a number that
// AWS publishes is written down once.
type Quota struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	// Description says what is counted, in AWS's terms.
	Description string  `json:"description"`
	Value       float64 `json:"value"`
	Unit        Unit    `json:"unit"`
	// Per says what shares the quota: "account" (per account and region),
	// "queue", "topic", "shard", "message", "request" and the like.
	Per string `json:"per"`
	// RegionValues overrides Value in the regions named.
	RegionValues map[string]float64 `json:"region_values,omitempty"`
	Adjustable   bool               `json:"adjustable"`
	// Source is the AWS documentation page the value was checked against.
	Source string `json:"source"`
}

// In returns the quota's value in region.
func (q Quota) In(region string) float64 {
	if v, ok := q.RegionValues[region]; ok {
		return v
	}
	return q.Value
}

const (
	srcSQSGeneral   = "https://docs.aws.amazon.com/general/latest/gr/sqs-service.html"
	srcSQSMessages  = "https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/quotas-messages.html"
	srcSNS          = "https://docs.aws.amazon.com/general/latest/gr/sns.html"
	srcEventBridge  = "https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-quota.html"
	srcKinesis      = "https://docs.aws.amazon.com/streams/latest/dev/service-sizes-and-limits.html"
	srcMSK          = "https://docs.aws.amazon.com/msk/latest/developerguide/limits.html"
	mib             = 1 << 20
	kib             = 1 << 10
	retention14Days = 14
)

func regions(v float64, names ...string) map[string]float64 {
	out := make(map[string]float64, len(names))
	for _, n := range names {
		out[n] = v
	}
	return out
}

func merge(ms ...map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

// documentedQuotas are the messaging quotas, checked against the pages in
// Source in November 2026.
func documentedQuotas() []Quota {
	return []Quota{
		{
			ID: "sqs-message-size", Service: "sqs", Description: "Maximum message size",
			Value: mib, Unit: UnitBytes, Per: "message", Source: srcSQSMessages,
		},
		{
			ID: "sqs-retention", Service: "sqs", Description: "Maximum message retention period",
			Value: retention14Days, Unit: UnitDays, Per: "queue", Source: srcSQSMessages,
		},
		{
			ID: "sqs-batch-entries", Service: "sqs", Description: "Messages per batch request",
			Value: 10, Unit: UnitEntriesPerReq, Per: "request", Source: srcSQSGeneral,
		},
		{
			ID: "sqs-fifo-unbatched-tps", Service: "sqs", Description: "Unbatched transactions per second for FIFO queues",
			Value: 300, Unit: UnitPerSecond, Per: "queue", Source: srcSQSGeneral,
		},
		{
			ID: "sqs-fifo-batched-tps", Service: "sqs", Description: "Batched transactions per second for FIFO queues (messages, in batches of up to 10)",
			Value: 3000, Unit: UnitPerSecond, Per: "queue", Source: srcSQSGeneral,
		},
		{
			ID: "sqs-fifo-high-throughput-batched-tps", Service: "sqs", Description: "Batched transactions per second for high-throughput FIFO queues",
			Value: 24000, Unit: UnitPerSecond, Per: "queue", Source: srcSQSGeneral,
			RegionValues: merge(
				regions(700000, "us-east-1", "us-west-2", "eu-west-1"),
				regions(180000, "us-east-2", "eu-central-1"),
				regions(90000, "ap-south-1", "ap-southeast-1", "ap-southeast-2", "ap-northeast-1"),
				regions(45000, "eu-west-2", "sa-east-1"),
			),
		},
		{
			ID: "sqs-in-flight", Service: "sqs", Description: "In-flight messages per queue",
			Value: 120000, Unit: UnitCount, Per: "queue", Adjustable: true, Source: srcSQSGeneral,
		},
		{
			ID: "sns-publish-rate", Service: "sns", Description: "Messages published per second (Publish and PublishBatch), standard topics",
			Value: 300, Unit: UnitPerSecond, Per: "account", Adjustable: true, Source: srcSNS,
			RegionValues: merge(
				regions(30000, "us-east-1"),
				regions(9000, "us-west-2", "eu-west-1"),
				regions(1500, "us-east-2", "us-west-1", "ap-south-1", "ap-northeast-2", "ap-southeast-1", "ap-southeast-2", "ap-northeast-1", "eu-central-1"),
			),
		},
		{
			ID: "sns-message-size", Service: "sns", Description: "Default maximum message size",
			Value: 256 * kib, Unit: UnitBytes, Per: "message", Source: srcSNS,
		},
		{
			ID: "eventbridge-putevents-rate", Service: "events", Description: "PutEvents requests per second",
			Value: 400, Unit: UnitPerSecond, Per: "account", Adjustable: true, Source: srcEventBridge,
			RegionValues: merge(
				regions(10000, "us-east-1", "us-west-2", "eu-west-1"),
				regions(2400, "us-east-2", "eu-central-1", "eu-south-2"),
				regions(1200, "us-west-1", "ap-northeast-1", "ap-southeast-1", "ap-southeast-2", "eu-west-2"),
				regions(600, "ap-east-1", "ap-northeast-2", "ap-south-1", "ca-central-1", "eu-north-1", "eu-west-3", "sa-east-1"),
			),
		},
		{
			ID: "eventbridge-targets-per-rule", Service: "events", Description: "Targets per rule",
			Value: 5, Unit: UnitCount, Per: "rule", Source: srcEventBridge,
		},
		{
			ID: "kinesis-shard-write-records", Service: "kinesis", Description: "Records written per second per shard",
			Value: 1000, Unit: UnitPerSecond, Per: "shard", Source: srcKinesis,
		},
		{
			ID: "kinesis-shard-write-bytes", Service: "kinesis", Description: "Bytes written per second per shard",
			Value: mib, Unit: UnitBytesPerSec, Per: "shard", Source: srcKinesis,
		},
		{
			ID: "kinesis-shard-read-bytes", Service: "kinesis", Description: "Bytes read per second per shard through GetRecords, shared by consumers",
			Value: 2 * mib, Unit: UnitBytesPerSec, Per: "shard", Source: srcKinesis,
		},
		{
			ID: "kinesis-record-size", Service: "kinesis", Description: "Maximum record size before base64 encoding",
			Value: 10 * mib, Unit: UnitBytes, Per: "record", Source: srcKinesis,
		},
		{
			ID: "kinesis-retention", Service: "kinesis", Description: "Maximum retention period",
			Value: 365, Unit: UnitDays, Per: "stream", Source: srcKinesis,
		},
		{
			ID: "msk-brokers-per-account", Service: "kafka", Description: "Brokers per account",
			Value: 90, Unit: UnitCount, Per: "account", Adjustable: true, Source: srcMSK,
		},
	}
}

// Documented returns AWS's documented default quotas that Stratus knows,
// sorted by ID.
func Documented() []Quota {
	qs := documentedQuotas()
	sort.Slice(qs, func(i, j int) bool { return qs[i].ID < qs[j].ID })
	return qs
}

// DocumentedQuota looks up one documented quota by ID.
func DocumentedQuota(id string) (Quota, bool) {
	for _, q := range documentedQuotas() {
		if q.ID == id {
			return q, true
		}
	}
	return Quota{}, false
}
