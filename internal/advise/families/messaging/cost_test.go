package messaging

import (
	"math"
	"strings"
	"testing"

	"github.com/stratus/internal/advise"
)

const secondsInAMonth = 730 * 3600

func costOf(t *testing.T, answers map[string]any, option string) advise.Estimate {
	t.Helper()
	for _, a := range assess(t, answers).Assessments {
		if a.Option == option {
			if a.Cost == nil {
				t.Fatalf("%s has no cost estimate", option)
			}
			return *a.Cost
		}
	}
	t.Fatalf("no assessment for %s", option)
	return advise.Estimate{}
}

func dollars(t *testing.T, e advise.Estimate, want float64) {
	t.Helper()
	if math.Abs(e.MonthlyUSD-want) > 0.005 {
		var b strings.Builder
		for _, l := range e.Lines {
			b.WriteString(l.Item + "\n")
		}
		t.Errorf("%s: $%.4f a month, want $%.4f\n%s", e.Option, e.MonthlyUSD, want, b.String())
	}
}

func TestAStandardQueueCostsThreeRequestsPerMessage(t *testing.T) {
	e := costOf(t, map[string]any{"average_rate": 10.0}, SQSStandard)
	// 10/s for a month, send + receive + delete, at $0.40 a million.
	dollars(t, e, 10*secondsInAMonth*3*0.40e-6)
	if !e.Complete() || e.PricesAsOf != "2026-11-26" || len(e.Assumptions) == 0 {
		t.Errorf("estimate %+v should be complete, dated and state its assumptions", e)
	}
}

func TestMessagesOver64KiBAreBilledPerChunk(t *testing.T) {
	small := costOf(t, map[string]any{"average_rate": 10.0, "message_size": 64.0}, SQSFIFO)
	large := costOf(t, map[string]any{"average_rate": 10.0, "message_size": 100.0}, SQSFIFO)
	if math.Abs(large.MonthlyUSD-2*small.MonthlyUSD) > 0.005 {
		t.Errorf("100 KiB is two 64 KiB chunks: $%.2f vs $%.2f", large.MonthlyUSD, small.MonthlyUSD)
	}
}

func TestSNSFanoutPaysForPublishesAfterTheFirstMillionAndForEachConsumersQueue(t *testing.T) {
	e := costOf(t, map[string]any{"average_rate": 10.0, "consumers": 3.0}, SNSFanout)
	msgs := 10.0 * secondsInAMonth
	dollars(t, e, (msgs-1e6)*0.50e-6+msgs*3*3*0.40e-6)
}

func TestEventBridgeCostsOneDollarAMillionEvents(t *testing.T) {
	dollars(t, costOf(t, map[string]any{"average_rate": 10.0}, EventBridge), 10*secondsInAMonth*1e-6)
}

func TestKinesisPaysForShardsPayloadAndRetention(t *testing.T) {
	msgs := 10.0 * secondsInAMonth
	base := 730*0.015 + msgs*0.014e-6
	// One shard at the default 100/s peak; four days' retention pays the
	// extended rate for the shard.
	dollars(t, costOf(t, map[string]any{"average_rate": 10.0}, Kinesis), base+730*0.020)
	// One day's retention is included.
	dollars(t, costOf(t, map[string]any{"average_rate": 10.0, "retention_days": 1.0}, Kinesis), base)
	// Thirty days adds 23 days of data per GB-month.
	stored := 10.0 * 4096 * 23 * 86400 / (1 << 30)
	dollars(t, costOf(t, map[string]any{"average_rate": 10.0, "retention_days": 30.0}, Kinesis), base+730*0.020+stored*0.023)
}

func TestKinesisAddsEnhancedFanOutWhenSharedReadsWouldNotKeepUp(t *testing.T) {
	answers := map[string]any{"average_rate": 100.0, "peak_rate": 200.0, "message_size": 2.0, "consumers": 3.0}
	if e := costOf(t, answers, Kinesis); strings.Contains(strings.Join(e.Assumptions, " "), "enhanced fan-out") {
		t.Errorf("3 x 200 x 2 KiB/s fits one shard's 2 MiB/s of shared reads: %v", e.Assumptions)
	}
	answers["message_size"] = 1024.0 // 3 x 200 x 1 MiB/s needs fan-out
	e := costOf(t, answers, Kinesis)
	var efo bool
	for _, l := range e.Lines {
		efo = efo || l.PriceID == priceKinesisEFOHours
	}
	if !efo {
		t.Errorf("no enhanced fan-out line: %+v", e.Lines)
	}
}

func TestMSKCostsThreeBrokersAndReplicatedStorage(t *testing.T) {
	e := costOf(t, map[string]any{"average_rate": 10.0, "retention_days": 4.0}, MSK)
	stored := 10.0 * 4096 * 4 * 86400 * 3 / (1 << 30)
	dollars(t, e, 3*730*0.204+stored*0.10)
}

func TestAMissingPriceIsListedAndTheTotalIsALowerBound(t *testing.T) {
	e := costOf(t, map[string]any{"region": "eu-west-1"}, SQSStandard)
	if e.Complete() || e.MonthlyUSD != 0 || e.Unpriced[0] != priceSQSStandard {
		t.Errorf("eu-west-1 has no price in the fake book; got %+v", e)
	}
	f := New(documented, nil)
	for _, est := range f.Costs(advise.NewRequirements(f.Questions(), nil)) {
		if est.Complete() {
			t.Errorf("%s is complete with no price book", est.Option)
		}
	}
}
