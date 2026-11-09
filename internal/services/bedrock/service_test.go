package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

const haiku = "us.anthropic.claude-3-5-haiku-20241022-v1:0"

type clock struct{ t time.Time }

func (c *clock) now() time.Time {
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

// fakeUpstream stands in for real Bedrock.
type fakeUpstream struct {
	mu    sync.Mutex
	calls []string
	reply func(kind Kind, model string, body []byte) ([]byte, error)
}

func (f *fakeUpstream) Call(_ context.Context, kind Kind, model string, body []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, string(kind)+" "+model)
	f.mu.Unlock()
	return f.reply(kind, model, body)
}

func (f *fakeUpstream) Describe() string { return "fake" }

func newTestService(t *testing.T, opts Options) *Service {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	opts.Metadata = db
	if opts.Now == nil {
		opts.Now = (&clock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}).now
	}
	return NewService(opts)
}

func requireCode(t *testing.T, err error, code string) *apierror.Error {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return apiErr
}

func mustAdd(t *testing.T, svc *Service, f Fixture) Fixture {
	t.Helper()
	added, err := svc.AddFixture(f)
	if err != nil {
		t.Fatalf("add fixture: %v", err)
	}
	return added
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTextFixtureAnswersConverseAndInvoke(t *testing.T) {
	svc := newTestService(t, Options{})
	f := mustAdd(t, svc, Fixture{Model: "anthropic.claude-*", Match: &Match{Contains: "refund"}, Text: longAnswer})
	if !strings.HasPrefix(f.ID, "fx-") || f.Origin != OriginRuntime {
		t.Fatalf("added fixture should get an id and runtime origin: %+v", f)
	}

	ans, err := svc.Respond(context.Background(), OpConverse, haiku, converseBody("Where is my refund?"))
	if err != nil {
		t.Fatal(err)
	}
	if converseAnswer(ans.Body) != longAnswer || ans.Outcome != OutcomeReplayed || ans.FixtureID != f.ID {
		t.Fatalf("converse answer: %+v %s", ans, ans.Body)
	}

	invokeBody := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":50,"messages":[{"role":"user","content":"refund status"}]}`)
	ans, err = svc.Respond(context.Background(), OpInvokeModel, haiku, invokeBody)
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, ans.Body)
	if doc["type"] != "message" || (anthropicFamily{}).answer(doc) != longAnswer || doc["model"] != "anthropic.claude-3-5-haiku-20241022-v1:0" {
		t.Fatalf("invoke should answer in the Anthropic Messages format: %s", ans.Body)
	}
}

func TestMissFailsWithTheRequestHashAndIsLogged(t *testing.T) {
	svc := newTestService(t, Options{})
	_, err := svc.Respond(context.Background(), OpConverse, haiku, converseBody("hello"))
	apiErr := requireCode(t, err, "ResourceNotFoundException")
	req := NewRequest(KindConverse, haiku, converseBody("hello"))
	if apiErr.StatusCode != http.StatusNotFound || !strings.Contains(apiErr.Message, req.Hash) ||
		!strings.Contains(apiErr.Message, "STRATUS_BEDROCK_MODE=record") || !strings.Contains(apiErr.Message, "/bedrock/fixtures") {
		t.Fatalf("a miss should say how to fix it: %s", apiErr.Message)
	}
	log, _ := svc.Invocations(InvocationFilter{})
	if len(log) != 1 || log[0].Outcome != OutcomeMissed || log[0].RequestHash != req.Hash || log[0].Prompt != "You are a support bot\nhello" {
		t.Fatalf("the miss should be logged with its prompt: %+v", log)
	}
}

func TestExactHashBeatsPromptMatchAndRuntimeBeatsFiles(t *testing.T) {
	dir := t.TempDir()
	svc := newTestService(t, Options{FixtureDir: dir})
	req := NewRequest(KindConverse, haiku, converseBody("Where is my refund?"))

	writeFixtureFile(t, dir, "a-general.json", `{"text":"from the file"}`)
	mustAdd(t, svc, Fixture{Match: &Match{Contains: "refund"}, Text: "from runtime"})
	answer := func() string {
		t.Helper()
		ans, err := svc.Respond(context.Background(), OpConverse, haiku, req.Body)
		if err != nil {
			t.Fatal(err)
		}
		return ans.Text
	}
	if got := answer(); got != "from runtime" {
		t.Fatalf("runtime fixtures come before files: %q", got)
	}
	writeFixtureFile(t, dir, "b-pinned.json", `{"kind":"converse","request_hash":"`+req.Hash+`","text":"pinned"}`)
	if got := answer(); got != "pinned" {
		t.Fatalf("an exact hash match wins over prompt matches: %q", got)
	}
}

