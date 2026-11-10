// Package bedrock emulates the Bedrock Runtime API (InvokeModel, Converse,
// their streaming variants and CountTokens) without calling a model.
//
// Answers come from fixtures. A recorded fixture pins one exact request to
// the response real Bedrock gave it; a hand-written fixture answers every
// request whose prompt contains some text. In record mode a request with no
// fixture goes to real Bedrock once and the answer is saved, so the same
// test replays it offline from then on. Embedding models need no fixtures:
// they get deterministic vectors derived from the input text.
package bedrock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Kind is the request shape a fixture answers. The streaming operations
// share their non-streaming sibling's fixtures, so one recording serves
// both.
type Kind string

const (
	// KindInvoke covers InvokeModel and InvokeModelWithResponseStream, whose
	// bodies are in the model provider's own format.
	KindInvoke Kind = "invoke"
	// KindConverse covers Converse and ConverseStream.
	KindConverse Kind = "converse"
)

// Mode says what happens to a request no fixture answers.
type Mode string

const (
	// ModeReplay fails the request with a message naming its request hash.
	ModeReplay Mode = "replay"
	// ModeRecord sends it to real Bedrock and saves the answer as a fixture.
	ModeRecord Mode = "record"
)

// ParseMode accepts "replay" or "record"; empty means replay.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeReplay:
		return ModeReplay, nil
	case ModeRecord:
		return ModeRecord, nil
	}
	return "", fmt.Errorf("unknown Bedrock mode %q: use replay or record", s)
}

// Match selects requests for a hand-written fixture.
type Match struct {
	// Contains must appear in the request's prompt text: system prompt,
	// messages and tool results, flattened. Empty matches every prompt.
	Contains string `json:"contains,omitempty"`
}

// Fixture is one canned answer.
type Fixture struct {
	ID string `json:"id"`
	// Kind limits the fixture to invoke or converse requests; empty answers
	// both. It is required with Response, whose shape depends on it.
	Kind Kind `json:"kind,omitempty"`
	// Model is a model id, inference profile or ARN, or a pattern with *
	// wildcards. Empty matches every model.
	Model string `json:"model,omitempty"`
	// RequestHash pins the fixture to one exact request. Recorded fixtures
	// always carry it; a failed replay prints the hash to pin.
	RequestHash string `json:"request_hash,omitempty"`
	// Match selects requests by prompt text when RequestHash is empty.
	Match *Match `json:"match,omitempty"`
	// Text is the answer, rendered in the model's response format.
	Text string `json:"text,omitempty"`
	// Response is the exact response body to return: a Converse response
	// for kind converse, the provider's body for kind invoke.
	Response json.RawMessage `json:"response,omitempty"`
	// Request is the recorded request, kept so a fixture file can be read
	// and reviewed. It plays no part in matching.
	Request    json.RawMessage `json:"request,omitempty"`
	RecordedAt *time.Time      `json:"recorded_at,omitempty"`
	// Origin says where the fixture lives: "runtime" (control API) or the
	// fixture file it was read from. It is not stored.
	Origin string `json:"origin,omitempty"`
}

var fixtureIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Validate checks a fixture before it is stored.
func (f Fixture) Validate() error {
	if f.ID != "" && !fixtureIDRe.MatchString(f.ID) {
		return fmt.Errorf("id %q may only contain letters, digits, '.', '_' and '-'", f.ID)
	}
	switch f.Kind {
	case "", KindInvoke, KindConverse:
	default:
		return fmt.Errorf("kind %q must be invoke or converse", f.Kind)
	}
	hasText, hasResponse := f.Text != "", len(f.Response) > 0
	switch {
	case hasText == hasResponse:
		return fmt.Errorf("give exactly one of text or response")
	case hasResponse && f.Kind == "":
		return fmt.Errorf("a response needs a kind (invoke or converse): its shape depends on it")
	case hasResponse && !json.Valid(f.Response):
		return fmt.Errorf("response is not valid JSON")
	}
	if hasResponse && f.Kind == KindConverse {
		var probe struct {
			Output *struct {
				Message json.RawMessage `json:"message"`
			} `json:"output"`
		}
		if err := json.Unmarshal(f.Response, &probe); err != nil || probe.Output == nil || len(probe.Output.Message) == 0 {
			return fmt.Errorf("a converse response needs output.message, like the Converse API returns")
		}
	}
	if f.RequestHash != "" && !strings.HasPrefix(f.RequestHash, hashPrefix) {
		return fmt.Errorf("request_hash must start with %q", hashPrefix)
	}
	return nil
}

