package bedrock

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return doc
}

const longAnswer = "Refunds take five business days to reach your card. We will email you as soon as it is processed."

// TestFamiliesRoundTrip checks every provider format: the prompt is read
// from a real request body, a rendered response yields the same text back,
// and the stream chunks carry the whole answer.
func TestFamiliesRoundTrip(t *testing.T) {
	cases := []struct {
		model      string
		request    string
		wantPrompt string
		streamText func(chunks []map[string]any) string
	}{
		{
			model:      "anthropic.claude-3-5-haiku-20241022-v1:0",
			request:    `{"anthropic_version":"bedrock-2023-05-31","max_tokens":100,"system":"Be brief","messages":[{"role":"user","content":"Where is my refund?"},{"role":"user","content":[{"type":"text","text":"Order 42"},{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"status: shipped"}]}]}]}`,
			wantPrompt: "Be brief\nWhere is my refund?\nOrder 42\nstatus: shipped",
			streamText: func(chunks []map[string]any) string {
				var b strings.Builder
				for _, c := range chunks {
					if c["type"] == "content_block_delta" {
						b.WriteString(str(object(c["delta"])["text"]))
					}
				}
				return b.String()
			},
		},
		{
			model:      "amazon.nova-lite-v1:0",
			request:    `{"system":[{"text":"Be brief"}],"messages":[{"role":"user","content":[{"text":"Where is my refund?"}]}],"inferenceConfig":{"maxTokens":100}}`,
			wantPrompt: "Be brief\nWhere is my refund?",
			streamText: func(chunks []map[string]any) string {
				var b strings.Builder
				for _, c := range chunks {
					b.WriteString(str(object(object(c["contentBlockDelta"])["delta"])["text"]))
				}
				return b.String()
			},
		},
		{
			model:      "amazon.titan-text-express-v1",
			request:    `{"inputText":"Where is my refund?","textGenerationConfig":{"maxTokenCount":100}}`,
			wantPrompt: "Where is my refund?",
			streamText: joinField("outputText"),
		},
		{
			model:      "meta.llama3-1-8b-instruct-v1:0",
			request:    `{"prompt":"Where is my refund?","max_gen_len":100}`,
			wantPrompt: "Where is my refund?",
			streamText: joinField("generation"),
		},
		{
			model:      "mistral.mistral-7b-instruct-v0:2",
			request:    `{"prompt":"<s>[INST] Where is my refund? [/INST]","max_tokens":100}`,
			wantPrompt: "<s>[INST] Where is my refund? [/INST]",
			streamText: func(chunks []map[string]any) string {
				var b strings.Builder
				for _, c := range chunks {
					b.WriteString(str(object(list(c["outputs"])[0])["text"]))
				}
				return b.String()
			},
		},
		{
			model:      "mistral.mistral-large-2407-v1:0",
			request:    `{"messages":[{"role":"user","content":"Where is my refund?"}]}`,
			wantPrompt: "Where is my refund?",
			streamText: func(chunks []map[string]any) string {
				var b strings.Builder
				for _, c := range chunks {
					b.WriteString(str(object(object(list(c["choices"])[0])["message"])["content"]))
				}
				return b.String()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			fam := familyFor(tc.model)
			if fam == nil {
				t.Fatalf("no family for %s", tc.model)
			}
			req := decode(t, []byte(tc.request))
			if got := fam.prompt(req); got != tc.wantPrompt {
				t.Fatalf("prompt = %q, want %q", got, tc.wantPrompt)
			}
			u := usageFor(tc.wantPrompt, longAnswer)
			body, err := fam.render(req, longAnswer, tc.model, u)
			if err != nil {
				t.Fatal(err)
			}
			if got := fam.answer(decode(t, body)); got != longAnswer {
				t.Fatalf("answer(render(text)) = %q", got)
			}
			raw, err := fam.chunks(req, longAnswer, tc.model, u)
			if err != nil {
				t.Fatal(err)
			}
			chunks := make([]map[string]any, len(raw))
			for i, c := range raw {
				chunks[i] = decode(t, c)
			}
			if got := tc.streamText(chunks); got != longAnswer {
				t.Fatalf("stream text = %q", got)
			}
			if !strings.Contains(string(raw[len(raw)-1]), "amazon-bedrock-invocationMetrics") {
				t.Fatalf("the last chunk should carry invocation metrics: %s", raw[len(raw)-1])
			}
		})
	}
}