func TestFixtureFilesHoldOneOrManyFixtures(t *testing.T) {
	dir := t.TempDir()
	svc := newTestService(t, Options{FixtureDir: dir})
	writeFixtureFile(t, dir, "support/refunds.json", `[{"match":{"contains":"refund"},"text":"five days"},{"id":"late","match":{"contains":"late"},"text":"sorry"}]`)
	writeFixtureFile(t, dir, "chat.json", `{"model":"amazon.nova-*","text":"hi"}`)

	fixtures, err := svc.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fixtures {
		got = append(got, f.ID+"@"+f.Origin)
	}
	want := "chat@chat.json,refunds-1@support/refunds.json,late@support/refunds.json"
	if strings.Join(got, ",") != want {
		t.Fatalf("fixtures in path order with derived ids\n got %s\nwant %s", strings.Join(got, ","), want)
	}

	writeFixtureFile(t, dir, "broken.json", `{"kind":"chat","text":"x"}`)
	if _, err := svc.Fixtures(); err == nil || !strings.Contains(err.Error(), "broken.json") {
		t.Fatalf("an invalid file should fail loudly and name itself: %v", err)
	}
}

func TestRecordModeSavesToTheFixtureDirectoryAndReplaysOffline(t *testing.T) {
	dir := t.TempDir()
	upstream := &fakeUpstream{reply: func(kind Kind, model string, body []byte) ([]byte, error) {
		return renderConverse("Sunny, 21 degrees.", usage{3, 5})
	}}
	svc := newTestService(t, Options{FixtureDir: dir, Mode: ModeRecord, Upstream: upstream})
	body := converseBody("weather?")

	ans, err := svc.Respond(context.Background(), OpConverse, "amazon.nova-lite-v1:0", body)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Outcome != OutcomeRecorded || ans.Text != "Sunny, 21 degrees." || len(upstream.calls) != 1 {
		t.Fatalf("record: %+v calls=%v", ans, upstream.calls)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 || !strings.HasPrefix(filepath.Base(files[0]), "converse-amazon.nova-lite-v1_0-") {
		t.Fatalf("one fixture file named after kind and model: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	var saved Fixture
	_ = json.Unmarshal(raw, &saved)
	req := NewRequest(KindConverse, "amazon.nova-lite-v1:0", body)
	if saved.RequestHash != req.Hash || saved.RecordedAt == nil || len(saved.Request) == 0 || saved.Origin != "" {
		t.Fatalf("recorded fixture should pin the request and keep it for review: %s", raw)
	}

	// The same request streams from the recording without calling upstream.
	ans, err = svc.Respond(context.Background(), OpConverseStream, "amazon.nova-lite-v1:0", body)
	if err != nil || ans.Outcome != OutcomeReplayed || len(upstream.calls) != 1 {
		t.Fatalf("replay should not call upstream again: %+v %v calls=%v", ans, err, upstream.calls)
	}

	// A fresh service in replay mode (CI) answers from the file alone.
	offline := newTestService(t, Options{FixtureDir: dir})
	if ans, err := offline.Respond(context.Background(), OpConverse, "amazon.nova-lite-v1:0", body); err != nil || ans.Text != "Sunny, 21 degrees." {
		t.Fatalf("offline replay: %v %+v", err, ans)
	}
}

func TestRecordModeWithoutDirectoryKeepsRecordingsInTheStore(t *testing.T) {
	upstream := &fakeUpstream{reply: func(Kind, string, []byte) ([]byte, error) {
		return []byte(`{"generation":"hi there","prompt_token_count":1,"generation_token_count":2,"stop_reason":"stop"}`), nil
	}}
	svc := newTestService(t, Options{Upstream: upstream})
	if err := svc.SetMode(ModeRecord); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"prompt":"hello","max_gen_len":10}`)
	ans, err := svc.Respond(context.Background(), OpInvokeModel, "meta.llama3-1-8b-instruct-v1:0", body)
	if err != nil || ans.Text != "hi there" {
		t.Fatalf("record invoke: %v %+v", err, ans)
	}
	fixtures, _ := svc.Fixtures()
	if len(fixtures) != 1 || fixtures[0].Origin != OriginRuntime || fixtures[0].Kind != KindInvoke {
		t.Fatalf("recording without a directory is a runtime fixture: %+v", fixtures)
	}
	if err := svc.SetMode(ModeReplay); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Respond(context.Background(), OpInvokeModelWithResponseStream, "meta.llama3-1-8b-instruct-v1:0", body); err != nil || len(upstream.calls) != 1 {
		t.Fatalf("streaming replays the recording: %v calls=%v", err, upstream.calls)
	}
}

func TestRecordModePassesUpstreamErrorsThrough(t *testing.T) {
	upstream := &fakeUpstream{reply: func(Kind, string, []byte) ([]byte, error) {
		return nil, &apierror.Error{StatusCode: http.StatusForbidden, Code: "AccessDeniedException", Message: "real Bedrock (record mode): no model access"}
	}}
	dir := t.TempDir()
	svc := newTestService(t, Options{Mode: ModeRecord, Upstream: upstream, FixtureDir: dir})
	_, err := svc.Respond(context.Background(), OpConverse, haiku, converseBody("hi"))
	requireCode(t, err, "AccessDeniedException")
	if files, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(files) != 0 {
		t.Fatalf("a failed call records nothing: %v", files)
	}
	log, _ := svc.Invocations(InvocationFilter{})
	if len(log) != 1 || log[0].Outcome != OutcomeFailed || !strings.Contains(log[0].Error, "no model access") {
		t.Fatalf("the failure is logged: %+v", log)
	}
}

func TestResponseFixtureIsReturnedVerbatim(t *testing.T) {
	svc := newTestService(t, Options{})
	toolCall := `{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"t1","name":"lookup","input":{"order":42}}}]}},"stopReason":"tool_use","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2},"metrics":{"latencyMs":5}}`
	mustAdd(t, svc, Fixture{Kind: KindConverse, Response: json.RawMessage(toolCall)})
	ans, err := svc.Respond(context.Background(), OpConverse, haiku, converseBody("order 42?"))
	if err != nil || string(ans.Body) != toolCall {
		t.Fatalf("response fixture: %v %s", err, ans.Body)
	}
}

