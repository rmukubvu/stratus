// Package messaging is the messaging and eventing decision family: SQS,
// SQS FIFO, SNS fan-out, EventBridge, Kinesis Data Streams and MSK.
package messaging

import (
	"github.com/stratus/internal/advise"
)

// Option IDs.
const (
	SQSStandard = "sqs-standard"
	SQSFIFO     = "sqs-fifo"
	SNSFanout   = "sns-sqs"
	EventBridge = "eventbridge"
	Kinesis     = "kinesis"
	MSK         = "msk"
)

// Question IDs.
const (
	qOrdering  = "ordering"
	qPeak      = "peak_rate"
	qAverage   = "average_rate"
	qSize      = "message_size"
	qConsumers = "consumers"
	qReplay    = "replay"
	qRetention = "retention_days"
	qDelivery  = "delivery"
	qRouting   = "routing"
	qKafka     = "kafka"
	qRegion    = "region"
)

// Answer values.
const (
	orderNone   = "none"
	orderPerKey = "per_key"
	orderGlobal = "global"
	atLeastOnce = "at_least_once"
	exactlyOnce = "exactly_once"
)

// Family is the messaging decision family.
type Family struct{ quotas advise.QuotaSource }

// New returns the family, reading AWS quotas from quotas.
func New(quotas advise.QuotaSource) Family { return Family{quotas: quotas} }

// ID implements advise.Family.
func (Family) ID() string { return "messaging" }

// Title implements advise.Family.
func (Family) Title() string { return "Messaging and eventing" }

// Summary implements advise.Family.
func (Family) Summary() string {
	return "How services hand work or events to each other: queues (SQS, SQS FIFO), fan-out (SNS to SQS), " +
		"event routing (EventBridge) and streams (Kinesis, MSK)."
}

// Questions implements advise.Family.
func (Family) Questions() []advise.Question {
	return []advise.Question{
		{
			ID: qOrdering, Kind: advise.KindChoice, Default: orderNone,
			Prompt: "Must consumers see messages in the order they were sent?",
			Why:    "Ordering rules out standard queues and pub/sub, and caps FIFO and stream throughput per key.",
			Choices: []advise.Choice{
				{Value: orderNone, Label: "No ordering needed"},
				{Value: orderPerKey, Label: "In order per key (per customer, per order)"},
				{Value: orderGlobal, Label: "One global order for everything"},
			},
		},
		{
			ID: qPeak, Kind: advise.KindQuantity, Unit: "messages/s", Min: 0, Max: 10_000_000, Default: 100.0,
			Prompt: "What is the peak rate, in messages per second?",
			Why:    "Peak rate is checked against each service's documented throughput quotas.",
		},
		{
			ID: qAverage, Kind: advise.KindQuantity, Unit: "messages/s", Min: 0, Max: 10_000_000, Default: 10.0,
			Prompt: "What is the average rate, in messages per second?",
			Why:    "Average rate drives the monthly cost.",
		},
		{
			ID: qSize, Kind: advise.KindQuantity, Unit: "KiB", Min: 0, Max: 1_048_576, Default: 4.0,
			Prompt: "How large is a typical message, in KiB?",
			Why:    "Each service has a maximum message size, and Kinesis shards are sized in bytes per second.",
		},
		{
			ID: qConsumers, Kind: advise.KindQuantity, Unit: "consumers", Min: 1, Max: 1000, Default: 1.0,
			Prompt: "How many independent consumers need every message?",
			Why:    "A queue hands each message to one consumer; several independent consumers need fan-out or a stream.",
		},
		{
			ID: qReplay, Kind: advise.KindBool, Default: false,
			Prompt: "Do consumers need to re-read past messages (replay)?",
			Why:    "Queues delete what is consumed; streams keep it for re-reading.",
		},
		{
			ID: qRetention, Kind: advise.KindQuantity, Unit: "days", Min: 0, Max: 3650, Default: 4.0,
			Prompt: "How long must unconsumed messages be kept, in days?",
			Why:    "SQS keeps messages for at most 14 days and Kinesis for 365.",
		},
		{
			ID: qDelivery, Kind: advise.KindChoice, Default: atLeastOnce,
			Prompt: "What delivery guarantee does processing need?",
			Why:    "Most services deliver at least once, so consumers must deduplicate unless the service does it.",
			Choices: []advise.Choice{
				{Value: atLeastOnce, Label: "At least once; consumers are idempotent"},
				{Value: exactlyOnce, Label: "Exactly-once processing"},
			},
		},
		{
			ID: qRouting, Kind: advise.KindBool, Default: false,
			Prompt: "Should messages be routed to different consumers by their content?",
			Why:    "EventBridge rules and SNS filter policies route by content; queues and streams don't.",
		},
		{
			ID: qKafka, Kind: advise.KindBool, Default: false,
			Prompt: "Do producers or consumers need the Kafka API?",
			Why:    "Only MSK speaks Kafka.",
		},
		{
			ID: qRegion, Kind: advise.KindText, Default: "us-east-1",
			Prompt: "Which AWS region will this run in?",
			Why:    "SNS, EventBridge and high-throughput FIFO quotas differ by region.",
		},
	}
}

