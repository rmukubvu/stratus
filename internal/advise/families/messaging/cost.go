package messaging

import (
	"math"

	"github.com/stratus/internal/advise"
)

// The usage model behind each estimate. Costs are monthly, at the stated
// average rate, from on-demand list prices, before the free tier.
const (
	hoursPerMonth   = 730
	secondsPerHour  = 3600
	secondsPerMonth = hoursPerMonth * secondsPerHour
	secondsPerDay   = 86400
	bytesPerGB      = 1 << 30 // AWS bills data in binary gigabytes

	// SQS, SNS and EventBridge bill each 64 KiB chunk of a payload as one
	// request or event; Kinesis bills PUTs in 25 KiB payload units.
	billingChunkBytes     = 64 * bytesPerKiB
	kinesisPayloadBytes   = 25 * bytesPerKiB
	sqsRequestsPerMessage = 3 // send, receive, delete

	kinesisIncludedRetentionDays = 1 // 24 hours come with a shard
	kinesisExtendedRetentionDays = 7 // beyond this, data is billed per GB-month

	mskBrokers     = 3 // one per Availability Zone, the usual minimum
	mskReplication = 3 // replication factor 3, so data is stored three times
)

// Price IDs from internal/pricing.
const (
	priceSQSStandard       = "sqs-standard-requests"
	priceSQSFIFO           = "sqs-fifo-requests"
	priceSNSRequests       = "sns-requests"
	priceEventBridgeEvents = "eventbridge-custom-events"
	priceKinesisShardHours = "kinesis-shard-hours"
	priceKinesisPayload    = "kinesis-put-payload-units"
	priceKinesisExtended   = "kinesis-extended-retention-shard-hours"
	priceKinesisLongTerm   = "kinesis-long-term-retention"
	priceKinesisEFOHours   = "kinesis-efo-consumer-shard-hours"
	priceKinesisEFOData    = "kinesis-efo-data-retrieved"
	priceMSKBrokerHours    = "msk-broker-hours-m7g-large"
	priceMSKStorage        = "msk-storage"
)

// PriceIDs are the prices the family's cost model reads, for tests that
// check the price snapshot covers them.
func PriceIDs() []string {
	return []string{
		priceSQSStandard, priceSQSFIFO, priceSNSRequests, priceEventBridgeEvents,
		priceKinesisShardHours, priceKinesisPayload, priceKinesisExtended, priceKinesisLongTerm,
		priceKinesisEFOHours, priceKinesisEFOData, priceMSKBrokerHours, priceMSKStorage,
	}
}

// Costs implements advise.Coster.
func (f Family) Costs(r advise.Requirements) []advise.Estimate {
	w := workloadFrom(r)
	q := quotaReader{src: f.quotas, region: w.region}
	m := monthOf(w)
	return []advise.Estimate{
		f.sqsCost(SQSStandard, priceSQSStandard, w, m),
		f.sqsCost(SQSFIFO, priceSQSFIFO, w, m),
		f.snsCost(w, m),
		f.eventBridgeCost(w, m),
		f.kinesisCost(w, q, m),
		f.mskCost(w),
	}
}

// month is a workload's monthly volume.
type month struct {
	messages float64
	chunks   float64 // 64 KiB billing chunks
}

func monthOf(w workload) month {
	msgs := w.average * secondsPerMonth
	return month{messages: msgs, chunks: msgs * chunksOf(w.sizeBytes, billingChunkBytes)}
}

// chunksOf is how many billing units of unit bytes one message of size
// bytes is: at least one.
func chunksOf(size, unit float64) float64 { return math.Max(1, math.Ceil(size/unit)) }

func (f Family) sqsCost(option, price string, w workload, m month) advise.Estimate {
	return advise.NewEstimate(f.prices, option, w.region).
		Add("Requests: send, receive and delete each message", price, m.chunks*sqsRequestsPerMessage).
		Assume("Each message is sent, received and deleted one at a time; batching up to 10 per call cuts requests by as much as 10x.").
		Build()
}

func (f Family) snsCost(w workload, m month) advise.Estimate {
	consumers := float64(max(1, w.consumers))
	return advise.NewEstimate(f.prices, SNSFanout, w.region).
		Add("SNS publishes", priceSNSRequests, m.chunks).
		Add("SQS requests: delivery, receive and delete in each consumer's queue", priceSQSStandard, m.chunks*consumers*sqsRequestsPerMessage).
		Assume("SNS charges nothing to deliver to SQS queues; each delivery is an SQS request.").
		Assume("One queue per consumer, read and deleted one message at a time.").
		Build()
}

func (f Family) eventBridgeCost(w workload, m month) advise.Estimate {
	return advise.NewEstimate(f.prices, EventBridge, w.region).
		Add("Custom events published", priceEventBridgeEvents, m.chunks).
		Assume("Rules delivering to targets in the same account cost nothing more; the targets themselves (an SQS queue, a Lambda function) are billed as usual and not included.").
		Build()
}

func (f Family) kinesisCost(w workload, q quotaReader, m month) advise.Estimate {
	b := advise.NewEstimate(f.prices, Kinesis, w.region)
	shards := 1
	records, ok1 := q.get("kinesis-shard-write-records")
	bytes, ok2 := q.get("kinesis-shard-write-bytes")
	if ok1 && ok2 {
		shards = shardsFor(w, records.Value, bytes.Value)
	} else {
		b.Assume("Shard quotas are not modelled, so one shard is assumed.")
	}
	shardHours := float64(shards) * hoursPerMonth
	b.Add("Provisioned shard-hours", priceKinesisShardHours, shardHours).
		Add("PUT payload units", priceKinesisPayload, m.messages*chunksOf(w.sizeBytes, kinesisPayloadBytes))
	if w.retentionDays > kinesisIncludedRetentionDays {
		b.Add("Retention beyond 24 hours", priceKinesisExtended, shardHours)
	}
	if extra := w.retentionDays - kinesisExtendedRetentionDays; extra > 0 {
		stored := w.average * w.sizeBytes * extra * secondsPerDay / bytesPerGB
		b.Add("Data kept beyond 7 days", priceKinesisLongTerm, stored)
	}
	if read, ok := q.get("kinesis-shard-read-bytes"); ok && w.consumers > 1 &&
		float64(w.consumers)*w.peak*w.sizeBytes > float64(shards)*read.Value {
		consumers := float64(w.consumers)
		b.Add("Enhanced fan-out consumer-shard-hours", priceKinesisEFOHours, consumers*shardHours).
			Add("Data read through enhanced fan-out", priceKinesisEFOData, consumers*m.messages*w.sizeBytes/bytesPerGB).
			Assume("Every consumer uses enhanced fan-out, because shared reads would exceed each shard's read quota.")
	}
	b.Assume("Provisioned mode, sized for the peak. On-demand mode bills per GB written and read instead and needs no shard planning.")
	return b.Build()
}

func (f Family) mskCost(w workload) advise.Estimate {
	stored := w.average * w.sizeBytes * w.retentionDays * secondsPerDay * mskReplication / bytesPerGB
	return advise.NewEstimate(f.prices, MSK, w.region).
		Add("Broker-hours, 3 x kafka.m7g.large", priceMSKBrokerHours, mskBrokers*hoursPerMonth).
		Add("Broker storage, replicated three times", priceMSKStorage, stored).
		Assume("Three kafka.m7g.large brokers, one per Availability Zone, with replication factor 3; larger peaks need larger or more brokers.").
		Assume("Storage is the data retained at the average rate; provisioned volumes are usually sized above it.").
		Build()
}