func TestTextFixtureForUnknownProviderAsksForAResponse(t *testing.T) {
	svc := newTestService(t, Options{})
	f := mustAdd(t, svc, Fixture{Text: "hi"})
	_, err := svc.Respond(context.Background(), OpInvokeModel, "cohere.command-r-v1:0", []byte(`{"message":"hello"}`))
	apiErr := requireCode(t, err, "ValidationException")
	if !strings.Contains(apiErr.Message, f.ID) || !strings.Contains(apiErr.Message, `"response"`) {
		t.Fatalf("message should name the fixture and the fix: %s", apiErr.Message)
	}
	// Converse works for the same model: its format does not depend on it.
	if _, err := svc.Respond(context.Background(), OpConverse, "cohere.command-r-v1:0", converseBody("hello")); err != nil {
		t.Fatalf("converse text fixture for any model: %v", err)
	}
}

func TestEmbeddingsNeedNoFixtures(t *testing.T) {
	svc := newTestService(t, Options{})
	ans, err := svc.Respond(context.Background(), OpInvokeModel, "amazon.titan-embed-text-v2:0", []byte(`{"inputText":"hello"}`))
	if err != nil || ans.Outcome != OutcomeSynthesized || !ans.embedding {
		t.Fatalf("embedding: %v %+v", err, ans)
	}
	_, err = svc.Respond(context.Background(), OpInvokeModelWithResponseStream, "amazon.titan-embed-text-v2:0", []byte(`{"inputText":"hello"}`))
	requireCode(t, err, "ValidationException")
}

func TestConverseRequestsAreValidated(t *testing.T) {
	svc := newTestService(t, Options{})
	mustAdd(t, svc, Fixture{Text: "hi"})
	_, err := svc.Respond(context.Background(), OpConverse, haiku, []byte(`{`))
	requireCode(t, err, "ValidationException")
	_, err = svc.Respond(context.Background(), OpConverse, haiku, []byte(`{"messages":[]}`))
	requireCode(t, err, "ValidationException")
}