// Options implements advise.Family.
func (Family) Options() []advise.Option {
	return []advise.Option{
		{ID: SQSStandard, Name: "SQS standard queue", Services: []string{"sqs"}, Ops: opsSQS,
			Summary: "A managed queue: each message goes to one consumer, at least once, in no guaranteed order."},
		{ID: SQSFIFO, Name: "SQS FIFO queue", Services: []string{"sqs"}, Ops: opsFIFO,
			Summary: "A queue that keeps order per message group and deduplicates, with a throughput quota."},
		{ID: SNSFanout, Name: "SNS topic fanning out to SQS queues", Services: []string{"sns", "sqs"}, Ops: opsSNS,
			Summary: "Publish once; every subscribed queue gets a copy, optionally filtered by content."},
		{ID: EventBridge, Name: "EventBridge event bus", Services: []string{"events"}, Ops: opsEventBridge,
			Summary: "An event router: rules match events by content and send them to targets."},
		{ID: Kinesis, Name: "Kinesis Data Streams", Services: []string{"kinesis"}, Ops: opsKinesis,
			Summary: "An ordered, replayable stream sharded by partition key, read by several consumers."},
		{ID: MSK, Name: "Amazon MSK (Kafka)", Services: []string{"kafka"}, Ops: opsMSK,
			Summary: "Managed Kafka brokers: the Kafka API, long retention and very high throughput, with a cluster to run."},
	}
}

// Assess implements advise.Family.
func (f Family) Assess(r advise.Requirements) []advise.Assessment {
	w := workloadFrom(r)
	q := quotaReader{src: f.quotas, region: w.region}
	return []advise.Assessment{
		advise.Judge(SQSStandard, sqsStandard(w, q)),
		advise.Judge(SQSFIFO, sqsFIFO(w, q)),
		advise.Judge(SNSFanout, snsFanout(w, q)),
		advise.Judge(EventBridge, eventBridge(w, q)),
		advise.Judge(Kinesis, kinesis(w, q)),
		advise.Judge(MSK, msk(w)),
	}
}

// workload is the requirements as the rules read them.
type workload struct {
	ordering      string
	peak, average float64
	sizeBytes     float64
	consumers     int
	replay        bool
	retentionDays float64
	exactlyOnce   bool
	routing       bool
	kafka         bool
	region        string
}

const bytesPerKiB = 1024

func workloadFrom(r advise.Requirements) workload {
	return workload{
		ordering:      r.Text(qOrdering),
		peak:          r.Number(qPeak),
		average:       r.Number(qAverage),
		sizeBytes:     r.Number(qSize) * bytesPerKiB,
		consumers:     int(r.Number(qConsumers)),
		replay:        r.Bool(qReplay),
		retentionDays: r.Number(qRetention),
		exactlyOnce:   r.Text(qDelivery) == exactlyOnce,
		routing:       r.Bool(qRouting),
		kafka:         r.Bool(qKafka),
		region:        r.Text(qRegion),
	}
}

// needsStream is true when something only a stream (or fan-out) gives is asked for.
func (w workload) needsStream() bool {
	return w.ordering != orderNone || w.replay || w.consumers > 1
}
