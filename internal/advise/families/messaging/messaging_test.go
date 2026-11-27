package messaging

import (
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/advise"
	"github.com/stratus/internal/limits"
)

// documented backs the family with the real quotas from internal/limits,
// so these scenarios break if a quota changes under them.
var documented = advise.QuotaFunc(func(id, region string) (advise.Quota, bool) {
	q, ok := limits.DocumentedQuota(id)
	return advise.Quota{ID: q.ID, Value: q.In(region), Source: q.Source}, ok
})

// listPrices is a fixed price book with us-east-1 list prices, so the
// cost tests pin the usage model rather than whatever AWS charges today.
// TestTheEmbeddedSnapshotPricesEveryMessagingItem in internal/app checks
// the real snapshot carries every one of these IDs.
var listPrices = advise.PriceFunc(func(id, region string) (advise.Price, bool) {
	flat := map[string]float64{
		priceSQSStandard: 0.40e-6, priceSQSFIFO: 0.50e-6, priceEventBridgeEvents: 1.00e-6,
		priceKinesisShardHours: 0.015, priceKinesisPayload: 0.014e-6, priceKinesisExtended: 0.020,
		priceKinesisLongTerm: 0.023, priceKinesisEFOHours: 0.015, priceKinesisEFOData: 0.013,
		priceMSKBrokerHours: 0.204, priceMSKStorage: 0.10,
	}
	if region != "us-east-1" {
		return advise.Price{}, false
	}
	if id == priceSNSRequests {
		return advise.Price{ID: id, Unit: "requests", AsOf: "2026-11-26", Tiers: []advise.PriceTier{{From: 0, To: 1e6, USD: 0}, {From: 1e6, USD: 0.50e-6}}}, true
	}
	usd, ok := flat[id]
	if !ok {
		return advise.Price{}, false
	}
	return advise.Price{ID: id, Unit: "units", AsOf: "2026-11-26", Tiers: []advise.PriceTier{{From: 0, USD: usd}}}, true
})

func assess(t *testing.T, answers map[string]any) advise.Decision {
	t.Helper()
	f := New(documented, listPrices)
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	d, err := advise.NewDecision("adr-test", "", f, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Answer(f, answers, now); err != nil {
		t.Fatal(err)
	}
	if err := d.Assess(f, now); err != nil {
		t.Fatal(err)
	}
	return d
}

func verdicts(d advise.Decision) map[string]advise.Verdict {
	out := map[string]advise.Verdict{}
	for _, a := range d.Assessments {
		out[a.Option] = a.Verdict
	}
	return out
}

func reasonsOf(d advise.Decision, option string) string {
	var b strings.Builder
	for _, a := range d.Assessments {
		if a.Option == option {
			for _, r := range a.Reasons {
				b.WriteString(string(r.Kind) + ": " + r.Text + "\n")
			}
		}
	}
	return b.String()
}

// The golden scenarios: a reviewer can read this table and disagree with
// a verdict without reading the rules.
func TestGoldenMessagingScenarios(t *testing.T) {
	cases := []struct {
		name    string
		answers map[string]any
		top     string
		want    map[string]advise.Verdict
	}{
		{
			name:    "a simple background job queue ranks a standard queue first",
			answers: map[string]any{"peak_rate": 50.0},
			top:     SQSStandard,
			want:    map[string]advise.Verdict{SQSStandard: advise.VerdictFits, MSK: advise.VerdictFitsCaveats, Kinesis: advise.VerdictFitsCaveats},
		},
		{
			name:    "needing replay rules out queues and SNS fan-out",
			answers: map[string]any{"replay": true, "consumers": 3.0},
			top:     Kinesis,
			want:    map[string]advise.Verdict{SQSStandard: advise.VerdictRuledOut, SQSFIFO: advise.VerdictRuledOut, SNSFanout: advise.VerdictRuledOut, EventBridge: advise.VerdictFitsCaveats},
		},
		{
			name:    "per-key order at 200 messages/s fits a FIFO queue without caveats",
			answers: map[string]any{"ordering": "per_key", "peak_rate": 200.0},
			top:     SQSFIFO,
			want:    map[string]advise.Verdict{SQSFIFO: advise.VerdictFits, SQSStandard: advise.VerdictRuledOut, SNSFanout: advise.VerdictRuledOut, EventBridge: advise.VerdictRuledOut},
		},
		{
			name:    "one global order at 5,000 messages/s rules out FIFO and Kinesis",
			answers: map[string]any{"ordering": "global", "peak_rate": 5000.0},
			top:     MSK,
			want:    map[string]advise.Verdict{SQSFIFO: advise.VerdictRuledOut, Kinesis: advise.VerdictRuledOut},
		},
		{
			name:    "per-key order at 5,000 messages/s needs FIFO high-throughput mode",
			answers: map[string]any{"ordering": "per_key", "peak_rate": 5000.0},
			want:    map[string]advise.Verdict{SQSFIFO: advise.VerdictFitsCaveats, Kinesis: advise.VerdictFits},
		},
		{
			name:    "the Kafka API rules out everything but MSK",
			answers: map[string]any{"kafka": true, "ops_capacity": "platform_team"},
			top:     MSK,
			want:    map[string]advise.Verdict{MSK: advise.VerdictFits, SQSStandard: advise.VerdictRuledOut, Kinesis: advise.VerdictRuledOut, EventBridge: advise.VerdictRuledOut},
		},
		{
			name:    "content routing to many consumers ranks EventBridge or SNS ahead of streams",
			answers: map[string]any{"routing": true, "consumers": 4.0},
			want:    map[string]advise.Verdict{EventBridge: advise.VerdictFits, SNSFanout: advise.VerdictFits, SQSStandard: advise.VerdictRuledOut},
		},
		{
			name:    "messages over 1 MiB rule out SQS and SNS but not Kinesis",
			answers: map[string]any{"message_size": 2048.0, "consumers": 2.0},
			want:    map[string]advise.Verdict{SQSStandard: advise.VerdictRuledOut, SNSFanout: advise.VerdictRuledOut, Kinesis: advise.VerdictFits},
		},
		{
			name:    "keeping messages 30 days rules out SQS",
			answers: map[string]any{"retention_days": 30.0},
			want:    map[string]advise.Verdict{SQSStandard: advise.VerdictRuledOut, SQSFIFO: advise.VerdictRuledOut},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := assess(t, tc.answers)
			got := verdicts(d)
			for option, want := range tc.want {
				if got[option] != want {
					t.Errorf("%s: verdict %s, want %s\n%s", option, got[option], want, reasonsOf(d, option))
				}
			}
			if tc.top != "" && d.Assessments[0].Option != tc.top {
				t.Errorf("ranked first: %s, want %s (ranking %v)", d.Assessments[0].Option, tc.top, got)
			}
		})
	}
}

