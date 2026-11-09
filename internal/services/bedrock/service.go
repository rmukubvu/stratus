package bedrock

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/store"
)

const (
	invocationsBucket = "bedrock-invocations"
	settingsBucket    = "bedrock-settings"
	modeKey           = "mode"

	maxBodyBytes = 25 << 20
)

// Operation names, as AWS SDKs and the request log use them.
const (
	OpInvokeModel                   = "InvokeModel"
	OpInvokeModelWithResponseStream = "InvokeModelWithResponseStream"
	OpConverse                      = "Converse"
	OpConverseStream                = "ConverseStream"
	OpCountTokens                   = "CountTokens"
)

// Options wires the service to its adapters.
type Options struct {
	Metadata store.Store
	// FixtureDir holds fixture files to replay and receives recordings.
	// Empty keeps recordings in the metadata store instead.
	FixtureDir string
	// Mode applies until the control API sets another one.
	Mode Mode
	// Upstream is real Bedrock, used in record mode.
	Upstream Upstream
	Now      func() time.Time
}

// Service answers Bedrock Runtime requests from fixtures. It is protocol
// agnostic; http.go adapts it to the REST and event-stream wire formats.
type Service struct {
	metadata    store.Store
	runtime     runtimeFixtures
	dir         fixtureDir
	defaultMode Mode
	upstream    Upstream
	now         func() time.Time

	mu  sync.Mutex
	seq uint64
}

// NewService builds a Bedrock service.
func NewService(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	mode := opts.Mode
	if mode == "" {
		mode = ModeReplay
	}
	return &Service{
		metadata:    opts.Metadata,
		runtime:     runtimeFixtures{metadata: opts.Metadata},
		dir:         fixtureDir{path: opts.FixtureDir},
		defaultMode: mode,
		upstream:    opts.Upstream,
		now:         now,
	}
}

// Answer is a model response plus what streaming and the log need.
type Answer struct {
	// Body is the complete, non-streaming response body.
	Body []byte
	// Text is the answer's text, when the format is known.
	Text      string
	Outcome   string
	FixtureID string
	usage     usage
	family    family
	request   map[string]any
	embedding bool
}

// Respond answers one InvokeModel, InvokeModelWithResponseStream, Converse
// or ConverseStream request and records it in the invocation log.
func (s *Service) Respond(ctx context.Context, operation, model string, body []byte) (Answer, error) {
	kind := KindInvoke
	if operation == OpConverse || operation == OpConverseStream {
		kind = KindConverse
	}
	req := NewRequest(kind, model, body)
	ans, err := s.answer(ctx, req, operation)
	s.log(operation, req, ans, err)
	return ans, err
}

func (s *Service) answer(ctx context.Context, req Request, operation string) (Answer, error) {
	var doc map[string]any
	if err := json.Unmarshal(req.Body, &doc); err != nil && req.Kind == KindConverse {
		return Answer{}, validation("the request body is not valid JSON")
	}
	if req.Kind == KindConverse && len(list(doc["messages"])) == 0 {
		return Answer{}, validation("messages must contain at least one message")
	}
	streaming := operation == OpInvokeModelWithResponseStream || operation == OpConverseStream

	fixture, found, err := s.lookup(req)
	if err != nil {
		return Answer{}, err
	}
	if found {
		return s.fromFixture(req, doc, fixture)
	}
	if req.Kind == KindInvoke {
		if emb := embedderFor(req.BaseModel); emb != nil {
			if streaming {
				return Answer{}, validation("model " + req.Model + " does not support streaming")
			}
			body, tokens, err := emb.embed(doc, req.Model)
			if err != nil {
				return Answer{}, err
			}
			return Answer{Body: body, Outcome: OutcomeSynthesized, usage: usage{Input: tokens}, embedding: true}, nil
		}
	}
	if s.Mode() == ModeRecord {
		return s.record(ctx, req, doc)
	}
	return Answer{}, s.missError(req)
}

// lookup finds the fixture for req: an exact request-hash match first,
// then the first fixture whose model and prompt match. Runtime fixtures come
// before fixture files, so a test can override a file for itself.
func (s *Service) lookup(req Request) (Fixture, bool, error) {
	fixtures, err := s.Fixtures()
	if err != nil {
		return Fixture{}, false, err
	}
	for _, f := range fixtures {
		if f.RequestHash != "" && f.Answers(req) {
			return f, true, nil
		}
	}
	for _, f := range fixtures {
		if f.RequestHash == "" && f.Answers(req) {
			return f, true, nil
		}
	}
	return Fixture{}, false, nil
}