func joinField(field string) func([]map[string]any) string {
	return func(chunks []map[string]any) string {
		var b strings.Builder
		for _, c := range chunks {
			b.WriteString(str(c[field]))
		}
		return b.String()
	}
}

func TestFamilyForUnknownProviders(t *testing.T) {
	for _, model := range []string{"cohere.command-r-v1:0", "ai21.jamba-1-5-mini-v1:0", "amazon.titan-embed-text-v2:0", "amazon.nova-canvas-v1:0"} {
		if familyFor(model) != nil {
			t.Errorf("%s should have no text family", model)
		}
	}
}

func TestTitanEmbeddingsAreDeterministicUnitVectors(t *testing.T) {
	emb := embedderFor("amazon.titan-embed-text-v2:0")
	body, tokens, err := emb.embed(map[string]any{"inputText": "hello world"}, "amazon.titan-embed-text-v2:0")
	if err != nil || tokens != 3 {
		t.Fatalf("embed: %v tokens=%d", err, tokens)
	}
	var resp struct {
		Embedding        []float64            `json:"embedding"`
		EmbeddingsByType map[string][]float64 `json:"embeddingsByType"`
	}
	_ = json.Unmarshal(body, &resp)
	if len(resp.Embedding) != 1024 || len(resp.EmbeddingsByType["float"]) != 1024 {
		t.Fatalf("v2 defaults to 1024 dimensions, got %d", len(resp.Embedding))
	}
	var norm float64
	for _, v := range resp.Embedding {
		norm += v * v
	}
	if math.Abs(norm-1) > 1e-3 {
		t.Fatalf("vector should be unit length, norm²=%f", norm)
	}
	again, _, _ := emb.embed(map[string]any{"inputText": "hello world"}, "amazon.titan-embed-text-v2:0")
	other, _, _ := emb.embed(map[string]any{"inputText": "hello there"}, "amazon.titan-embed-text-v2:0")
	if string(again) != string(body) || string(other) == string(body) {
		t.Fatal("the same text gives the same vector and other text another")
	}

	small, _, _ := emb.embed(map[string]any{"inputText": "x", "dimensions": float64(256)}, "amazon.titan-embed-text-v2:0")
	if n := len(decode(t, small)["embedding"].([]any)); n != 256 {
		t.Fatalf("dimensions=256 gives %d", n)
	}
	v1, _, _ := emb.embed(map[string]any{"inputText": "x"}, "amazon.titan-embed-text-v1")
	if n := len(decode(t, v1)["embedding"].([]any)); n != 1536 {
		t.Fatalf("v1 has 1536 dimensions, got %d", n)
	}
	if _, _, err := emb.embed(map[string]any{"inputText": "x", "dimensions": float64(300)}, "amazon.titan-embed-text-v2:0"); err == nil {
		t.Fatal("unsupported dimensions are rejected")
	}
	if _, _, err := emb.embed(map[string]any{}, "amazon.titan-embed-text-v2:0"); err == nil {
		t.Fatal("missing inputText is rejected")
	}
}

func TestCohereEmbeddings(t *testing.T) {
	emb := embedderFor("cohere.embed-english-v3")
	body, _, err := emb.embed(map[string]any{"texts": []any{"a", "b"}, "input_type": "search_document"}, "cohere.embed-english-v3")
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, body)
	if doc["response_type"] != "embeddings_floats" || len(list(doc["embeddings"])) != 2 || len(list(list(doc["embeddings"])[0])) != 1024 {
		t.Fatalf("one 1024-d float vector per text: %v", doc["response_type"])
	}
	byType, _, _ := emb.embed(map[string]any{"texts": []any{"a"}, "embedding_types": []any{"float"}}, "cohere.embed-english-v3")
	doc = decode(t, byType)
	if doc["response_type"] != "embeddings_by_type" || len(list(object(doc["embeddings"])["float"])) != 1 {
		t.Fatalf("embedding_types gives embeddings by type: %s", byType)
	}
	if _, _, err := emb.embed(map[string]any{}, "cohere.embed-english-v3"); err == nil {
		t.Fatal("missing texts is rejected")
	}
}