func TestATeamWithNoOpsCapacityGetsACaveatOnMSKAndKinesis(t *testing.T) {
	d := assess(t, map[string]any{"ops_capacity": "none", "replay": true, "ordering": "per_key"})
	for _, option := range []string{MSK, Kinesis} {
		if !strings.Contains(reasonsOf(d, option), "Ops overhead") {
			t.Errorf("%s has no ops caveat:\n%s", option, reasonsOf(d, option))
		}
	}
	if strings.Contains(reasonsOf(d, SQSFIFO), "Ops overhead") {
		t.Errorf("FIFO should be within a no-ops team's budget")
	}
}

func TestSNSPublishQuotaDependsOnTheRegion(t *testing.T) {
	answers := map[string]any{"consumers": 2.0, "peak_rate": 5000.0}
	answers["region"] = "us-east-1"
	if r := reasonsOf(assess(t, answers), SNSFanout); strings.Contains(r, "request an increase") {
		t.Errorf("5,000/s is within us-east-1's 30,000:\n%s", r)
	}
	answers["region"] = "af-south-1"
	if r := reasonsOf(assess(t, answers), SNSFanout); !strings.Contains(r, "300 published messages/s in af-south-1") {
		t.Errorf("af-south-1's default is 300:\n%s", r)
	}
}

func TestKinesisShardCountComesFromRecordsOrBytesWhicheverIsLarger(t *testing.T) {
	w := workload{peak: 2500, sizeBytes: 1024}
	if got := shardsFor(w, 1000, 1<<20); got != 3 {
		t.Errorf("records-bound: %d shards, want 3", got)
	}
	w = workload{peak: 500, sizeBytes: 10 * 1024}
	if got := shardsFor(w, 1000, 1<<20); got != 5 {
		t.Errorf("bytes-bound: %d shards, want 5", got)
	}
	if got := shardsFor(workload{}, 1000, 1<<20); got != 1 {
		t.Errorf("idle: %d shards, want 1", got)
	}
}

func TestAMissingQuotaIsReportedAsNotCheckedInsteadOfPassing(t *testing.T) {
	f := New(nil, nil)
	now := time.Now()
	d, _ := advise.NewDecision("adr-x", "", f, now)
	if err := d.Assess(f, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reasonsOf(d, SQSStandard), "unchecked: Quota sqs-message-size is not modelled") {
		t.Errorf("want an unchecked reason:\n%s", reasonsOf(d, SQSStandard))
	}
}

func TestReasonsThatUseAQuotaCiteItsSource(t *testing.T) {
	d := assess(t, map[string]any{"ordering": "global", "peak_rate": 5000.0})
	for _, a := range d.Assessments {
		if a.Option != SQSFIFO {
			continue
		}
		for _, r := range a.Reasons {
			if r.Kind == advise.ReasonRuledOut && (r.Citation == nil || !strings.HasPrefix(r.Citation.URL, "https://docs.aws.amazon.com/")) {
				t.Errorf("uncited rule-out: %+v", r)
			}
		}
	}
}

func TestSizesPrintInMiBWhenWhole(t *testing.T) {
	for in, want := range map[float64]string{1 << 20: "1 MiB", 2 << 20: "2 MiB", 1536: "1.5 KiB", 256 << 10: "256 KiB"} {
		if got := kib(in); got != want {
			t.Errorf("kib(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestNumbersPrintWithThousandsSeparators(t *testing.T) {
	for in, want := range map[float64]string{300: "300", 3000: "3,000", 700000: "700,000", 2.5: "2.5"} {
		if got := num(in); got != want {
			t.Errorf("num(%v) = %q, want %q", in, got, want)
		}
	}
}
