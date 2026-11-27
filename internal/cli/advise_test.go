package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func (f *fakeControl) AdviseFamilies(context.Context) (json.RawMessage, error) {
	return f.record("advise-families")
}
func (f *fakeControl) AdviseStart(_ context.Context, in json.RawMessage) (json.RawMessage, error) {
	return f.record("advise-start " + string(in))
}
func (f *fakeControl) AdviseList(context.Context) (json.RawMessage, error) {
	return f.record("advise-list")
}
func (f *fakeControl) AdviseGet(_ context.Context, id string) (json.RawMessage, error) {
	return f.record("advise-get " + id)
}
func (f *fakeControl) AdviseAnswer(_ context.Context, id string, answers map[string]any) (json.RawMessage, error) {
	keys := make([]string, 0, len(answers))
	for k, v := range answers {
		keys = append(keys, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(keys)
	return f.record("advise-answer " + id + " " + strings.Join(keys, ","))
}
func (f *fakeControl) AdviseAssess(_ context.Context, id string) (json.RawMessage, error) {
	return f.record("advise-assess " + id)
}
func (f *fakeControl) AdviseDecide(_ context.Context, id, option, why string) (json.RawMessage, error) {
	return f.record("advise-decide " + id + " " + option + " " + why)
}
func (f *fakeControl) AdviseADR(_ context.Context, id string) (string, error) {
	_, err := f.record("advise-adr " + id)
	return "# ADR " + id + "\n", err
}

func TestAdviseCommandsMapToClientCalls(t *testing.T) {
	cases := []struct {
		args []string
		call string
	}{
		{[]string{"advise", "families"}, "advise-families"},
		{[]string{"advise", "list"}, "advise-list"},
		{[]string{"advise", "show", "adr-1"}, "advise-get adr-1"},
		{[]string{"advise", "assess", "adr-1"}, "advise-assess adr-1"},
		{[]string{"advise", "start", "messaging"}, `advise-start {"family":"messaging"}`},
		{[]string{"advise", "start", "messaging", "--title", "Order events", "replay=true"}, `advise-start {"answers":{"replay":"true"},"family":"messaging","title":"Order events"}`},
		{[]string{"advise", "answer", "adr-1", "ordering=per_key", "peak_rate=2000"}, "advise-answer adr-1 ordering=per_key,peak_rate=2000"},
		{[]string{"advise", "decide", "adr-1", "kinesis", "--why", "we replay"}, "advise-decide adr-1 kinesis we replay"},
		{[]string{"advise", "decide", "--why", "simple", "adr-1", "sqs-standard"}, "advise-decide adr-1 sqs-standard simple"},
	}
	for _, tc := range cases {
		fake := &fakeControl{}
		code, _, stderr := run(t, fake, tc.args...)
		if code != 0 || len(fake.calls) != 1 || fake.calls[0] != tc.call {
			t.Errorf("%v: code %d calls %q stderr %s; want %q", tc.args, code, fake.calls, stderr, tc.call)
		}
	}
}

func TestAdviseRejectsMalformedCommandsWithUsage(t *testing.T) {
	for _, args := range [][]string{
		{"advise"},
		{"advise", "nope"},
		{"advise", "answer", "adr-1"},
		{"advise", "answer", "adr-1", "no-equals"},
		{"advise", "decide", "adr-1"},
		{"advise", "show"},
	} {
		fake := &fakeControl{}
		if code, _, stderr := run(t, fake, args...); code != 2 || !(strings.Contains(stderr, "usage:") || strings.Contains(stderr, "not id=value")) || len(fake.calls) != 0 {
			t.Errorf("%v: code %d calls %v stderr %q", args, code, fake.calls, stderr)
		}
	}
}

func TestAdviseADRPrintsMarkdownOrWritesAFile(t *testing.T) {
	fake := &fakeControl{}
	code, stdout, _ := run(t, fake, "advise", "adr", "adr-1")
	if code != 0 || stdout != "# ADR adr-1\n" {
		t.Fatalf("code %d stdout %q", code, stdout)
	}
	out := filepath.Join(t.TempDir(), "0001-order-events.md")
	code, stdout, stderr := run(t, fake, "advise", "adr", "adr-1", "--out", out)
	raw, _ := os.ReadFile(out)
	if code != 0 || stdout != "" || string(raw) != "# ADR adr-1\n" || !strings.Contains(stderr, "wrote") {
		t.Fatalf("code %d stdout %q file %q stderr %q", code, stdout, raw, stderr)
	}
}

func TestAdviseAskAsksPendingQuestionsKeepsDefaultsForBlankLinesAndPrintsTheRanking(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{
		"advise-start": `{"id":"adr-1","title":"Order events","pending_questions":[
			{"id":"ordering","prompt":"Order?","why":"because","kind":"choice","choices":[{"value":"none","label":"No"}],"default":"none"},
			{"id":"peak_rate","prompt":"Peak?","why":"quotas","kind":"quantity","unit":"messages/s","default":100},
			{"id":"replay","prompt":"Replay?","why":"streams","kind":"bool","default":false}]}`,
		"advise-assess": `{"id":"adr-1","title":"Order events","recommended":"kinesis","assessments":[
			{"option":"kinesis","verdict":"fits","score":100,"reasons":[{"kind":"strength","text":"replays"}]},
			{"option":"sqs-standard","verdict":"ruled_out","score":0,"reasons":[{"kind":"ruled_out","text":"cannot replay"}]}]}`,
	}}
	adviseInput = strings.NewReader("\n2500\ntrue\n")
	t.Cleanup(func() { adviseInput = os.Stdin })
	code, stdout, stderr := run(t, fake, "advise", "ask", "messaging", "--title", "Order events")
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	if fake.calls[1] != "advise-answer adr-1 peak_rate=2500,replay=true" {
		t.Errorf("answers sent: %q", fake.calls)
	}
	for _, want := range []string{"Peak?", "[100 messages/s] >", "- none: No", "1. kinesis", "ruled out: cannot replay", "stratus advise decide adr-1 kinesis"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
}