func TestConversePromptIncludesToolResults(t *testing.T) {
	req := decode(t, []byte(`{"system":[{"text":"sys"}],"messages":[
		{"role":"user","content":[{"text":"weather in Paris?"}]},
		{"role":"assistant","content":[{"toolUse":{"toolUseId":"t1","name":"weather","input":{"city":"Paris"}}}]},
		{"role":"user","content":[{"toolResult":{"toolUseId":"t1","content":[{"json":{"temp":21}},{"text":"sunny"}]}}]}]}`))
	if got := conversePrompt(req); got != "sys\nweather in Paris?\n{\"temp\":21}\nsunny" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestConverseEventsReplayTextAndToolUse(t *testing.T) {
	body := []byte(`{"output":{"message":{"role":"assistant","content":[
		{"text":"` + longAnswer + `"},
		{"toolUse":{"toolUseId":"t1","name":"lookup","input":{"order":42}}}]}},
		"stopReason":"tool_use","usage":{"inputTokens":3,"outputTokens":9,"totalTokens":12}}`)
	events, err := converseEvents(body)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var text, toolInput string
	for _, e := range events {
		types = append(types, e.Type)
		p := e.Payload.(map[string]any)
		if e.Type == "contentBlockDelta" {
			delta := p["delta"].(map[string]any)
			if s, ok := delta["text"].(string); ok {
				text += s
			}
			if tu, ok := delta["toolUse"].(map[string]any); ok {
				toolInput += tu["input"].(string)
			}
		}
	}
	got := strings.Join(types, ",")
	want := "messageStart,contentBlockDelta,contentBlockDelta,contentBlockDelta,contentBlockStop,contentBlockStart,contentBlockDelta,contentBlockStop,messageStop,metadata"
	if got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
	if text != longAnswer || toolInput != `{"order":42}` {
		t.Fatalf("text %q tool input %q", text, toolInput)
	}
	stop := events[len(events)-2].Payload.(map[string]any)
	if stop["stopReason"] != "tool_use" {
		t.Fatalf("stop reason should be kept: %v", stop)
	}
}

func TestConverseAnswer(t *testing.T) {
	body, _ := renderConverse("hello", usage{1, 2})
	if converseAnswer(body) != "hello" {
		t.Fatal("answer of a rendered converse response")
	}
	if converseAnswer([]byte("not json")) != "" {
		t.Fatal("garbage has no answer")
	}
}

func TestGenericPromptCollectsTextFields(t *testing.T) {
	req := decode(t, []byte(`{"message":"hello","chat_history":[{"role":"USER","message":"before"}],"temperature":0.3}`))
	if got := genericPrompt(req); got != "before\nhello" {
		t.Fatalf("generic prompt = %q", got)
	}
}

func TestSplitText(t *testing.T) {
	if got := splitText(""); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty text is one empty piece: %q", got)
	}
	pieces := splitText(longAnswer)
	if len(pieces) < 2 || strings.Join(pieces, "") != longAnswer {
		t.Fatalf("pieces should rebuild the text: %q", pieces)
	}
	for _, p := range pieces {
		if len(p) > 48 {
			t.Fatalf("piece too long: %q", p)
		}
	}
	unbroken := strings.Repeat("é", 40) // 80 bytes, no spaces
	if strings.Join(splitText(unbroken), "") != unbroken {
		t.Fatal("cuts must not split a UTF-8 character")
	}
	for _, p := range splitText(unbroken) {
		if !strings.HasPrefix(p, "é") {
			t.Fatalf("piece starts mid-character: %q", p)
		}
	}
}
