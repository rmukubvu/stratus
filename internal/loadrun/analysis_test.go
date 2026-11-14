package loadrun

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// feed runs observations through a fresh tally, one second of the run per
// perSecond observations, and returns the findings at the end.
type feeder struct {
	t     *tally
	start time.Time
	n     int
}

func newFeeder() *feeder {
	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	return &feeder{t: newTally(start), start: start}
}

// add observes o at second sec of the run.
func (f *feeder) add(sec int, o Observation) {
	f.n++
	o.Time = f.start.Add(time.Duration(sec)*time.Second + time.Duration(f.n%1000)*time.Microsecond)
	if o.Status == 0 && o.Code == "" {
		o.Status = 200
	}
	f.t.add(o)
}

func (f *feeder) findings(seconds int) []Finding {
	return f.t.findings(f.start.Add(time.Duration(seconds) * time.Second))
}

func only(t *testing.T, fs []Finding, detector string) []Finding {
	t.Helper()
	var out []Finding
	for _, f := range fs {
		if f.Detector == detector {
			out = append(out, f)
		}
	}
	return out
}

func put(table, key string) Observation {
	return Observation{Service: "dynamodb", Operation: "PutItem", Keys: []Key{{table, key}}}
}

func throttled(o Observation) Observation {
	o.Status, o.Code = 400, "ProvisionedThroughputExceededException"
	return o
}

func TestHotPartitionNeedsAShareOfTrafficOrThrottles(t *testing.T) {
	f := newFeeder()
	for i := range 300 {
		o := put("orders", fmt.Sprintf(`S:"k%d"`, i))
		if i%2 == 0 {
			o = put("orders", `S:"customer#hot"`)
			if i%10 == 0 {
				o = throttled(o)
			}
		}
		f.add(i/30, o)
	}
	// An evenly spread table is not reported.
	for i := range 300 {
		f.add(i/30, put("events", fmt.Sprintf("N:%d", i)))
	}
	hot := only(t, f.findings(10), DetectorHotPartition)
	if len(hot) != 1 {
		t.Fatalf("findings %+v", hot)
	}
	h := hot[0]
	if h.Resource != "orders" || h.Severity != SeverityHigh || h.Evidence["partition_key"] != "customer#hot" || h.Evidence["key_throttled"] != 30 {
		t.Fatalf("finding %+v", h)
	}
	if !strings.Contains(h.Detail, "150 of 300") {
		t.Fatalf("detail %q", h.Detail)
	}
}

func TestHotPartitionTrackingStaysBounded(t *testing.T) {
	f := newFeeder()
	for i := range 20000 {
		f.add(i/1000, put("big", fmt.Sprintf(`S:"k%d"`, i)))
		if i%3 == 0 {
			f.add(i/1000, put("big", `S:"hot"`))
		}
	}
	h := f.t.detectors[0].(*hotPartitions)
	if n := len(h.tables["big"].keys); n > hotTrackedKeys {
		t.Fatalf("tracked %d keys", n)
	}
	hot := only(t, f.findings(20), DetectorHotPartition)
	if len(hot) != 1 || hot[0].Evidence["partition_key"] != "hot" || hot[0].Severity != SeverityMedium {
		t.Fatalf("findings %+v", hot)
	}
}

func TestChattyAndCacheableCalls(t *testing.T) {
	f := newFeeder()
	for i := range 600 {
		f.add(i/60, Observation{Service: "sqs", Operation: "SendMessage"})
		if i%6 == 0 {
			f.add(i/60, Observation{Service: "ssm", Operation: "GetParameter"})
		}
		if i%60 == 0 {
			f.add(i/60, Observation{Service: "dynamodb", Operation: "GetItem"})
		}
	}
	fs := f.findings(10)
	chatty := only(t, fs, DetectorChattyCalls)
	if len(chatty) != 1 || chatty[0].Operation != "SendMessage" || chatty[0].Evidence["batch_operation"] != "SendMessageBatch" || chatty[0].Severity != SeverityLow {
		t.Fatalf("chatty %+v", chatty)
	}
	cache := only(t, fs, DetectorCacheableReads)
	if len(cache) != 1 || cache[0].Operation != "GetParameter" || cache[0].Severity != SeverityMedium {
		t.Fatalf("cacheable %+v", cache)
	}

	// Using the batch API clears the chatty finding.
	for i := range 100 {
		f.add(i/10, Observation{Service: "sqs", Operation: "SendMessageBatch"})
	}
	if len(only(t, f.findings(10), DetectorChattyCalls)) != 0 {
		t.Fatal("chatty reported although the batch API is used")
	}
}

func sdk(id string, attempt, max int, o Observation) Observation {
	o.InvocationID, o.Attempt, o.MaxAttempts = id, attempt, max
	return o
}

