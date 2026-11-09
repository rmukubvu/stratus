package bedrock

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeReplay, "replay": ModeReplay, " RECORD ": ModeRecord} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("live"); err == nil || !strings.Contains(err.Error(), "replay or record") {
		t.Fatalf("an unknown mode should name the valid ones: %v", err)
	}
}

func TestFixtureValidate(t *testing.T) {
	converse := json.RawMessage(`{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn"}`)
	cases := []struct {
		name    string
		fixture Fixture
		wantErr string
	}{
		{"text only", Fixture{Text: "hi"}, ""},
		{"converse response", Fixture{Kind: KindConverse, Response: converse}, ""},
		{"invoke response", Fixture{Kind: KindInvoke, Response: json.RawMessage(`{"generation":"hi"}`)}, ""},
		{"neither", Fixture{Model: "x"}, "exactly one of text or response"},
		{"both", Fixture{Text: "hi", Kind: KindConverse, Response: converse}, "exactly one of text or response"},
		{"response without kind", Fixture{Response: converse}, "needs a kind"},
		{"bad kind", Fixture{Kind: "chat", Text: "hi"}, "invoke or converse"},
		{"bad json", Fixture{Kind: KindInvoke, Response: json.RawMessage(`{`)}, "not valid JSON"},
		{"converse without output", Fixture{Kind: KindConverse, Response: json.RawMessage(`{"text":"hi"}`)}, "output.message"},
		{"bad id", Fixture{ID: "a b", Text: "hi"}, "may only contain"},
		{"bad hash", Fixture{RequestHash: "md5:x", Text: "hi"}, "sha256:"},
	}
	for _, tc := range cases {
		err := tc.fixture.Validate()
		if tc.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: error %v should contain %q", tc.name, err, tc.wantErr)
		}
	}
}

func converseBody(text string) []byte {
	return []byte(`{"system":[{"text":"You are a support bot"}],"messages":[{"role":"user","content":[{"text":"` + text + `"}]}]}`)
}

func TestFixtureAnswers(t *testing.T) {
	req := NewRequest(KindConverse, "us.anthropic.claude-3-5-haiku-20241022-v1:0", converseBody("Where is my refund?"))
	cases := []struct {
		name    string
		fixture Fixture
		want    bool
	}{
		{"catch-all", Fixture{Text: "x"}, true},
		{"kind mismatch", Fixture{Kind: KindInvoke, Text: "x"}, false},
		{"exact model as sent", Fixture{Model: "us.anthropic.claude-3-5-haiku-20241022-v1:0", Text: "x"}, true},
		{"pattern on base model", Fixture{Model: "anthropic.claude-*", Text: "x"}, true},
		{"other model", Fixture{Model: "amazon.nova-*", Text: "x"}, false},
		{"prompt contains", Fixture{Match: &Match{Contains: "refund"}, Text: "x"}, true},
		{"system prompt counts", Fixture{Match: &Match{Contains: "support bot"}, Text: "x"}, true},
		{"prompt lacks", Fixture{Match: &Match{Contains: "invoice"}, Text: "x"}, false},
		{"hash pins", Fixture{RequestHash: req.Hash, Match: &Match{Contains: "invoice"}, Text: "x"}, true},
		{"other hash", Fixture{RequestHash: "sha256:00", Text: "x"}, false},
	}
	for _, tc := range cases {
		if got := tc.fixture.Answers(req); got != tc.want {
			t.Errorf("%s: Answers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRequestHashIgnoresKeyOrderAndWhitespace(t *testing.T) {
	a := NewRequest(KindConverse, "m", []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}],"inferenceConfig":{"maxTokens":10}}`))
	b := NewRequest(KindConverse, "m", []byte("{\n \"inferenceConfig\": {\"maxTokens\": 10},\n \"messages\": [{\"content\": [{\"text\": \"hi\"}], \"role\": \"user\"}]\n}"))
	if a.Hash != b.Hash || !strings.HasPrefix(a.Hash, "sha256:") {
		t.Fatalf("equivalent bodies should hash alike: %s vs %s", a.Hash, b.Hash)
	}
	for _, other := range []Request{
		NewRequest(KindInvoke, "m", a.Body),
		NewRequest(KindConverse, "m2", a.Body),
		NewRequest(KindConverse, "m", []byte(`{"messages":[{"role":"user","content":[{"text":"hi!"}]}],"inferenceConfig":{"maxTokens":10}}`)),
	} {
		if other.Hash == a.Hash {
			t.Fatalf("kind, model and body all take part in the hash")
		}
	}
}

func TestBaseModelID(t *testing.T) {
	cases := map[string]string{
		"anthropic.claude-3-haiku-20240307-v1:0":                                                     "anthropic.claude-3-haiku-20240307-v1:0",
		"us.anthropic.claude-3-5-haiku-20241022-v1:0":                                                "anthropic.claude-3-5-haiku-20241022-v1:0",
		"global.anthropic.claude-sonnet-4-20250514-v1:0":                                             "anthropic.claude-sonnet-4-20250514-v1:0",
		"arn:aws:bedrock:us-east-1::foundation-model/amazon.nova-lite-v1:0":                          "amazon.nova-lite-v1:0",
		"arn:aws:bedrock:us-east-1:123456789012:inference-profile/eu.meta.llama3-2-1b-instruct-v1:0": "meta.llama3-2-1b-instruct-v1:0",
	}
	for in, want := range cases {
		if got := BaseModelID(in); got != want {
			t.Errorf("BaseModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"a", "a", true},
		{"a", "ab", false},
		{"*", "anything", true},
		{"anthropic.*", "anthropic.claude", true},
		{"*haiku*", "anthropic.claude-3-haiku-v1:0", true},
		{"a*c*e", "abcde", true},
		{"a*c*e", "abcdf", false},
		{"*-v1:0", "amazon.nova-lite-v1:0", true},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q) = %v", tc.pattern, tc.s, got)
		}
	}
}

func TestInvocationFilterKeeps(t *testing.T) {
	inv := Invocation{Model: "us.anthropic.claude-3-5-haiku-20241022-v1:0", Prompt: "Where is my refund?", Outcome: OutcomeReplayed}
	if !(InvocationFilter{Model: "anthropic.*", Contains: "refund", Outcome: OutcomeReplayed}).keeps(inv) {
		t.Fatal("matching filter should keep the invocation")
	}
	for _, f := range []InvocationFilter{{Model: "amazon.*"}, {Contains: "invoice"}, {Outcome: OutcomeMissed}} {
		if f.keeps(inv) {
			t.Fatalf("filter %+v should drop the invocation", f)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if estimateTokens("") != 0 || estimateTokens("a") != 1 || estimateTokens("abcd") != 1 || estimateTokens("abcde") != 2 {
		t.Fatal("tokens are estimated as one per four bytes, rounded up")
	}
}