func TestModeDefaultsAndOverrides(t *testing.T) {
	svc := newTestService(t, Options{Mode: ModeRecord, FixtureDir: "/fx", Upstream: &fakeUpstream{}})
	if st := svc.Status(); st.Mode != ModeRecord || st.DefaultMode != ModeRecord || st.FixtureDir != "/fx" || st.Upstream != "fake" {
		t.Fatalf("status: %+v", st)
	}
	if err := svc.SetMode(ModeReplay); err != nil {
		t.Fatal(err)
	}
	if svc.Mode() != ModeReplay {
		t.Fatal("the control API's mode wins until reset")
	}
	if NewService(Options{Metadata: svc.metadata}).defaultMode != ModeReplay {
		t.Fatal("the default mode is replay")
	}
}

func TestFixtureLifecycle(t *testing.T) {
	dir := t.TempDir()
	svc := newTestService(t, Options{FixtureDir: dir})
	writeFixtureFile(t, dir, "file.json", `{"text":"x"}`)
	mustAdd(t, svc, Fixture{ID: "mine", Text: "a"})
	mustAdd(t, svc, Fixture{Text: "b"})

	if _, err := svc.AddFixture(Fixture{ID: "mine", Text: "again"}); err == nil {
		t.Fatal("duplicate id")
	} else {
		requireCode(t, err, "ConflictException")
	}
	_, err := svc.AddFixture(Fixture{})
	requireCode(t, err, "ValidationException")

	requireCode(t, svc.DeleteFixture("file"), "ConflictException")
	requireCode(t, svc.DeleteFixture("nope"), "ResourceNotFoundException")
	if err := svc.DeleteFixture("mine"); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ClearFixtures(); err != nil || n != 1 {
		t.Fatalf("clear removes the one runtime fixture left: %d %v", n, err)
	}
	fixtures, _ := svc.Fixtures()
	if len(fixtures) != 1 || fixtures[0].ID != "file" {
		t.Fatalf("files stay after clear: %+v", fixtures)
	}
}

func TestInvocationLogFiltersAndClears(t *testing.T) {
	svc := newTestService(t, Options{})
	mustAdd(t, svc, Fixture{Text: "ok"})
	for _, prompt := range []string{"first refund", "second", "third refund"} {
		if _, err := svc.Respond(context.Background(), OpConverse, haiku, converseBody(prompt)); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := svc.Invocations(InvocationFilter{})
	if len(all) != 3 || !strings.HasSuffix(all[0].Prompt, "first refund") || all[0].Response != "ok" || all[0].OutputTokens != 1 {
		t.Fatalf("log oldest first with answer and tokens: %+v", all)
	}
	latest, _ := svc.Invocations(InvocationFilter{Contains: "refund", Limit: 1})
	if len(latest) != 1 || !strings.HasSuffix(latest[0].Prompt, "third refund") {
		t.Fatalf("limit keeps the newest: %+v", latest)
	}
	if n, err := svc.ClearInvocations(); err != nil || n != 3 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if left, _ := svc.Invocations(InvocationFilter{}); len(left) != 0 || left == nil {
		t.Fatalf("an empty log is an empty list: %#v", left)
	}
}

func TestCountTokens(t *testing.T) {
	svc := newTestService(t, Options{})
	n, err := svc.CountTokens(haiku, []byte(`{"input":{"converse":{"messages":[{"role":"user","content":[{"text":"abcdefgh"}]}]}}}`))
	if err != nil || n != 2 {
		t.Fatalf("converse tokens: %d %v", n, err)
	}
	n, err = svc.CountTokens("meta.llama3-1-8b-instruct-v1:0", []byte(`{"input":{"invokeModel":{"body":"eyJwcm9tcHQiOiJhYmNkZSJ9"}}}`)) // {"prompt":"abcde"}
	if err != nil || n != 2 {
		t.Fatalf("invoke tokens: %d %v", n, err)
	}
	for _, bad := range []string{`{`, `{"input":{}}`, `{"input":{"invokeModel":{"body":"%%%"}}}`} {
		if _, err := svc.CountTokens(haiku, []byte(bad)); err == nil {
			t.Errorf("CountTokens(%s) should fail", bad)
		}
	}
}