func (s *Service) fromFixture(req Request, doc map[string]any, f Fixture) (Answer, error) {
	ans := Answer{Outcome: OutcomeReplayed, FixtureID: f.ID, request: doc}
	if req.Kind == KindInvoke {
		ans.family = familyFor(req.BaseModel)
	}
	if len(f.Response) > 0 {
		ans.Body = f.Response
		ans.Text = answerText(req.Kind, ans.family, f.Response)
		ans.usage = usageFor(req.Prompt, ans.Text)
		return ans, nil
	}
	ans.Text = f.Text
	ans.usage = usageFor(req.Prompt, f.Text)
	var err error
	if req.Kind == KindConverse {
		ans.Body, err = renderConverse(f.Text, ans.usage)
		return ans, err
	}
	if ans.family == nil {
		return Answer{}, validation(fmt.Sprintf("fixture %s answers with text, but Stratus does not know the response format of %s: "+
			"give the exact response body instead (\"kind\": \"invoke\" with \"response\")", f.ID, req.Model))
	}
	ans.Body, err = ans.family.render(doc, f.Text, req.BaseModel, ans.usage)
	return ans, err
}

func answerText(kind Kind, fam family, body []byte) string {
	if kind == KindConverse {
		return converseAnswer(body)
	}
	var resp map[string]any
	if fam == nil || json.Unmarshal(body, &resp) != nil {
		return ""
	}
	return fam.answer(resp)
}

// record asks real Bedrock and saves the answer as a fixture pinned to this
// exact request.
func (s *Service) record(ctx context.Context, req Request, doc map[string]any) (Answer, error) {
	if s.upstream == nil {
		return Answer{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: "record mode has no upstream Bedrock configured"}
	}
	body, err := s.upstream.Call(ctx, req.Kind, req.Model, req.Body)
	if err != nil {
		return Answer{}, err
	}
	if !json.Valid(body) {
		return Answer{}, internal("real Bedrock answered with a body that is not JSON; Stratus can only record JSON responses")
	}
	recordedAt := s.now().UTC()
	f := Fixture{Kind: req.Kind, Model: req.Model, RequestHash: req.Hash, Response: body, RecordedAt: &recordedAt}
	if json.Valid(req.Body) {
		f.Request = canonicalJSON(req.Body)
	}
	f.ID = recordedID(f)
	if s.dir.path != "" {
		if _, err := s.dir.save(f); err != nil {
			return Answer{}, internal("save recorded fixture: " + err.Error())
		}
	} else if err := s.runtime.add(f, recordedAt); err != nil {
		var conflict *conflictError
		if !errors.As(err, &conflict) {
			return Answer{}, internal("save recorded fixture: " + err.Error())
		}
	}
	ans := Answer{Body: body, Outcome: OutcomeRecorded, FixtureID: f.ID, request: doc}
	if req.Kind == KindInvoke {
		ans.family = familyFor(req.BaseModel)
	}
	ans.Text = answerText(req.Kind, ans.family, body)
	ans.usage = usageFor(req.Prompt, ans.Text)
	return ans, nil
}

func (s *Service) missError(req Request) error {
	operation := "an InvokeModel"
	if req.Kind == KindConverse {
		operation = "a Converse"
	}
	return &apierror.Error{
		StatusCode: http.StatusNotFound,
		Code:       "ResourceNotFoundException",
		Message: fmt.Sprintf("Stratus has no Bedrock fixture for %s request to %s (request_hash %s). "+
			"Record one by running with STRATUS_BEDROCK_MODE=record, or add one: "+
			"POST %s/bedrock/fixtures {\"model\":%q,\"text\":\"...\"}",
			operation, req.Model, req.Hash, control.PathPrefix, req.Model),
	}
}