func TestRetriesFollowSDKCallsAcrossAttempts(t *testing.T) {
	f := newFeeder()
	get := Observation{Service: "dynamodb", Operation: "GetItem"}
	for i := range 10 {
		// Retries off: one attempt, throttled.
		f.add(0, sdk(fmt.Sprintf("off-%d", i), 1, 1, throttled(get)))
		// Exhausted: three throttled attempts.
		for a := 1; a <= 3; a++ {
			f.add(0, sdk(fmt.Sprintf("ex-%d", i), a, 3, throttled(get)))
		}
		// Recovered: throttled, then OK.
		f.add(0, sdk(fmt.Sprintf("ok-%d", i), 1, 3, throttled(get)))
		f.add(0, sdk(fmt.Sprintf("ok-%d", i), 2, 3, get))
	}
	// Still retrying when the run ended: not reported.
	f.add(0, sdk("pending", 1, 3, throttled(get)))
	// A late earlier attempt does not undo a later success.
	f.add(0, sdk("late", 2, 3, get))
	f.add(0, sdk("late", 1, 3, throttled(get)))

	missing := only(t, f.findings(1), DetectorMissingRetries)
	if len(missing) != 2 {
		t.Fatalf("missing retries %+v", missing)
	}
	if missing[0].Evidence["failed_calls"] != 10 || missing[1].Evidence["failed_calls"] != 10 {
		t.Fatalf("counts %+v", missing)
	}
	r := f.t.detectors[3].(*retries)
	if r.failedNoRetries != 10 || r.failedExhausted != 10 {
		t.Fatalf("state %+v", r)
	}
}

func TestRetryStormAndUntrackedFailures(t *testing.T) {
	f := newFeeder()
	for i := range 100 {
		f.add(0, sdk(fmt.Sprintf("c%d", i), 1, 3, throttled(Observation{Service: "sqs", Operation: "SendMessage"})))
		f.add(0, sdk(fmt.Sprintf("c%d", i), 2, 3, Observation{Service: "sqs", Operation: "SendMessage"}))
	}
	for range 25 {
		f.add(0, Observation{Service: "s3", Operation: "GetObject", Status: 503, Code: "SlowDown"})
	}
	fs := f.findings(1)
	if storm := only(t, fs, DetectorRetryStorm); len(storm) != 1 || storm[0].Evidence["retries"] != 100 {
		t.Fatalf("storm %+v", storm)
	}
	if m := only(t, fs, DetectorMissingRetries); len(m) != 1 || m[0].Severity != SeverityLow || m[0].Evidence["failed_requests"] != 25 {
		t.Fatalf("untracked %+v", m)
	}
}

func TestMissingQueueNeedsLastingThrottlesAndNoQueue(t *testing.T) {
	f := newFeeder()
	for sec := range 10 {
		for i := range 20 {
			o := put("orders", fmt.Sprintf("N:%d", i))
			if i < 5 {
				o = throttled(o)
			}
			f.add(sec, o)
		}
	}
	// Async invokes are queued; sync throttles for only 2 seconds are a blip.
	for sec := range 2 {
		f.add(sec, Observation{Service: "lambda", Operation: "Invoke", Status: 429, Code: "TooManyRequestsException"})
	}
	q := only(t, f.findings(10), DetectorMissingQueue)
	if len(q) != 1 || q[0].Operation != "PutItem" || q[0].Evidence["throttled_seconds"] != 10 {
		t.Fatalf("missing queue %+v", q)
	}

	// Sending work through SQS clears it.
	for i := range 50 {
		f.add(i/5, Observation{Service: "sqs", Operation: "SendMessage"})
	}
	if q := only(t, f.findings(10), DetectorMissingQueue); len(q) != 0 {
		t.Fatalf("reported with a queue in use: %+v", q)
	}
}

func TestFindingsSortBySeverityAndSeverityHelpers(t *testing.T) {
	fs := []Finding{{Detector: "b", Severity: SeverityLow}, {Detector: "a", Severity: SeverityHigh}, {Detector: "c", Severity: SeverityMedium}}
	sortFindings(fs)
	if fs[0].Severity != SeverityHigh || fs[2].Severity != SeverityLow || Highest(fs) != SeverityHigh || Highest(nil) != "" {
		t.Fatalf("sorted %+v", fs)
	}
	if s, err := ParseSeverity(" Medium "); err != nil || s != SeverityMedium || !SeverityHigh.AtLeast(s) || SeverityLow.AtLeast(s) {
		t.Fatalf("parse: %v %v", s, err)
	}
	if _, err := ParseSeverity("urgent"); err == nil {
		t.Fatal("parsed urgent")
	}
}

func TestMarkdownReport(t *testing.T) {
	f := newFeeder()
	for i := range 200 {
		f.add(i/20, throttled(put("orders", `S:"hot"`)))
	}
	s := Summary{ID: "run-3", Name: "orders", Status: StatusFinished, DurationSeconds: 10, StartedAt: f.start,
		Totals: f.t.totals(f.start.Add(10 * time.Second)), Operations: f.t.operationStats(), Findings: f.findings(10)}
	md := Markdown(s)
	for _, want := range []string{"# Load run run-3 (orders)", "| 200 | 20.0 |", `### HIGH: Hot partition key "hot" on table orders`, "**What to do:**", "| dynamodb PutItem | 200 | 200 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if !strings.Contains(Markdown(Summary{ID: "run-4"}), "No problems spotted") {
		t.Error("empty report")
	}
}
