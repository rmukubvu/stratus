package bedrock

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/control"
)

// event is a decoded event-stream message.
type event struct {
	headers map[string]string
	payload []byte
}

// decodeEvents parses an AWS event stream and checks both CRCs of every
// message, as SDKs do.
func decodeEvents(t *testing.T, raw []byte) []event {
	t.Helper()
	var events []event
	for len(raw) > 0 {
		if len(raw) < 16 {
			t.Fatalf("truncated message: %d bytes left", len(raw))
		}
		total := int(binary.BigEndian.Uint32(raw[0:4]))
		headersLen := int(binary.BigEndian.Uint32(raw[4:8]))
		if crc32.ChecksumIEEE(raw[0:8]) != binary.BigEndian.Uint32(raw[8:12]) {
			t.Fatal("prelude CRC mismatch")
		}
		msg := raw[:total]
		if crc32.ChecksumIEEE(msg[:total-4]) != binary.BigEndian.Uint32(msg[total-4:]) {
			t.Fatal("message CRC mismatch")
		}
		headers := map[string]string{}
		h := msg[12 : 12+headersLen]
		for len(h) > 0 {
			nameLen := int(h[0])
			name := string(h[1 : 1+nameLen])
			if h[1+nameLen] != 7 {
				t.Fatalf("header %s is not a string", name)
			}
			valueLen := int(binary.BigEndian.Uint16(h[2+nameLen : 4+nameLen]))
			headers[name] = string(h[4+nameLen : 4+nameLen+valueLen])
			h = h[4+nameLen+valueLen:]
		}
		events = append(events, event{headers: headers, payload: msg[12+headersLen : total-4]})
		raw = raw[total:]
	}
	return events
}

func TestEncodeEventRoundTrips(t *testing.T) {
	frame := eventMessage("messageStart", []byte(`{"role":"assistant"}`))
	events := decodeEvents(t, append(frame, eventMessage("messageStop", []byte(`{}`))...))
	if len(events) != 2 || events[0].headers[":event-type"] != "messageStart" || events[0].headers[":message-type"] != "event" ||
		events[0].headers[":content-type"] != "application/json" || string(events[0].payload) != `{"role":"assistant"}` {
		t.Fatalf("decoded: %+v", events)
	}
}