// Answers reports whether the fixture answers req.
func (f Fixture) Answers(req Request) bool {
	if f.Kind != "" && f.Kind != req.Kind {
		return false
	}
	if f.Model != "" && !wildcardMatch(f.Model, req.Model) && !wildcardMatch(f.Model, req.BaseModel) {
		return false
	}
	if f.RequestHash != "" {
		return f.RequestHash == req.Hash
	}
	return f.Match == nil || strings.Contains(req.Prompt, f.Match.Contains)
}

// Request is a model request reduced to what matching needs.
type Request struct {
	Kind Kind
	// Model is the id as the caller sent it: a model id, an inference
	// profile such as us.anthropic..., or an ARN.
	Model string
	// BaseModel is the foundation model id behind Model.
	BaseModel string
	Body      []byte
	// Hash identifies the exact request: kind, model and canonical body.
	Hash string
	// Prompt is the request's text, flattened, for matching and the log.
	Prompt string
}

const hashPrefix = "sha256:"

// NewRequest builds the matching view of a request.
func NewRequest(kind Kind, model string, body []byte) Request {
	req := Request{Kind: kind, Model: model, BaseModel: BaseModelID(model), Body: body}
	req.Hash = requestHash(kind, model, body)
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		if kind == KindConverse {
			req.Prompt = conversePrompt(doc)
		} else if fam := familyFor(req.BaseModel); fam != nil {
			req.Prompt = fam.prompt(doc)
		} else {
			req.Prompt = genericPrompt(doc)
		}
	}
	return req
}

// requestHash hashes the request with its JSON body in canonical form, so
// key order and whitespace do not change it.
func requestHash(kind Kind, model string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(string(kind) + "\n" + model + "\n"))
	h.Write(canonicalJSON(body))
	return hashPrefix + hex.EncodeToString(h.Sum(nil))
}

func canonicalJSON(body []byte) []byte {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return body
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// BaseModelID reduces an ARN or inference profile to the foundation model
// id: "arn:...:inference-profile/us.anthropic.claude-x" and
// "us.anthropic.claude-x" both become "anthropic.claude-x".
func BaseModelID(model string) string {
	if strings.HasPrefix(model, "arn:") {
		if i := strings.LastIndex(model, "/"); i >= 0 {
			model = model[i+1:]
		}
	}
	if prefix, rest, ok := strings.Cut(model, "."); ok {
		switch prefix {
		case "us", "eu", "apac", "us-gov", "global", "jp", "au", "ca":
			return rest
		}
	}
	return model
}

// wildcardMatch matches s against a pattern where * stands for any run of
// characters.
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// Invocation is one model call, as the control API reports it.
type Invocation struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Operation string    `json:"operation"`
	Model     string    `json:"model"`
	// Prompt is the request's flattened text.
	Prompt string `json:"prompt"`
	// Response is the answer's text, empty when the call failed.
	Response    string `json:"response,omitempty"`
	RequestHash string `json:"request_hash"`
	// Outcome is replayed, recorded, synthesized (embeddings), missed or
	// failed.
	Outcome      string `json:"outcome"`
	FixtureID    string `json:"fixture_id,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Invocation outcomes.
const (
	OutcomeReplayed    = "replayed"
	OutcomeRecorded    = "recorded"
	OutcomeSynthesized = "synthesized"
	OutcomeMissed      = "missed"
	OutcomeFailed      = "failed"
)

// InvocationFilter narrows the invocation log.
type InvocationFilter struct {
	// Model matches the model id as sent or its base id, with * wildcards.
	Model string
	// Contains must appear in the prompt.
	Contains string
	// Outcome keeps one outcome only.
	Outcome string
	// Limit keeps the newest n entries; 0 keeps all.
	Limit int
}

func (f InvocationFilter) keeps(inv Invocation) bool {
	if f.Model != "" && !wildcardMatch(f.Model, inv.Model) && !wildcardMatch(f.Model, BaseModelID(inv.Model)) {
		return false
	}
	if f.Outcome != "" && f.Outcome != inv.Outcome {
		return false
	}
	return strings.Contains(inv.Prompt, f.Contains)
}

// usage is a token count. Stratus estimates tokens as one per four bytes of
// text, which is stable and close enough for code that budgets tokens.
type usage struct {
	Input  int
	Output int
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func usageFor(prompt, answer string) usage {
	return usage{Input: estimateTokens(prompt), Output: estimateTokens(answer)}
}