// CountTokens estimates the input tokens of an InvokeModel body or a
// Converse request, the way CountTokens is asked.
func (s *Service) CountTokens(model string, body []byte) (int, error) {
	var in struct {
		Input struct {
			InvokeModel *struct {
				Body string `json:"body"`
			} `json:"invokeModel"`
			Converse map[string]any `json:"converse"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return 0, validation("the request body is not valid JSON")
	}
	switch {
	case in.Input.InvokeModel != nil:
		raw, err := base64.StdEncoding.DecodeString(in.Input.InvokeModel.Body)
		if err != nil {
			return 0, validation("input.invokeModel.body must be base64")
		}
		return estimateTokens(NewRequest(KindInvoke, model, raw).Prompt), nil
	case in.Input.Converse != nil:
		return estimateTokens(conversePrompt(in.Input.Converse)), nil
	}
	return 0, validation("input must contain invokeModel or converse")
}

// --- mode ---

// Mode is the current mode: the one set through the control API, else the
// configured default.
func (s *Service) Mode() Mode {
	raw, err := s.metadata.Get(settingsBucket, modeKey)
	if err != nil || raw == nil {
		return s.defaultMode
	}
	if mode, err := ParseMode(string(raw)); err == nil {
		return mode
	}
	return s.defaultMode
}

// SetMode switches between replay and record until the next reset.
func (s *Service) SetMode(mode Mode) error {
	return s.metadata.Put(settingsBucket, modeKey, []byte(mode))
}

// Status describes where fixtures come from and go to.
type Status struct {
	Mode        Mode   `json:"mode"`
	DefaultMode Mode   `json:"default_mode"`
	FixtureDir  string `json:"fixture_dir,omitempty"`
	Upstream    string `json:"upstream,omitempty"`
}

// Status reports the current configuration.
func (s *Service) Status() Status {
	st := Status{Mode: s.Mode(), DefaultMode: s.defaultMode, FixtureDir: s.dir.path}
	if s.upstream != nil {
		st.Upstream = s.upstream.Describe()
	}
	return st
}

// --- fixtures ---

// Fixtures lists every fixture in match order: runtime fixtures first, then
// fixture files in path order.
func (s *Service) Fixtures() ([]Fixture, error) {
	runtime, err := s.runtime.list()
	if err != nil {
		return nil, internal("list fixtures: " + err.Error())
	}
	files, err := s.dir.list()
	if err != nil {
		return nil, internal(err.Error())
	}
	return append(runtime, files...), nil
}

// AddFixture stores a runtime fixture and returns it with its id.
func (s *Service) AddFixture(f Fixture) (Fixture, error) {
	if err := f.Validate(); err != nil {
		return Fixture{}, validation(err.Error())
	}
	if f.ID == "" {
		f.ID = "fx-" + randomHex(4)
	}
	f.Origin = ""
	if err := s.runtime.add(f, s.now()); err != nil {
		var conflict *conflictError
		if errors.As(err, &conflict) {
			return Fixture{}, &apierror.Error{StatusCode: http.StatusConflict, Code: "ConflictException", Message: err.Error()}
		}
		return Fixture{}, internal(err.Error())
	}
	f.Origin = OriginRuntime
	return f, nil
}

// DeleteFixture removes a runtime fixture. Fixtures in files are changed by
// editing the file.
func (s *Service) DeleteFixture(id string) error {
	deleted, err := s.runtime.delete(id)
	if err != nil {
		return internal(err.Error())
	}
	if deleted {
		return nil
	}
	files, err := s.dir.list()
	if err != nil {
		return internal(err.Error())
	}
	for _, f := range files {
		if f.ID == id {
			return &apierror.Error{StatusCode: http.StatusConflict, Code: "ConflictException",
				Message: fmt.Sprintf("fixture %s comes from the file %s; edit or delete that file instead", id, f.Origin)}
		}
	}
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "no fixture with id " + id}
}

// ClearFixtures removes every runtime fixture and returns how many there
// were. Fixture files are left alone.
func (s *Service) ClearFixtures() (int, error) {
	n, err := s.runtime.clear()
	if err != nil {
		return 0, internal(err.Error())
	}
	return n, nil
}

// --- invocation log ---

func (s *Service) log(operation string, req Request, ans Answer, callErr error) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	now := s.now().UTC()
	inv := Invocation{
		ID:           fmt.Sprintf("%020d-%06d", now.UnixNano(), seq),
		Time:         now,
		Operation:    operation,
		Model:        req.Model,
		Prompt:       req.Prompt,
		Response:     ans.Text,
		RequestHash:  req.Hash,
		Outcome:      ans.Outcome,
		FixtureID:    ans.FixtureID,
		InputTokens:  ans.usage.Input,
		OutputTokens: ans.usage.Output,
	}
	if callErr != nil {
		inv.Outcome = OutcomeFailed
		var apiErr *apierror.Error
		if errors.As(callErr, &apiErr) && apiErr.Code == "ResourceNotFoundException" {
			inv.Outcome = OutcomeMissed
		}
		inv.Error = callErr.Error()
	}
	raw, err := json.Marshal(inv)
	if err != nil {
		return
	}
	_ = s.metadata.Put(invocationsBucket, inv.ID, raw)
}

// Invocations returns logged calls, oldest first.
func (s *Service) Invocations(filter InvocationFilter) ([]Invocation, error) {
	var out []Invocation
	err := s.metadata.Scan(invocationsBucket, "", func(_, v []byte) error {
		var inv Invocation
		if err := json.Unmarshal(v, &inv); err != nil {
			return err
		}
		if filter.keeps(inv) {
			out = append(out, inv)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err.Error())
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[len(out)-filter.Limit:]
	}
	if out == nil {
		out = []Invocation{}
	}
	return out, nil
}

// ClearInvocations empties the log and returns how many entries it held.
func (s *Service) ClearInvocations() (int, error) {
	all, err := s.Invocations(InvocationFilter{})
	if err != nil {
		return 0, err
	}
	if err := s.metadata.DeletePrefix(invocationsBucket, ""); err != nil {
		return 0, internal(err.Error())
	}
	return len(all), nil
}

// --- errors ---

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: message}
}

func internal(message string) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerException", Message: message}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