func TestOperation(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"POST", "/model/anthropic.claude-3-haiku-20240307-v1%3A0/invoke", OpInvokeModel},
		{"POST", "/model/m/invoke-with-response-stream", OpInvokeModelWithResponseStream},
		{"POST", "/model/m/converse", OpConverse},
		{"POST", "/model/m/converse-stream", OpConverseStream},
		{"POST", "/model/m/count-tokens", OpCountTokens},
		{"POST", "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Ainference-profile%2Fus.amazon.nova-lite-v1%3A0/converse", OpConverse},
		{"GET", "/model/m/converse", ""},
		{"POST", "/model/m/chat", ""},
		{"POST", "/foundation-models", ""},
	}
	for _, tc := range cases {
		if got := Operation(tc.method, tc.path); got != tc.want {
			t.Errorf("Operation(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
	model, _, _ := splitModelPath("/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Ainference-profile%2Fus.amazon.nova-lite-v1%3A0/converse")
	if model != "arn:aws:bedrock:us-east-1:1:inference-profile/us.amazon.nova-lite-v1:0" {
		t.Fatalf("an ARN with an escaped '/' stays one model id: %q", model)
	}
}

func serve(t *testing.T, svc *Service, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if err := svc.Handle(rec, req); err != nil {
		t.Fatalf("handle %s: %v", path, err)
	}
	return rec
}

func TestHandleConverseAndConverseStream(t *testing.T) {
	svc := newTestService(t, Options{})
	mustAdd(t, svc, Fixture{Text: longAnswer})

	rec := serve(t, svc, "/model/amazon.nova-lite-v1%3A0/converse", string(converseBody("hi")))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" || converseAnswer(rec.Body.Bytes()) != longAnswer {
		t.Fatalf("converse: %d %s", rec.Code, rec.Body)
	}

	rec = serve(t, svc, "/model/amazon.nova-lite-v1%3A0/converse-stream", string(converseBody("hi")))
	if rec.Header().Get("Content-Type") != EventStreamContentType {
		t.Fatalf("stream content type: %q", rec.Header().Get("Content-Type"))
	}
	var types []string
	var text string
	for _, e := range decodeEvents(t, rec.Body.Bytes()) {
		types = append(types, e.headers[":event-type"])
		var p struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(e.payload, &p)
		text += p.Delta.Text
	}
	if types[0] != "messageStart" || types[len(types)-1] != "metadata" || text != longAnswer {
		t.Fatalf("stream events %v text %q", types, text)
	}
}

func TestHandleInvokeAndInvokeStream(t *testing.T) {
	svc := newTestService(t, Options{})
	mustAdd(t, svc, Fixture{Text: longAnswer})
	body := `{"anthropic_version":"bedrock-2023-05-31","max_tokens":50,"messages":[{"role":"user","content":"hello"}]}`

	rec := serve(t, svc, "/model/anthropic.claude-3-haiku-20240307-v1%3A0/invoke", body)
	if rec.Header().Get("X-Amzn-Bedrock-Output-Token-Count") != "25" || rec.Header().Get("X-Amzn-Bedrock-Input-Token-Count") != "2" {
		t.Fatalf("token headers: %v", rec.Header())
	}

	rec = serve(t, svc, "/model/anthropic.claude-3-haiku-20240307-v1%3A0/invoke-with-response-stream", body)
	if rec.Header().Get("X-Amzn-Bedrock-Content-Type") != "application/json" {
		t.Fatalf("stream should name the chunk content type: %v", rec.Header())
	}
	var text string
	for _, e := range decodeEvents(t, rec.Body.Bytes()) {
		if e.headers[":event-type"] != "chunk" {
			t.Fatalf("invoke streams only chunk events: %v", e.headers)
		}
		var part struct {
			Bytes string `json:"bytes"`
		}
		_ = json.Unmarshal(e.payload, &part)
		raw, _ := base64.StdEncoding.DecodeString(part.Bytes)
		var chunk map[string]any
		_ = json.Unmarshal(raw, &chunk)
		if chunk["type"] == "content_block_delta" {
			text += str(object(chunk["delta"])["text"])
		}
	}
	if text != longAnswer {
		t.Fatalf("streamed text %q", text)
	}
}

func TestHandleInvokeStreamForUnknownProviderSendsOneChunk(t *testing.T) {
	svc := newTestService(t, Options{})
	response := `{"text":"hi","finish_reason":"COMPLETE"}`
	mustAdd(t, svc, Fixture{Kind: KindInvoke, Model: "cohere.*", Response: json.RawMessage(response)})
	rec := serve(t, svc, "/model/cohere.command-r-v1%3A0/invoke-with-response-stream", `{"message":"hello"}`)
	events := decodeEvents(t, rec.Body.Bytes())
	var part struct {
		Bytes []byte `json:"bytes"`
	}
	if len(events) != 1 || json.Unmarshal(events[0].payload, &part) != nil || string(part.Bytes) != response {
		t.Fatalf("one chunk with the whole body: %d events", len(events))
	}
}

func TestHandleCountTokens(t *testing.T) {
	svc := newTestService(t, Options{})
	rec := serve(t, svc, "/model/m/count-tokens", `{"input":{"converse":{"messages":[{"role":"user","content":[{"text":"abcdefgh"}]}]}}}`)
	if strings.TrimSpace(rec.Body.String()) != `{"inputTokens":2}` {
		t.Fatalf("count tokens: %s", rec.Body)
	}
}

func TestHandleRejectsUnknownOperations(t *testing.T) {
	svc := newTestService(t, Options{})
	rec := httptest.NewRecorder()
	err := svc.Handle(rec, httptest.NewRequest(http.MethodGet, "/foundation-models", nil))
	requireCode(t, err, "UnknownOperationException")
	err = svc.Handle(rec, httptest.NewRequest(http.MethodPost, "/model/m/converse", bytes.NewReader([]byte(`{"messages":[{"role":"user","content":[{"text":"x"}]}]}`))))
	requireCode(t, err, "ResourceNotFoundException")
}

type muxRouter struct{ *http.ServeMux }

func (m muxRouter) HandleFunc(pattern string, fn http.HandlerFunc) {
	m.ServeMux.HandleFunc(pattern, fn)
}

func TestControlRoutes(t *testing.T) {
	dir := t.TempDir()
	svc := newTestService(t, Options{FixtureDir: dir})
	writeFixtureFile(t, dir, "file.json", `{"model":"amazon.nova-*","text":"from file"}`)
	mux := http.NewServeMux()
	RegisterRoutes(muxRouter{mux}, svc)
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, control.PathPrefix+path, strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, want, rec.Body)
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	if st := call("GET", "/bedrock", "", 200); st["mode"] != "replay" || st["fixture_dir"] != dir {
		t.Fatalf("status: %v", st)
	}
	if st := call("PUT", "/bedrock/mode", `{"mode":"record"}`, 200); st["mode"] != "record" {
		t.Fatalf("set mode: %v", st)
	}
	call("PUT", "/bedrock/mode", `{"mode":"live"}`, 400)
	call("PUT", "/bedrock/mode", `{}`, 400)
	call("PUT", "/bedrock/mode", `{"mode":"replay"}`, 200)

	added := call("POST", "/bedrock/fixtures", `{"id":"refunds","model":"anthropic.*","match":{"contains":"refund"},"text":"five days"}`, 201)
	if added["id"] != "refunds" || added["origin"] != OriginRuntime {
		t.Fatalf("added: %v", added)
	}
	call("POST", "/bedrock/fixtures", `{"id":"refunds","text":"again"}`, 409)
	call("POST", "/bedrock/fixtures", `{"text":"x","colour":"red"}`, 400)
	call("POST", "/bedrock/fixtures", `{"kind":"converse","response":{"text":"no output"}}`, 400)

	list := call("GET", "/bedrock/fixtures", "", 200)
	if list["count"] != float64(2) {
		t.Fatalf("runtime and file fixtures: %v", list)
	}
	call("DELETE", "/bedrock/fixtures/file", "", 409)
	call("DELETE", "/bedrock/fixtures/nope", "", 404)
	call("DELETE", "/bedrock/fixtures/refunds", "", 200)

	if _, err := svc.Respond(t.Context(), OpConverse, "amazon.nova-lite-v1:0", converseBody("hi")); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.Respond(t.Context(), OpConverse, haiku, converseBody("unknown"))
	log := call("GET", "/bedrock/invocations?outcome=missed", "", 200)
	items := log["items"].([]any)
	if log["count"] != float64(1) || object(items[0])["model"] != haiku {
		t.Fatalf("missed calls: %v", log)
	}
	if log := call("GET", "/bedrock/invocations?model=amazon.*&contains=hi&limit=5", "", 200); log["count"] != float64(1) {
		t.Fatalf("filtered log: %v", log)
	}
	call("GET", "/bedrock/invocations?limit=-1", "", 400)
	if cleared := call("DELETE", "/bedrock/invocations", "", 200); cleared["cleared"] != float64(2) {
		t.Fatalf("cleared: %v", cleared)
	}
	call("POST", "/bedrock/fixtures", `{"text":"a"}`, 201)
	if cleared := call("DELETE", "/bedrock/fixtures", "", 200); cleared["cleared"] != float64(1) {
		t.Fatalf("cleared fixtures: %v", cleared)
	}
}
