package bedrock

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// family adapts one model provider's InvokeModel body format. Converse needs
// no adapter: it is the same for every model.
type family interface {
	// prompt flattens a request body to its text.
	prompt(req map[string]any) string
	// render builds a complete response body that answers with text.
	render(req map[string]any, text, model string, u usage) ([]byte, error)
	// answer extracts the text from a response body.
	answer(resp map[string]any) string
	// chunks builds the payloads of InvokeModelWithResponseStream.
	chunks(req map[string]any, text, model string, u usage) ([][]byte, error)
}

// embedder synthesizes embeddings for a provider's embedding models.
type embedder interface {
	// embed returns the response body and the input token count.
	embed(req map[string]any, model string) ([]byte, int, error)
}

// familyFor picks the adapter for a foundation model id, or nil when Stratus
// does not know the provider's format.
func familyFor(baseModel string) family {
	switch {
	case strings.HasPrefix(baseModel, "anthropic."):
		return anthropicFamily{}
	case strings.HasPrefix(baseModel, "amazon.nova-") && !strings.Contains(baseModel, "embed") && !strings.Contains(baseModel, "canvas") && !strings.Contains(baseModel, "reel") && !strings.Contains(baseModel, "sonic"):
		return novaFamily{}
	case strings.HasPrefix(baseModel, "amazon.titan-text-") || strings.HasPrefix(baseModel, "amazon.titan-tg1"):
		return titanTextFamily{}
	case strings.HasPrefix(baseModel, "meta.llama"):
		return llamaFamily{}
	case strings.HasPrefix(baseModel, "mistral."):
		return mistralFamily{}
	}
	return nil
}

// embedderFor picks the embedding synthesizer for a model id, or nil.
func embedderFor(baseModel string) embedder {
	switch {
	case strings.HasPrefix(baseModel, "amazon.titan-embed-text-"):
		return titanEmbedder{}
	case strings.HasPrefix(baseModel, "cohere.embed-"):
		return cohereEmbedder{}
	}
	return nil
}

// --- Anthropic Claude (Messages API) ---

type anthropicFamily struct{}

func (anthropicFamily) prompt(req map[string]any) string {
	var parts []string
	if legacy, ok := req["prompt"].(string); ok {
		parts = append(parts, legacy)
	}
	parts = append(parts, anthropicText(req["system"]))
	for _, m := range list(req["messages"]) {
		parts = append(parts, anthropicText(object(m)["content"]))
	}
	return joinText(parts)
}

// anthropicText flattens a content value: a string, or blocks of text and
// tool results.
func anthropicText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	for _, b := range list(content) {
		block := object(b)
		switch block["type"] {
		case "text":
			parts = append(parts, str(block["text"]))
		case "tool_result":
			parts = append(parts, anthropicText(block["content"]))
		}
	}
	return joinText(parts)
}

func (anthropicFamily) render(_ map[string]any, text, model string, u usage) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":            "msg_stratus_" + shortHash(text),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{map[string]any{"type": "text", "text": text}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": u.Input, "output_tokens": u.Output},
	})
}

func (anthropicFamily) answer(resp map[string]any) string {
	if completion, ok := resp["completion"].(string); ok {
		return completion
	}
	var parts []string
	for _, b := range list(resp["content"]) {
		if block := object(b); block["type"] == "text" {
			parts = append(parts, str(block["text"]))
		}
	}
	return strings.Join(parts, "")
}

func (anthropicFamily) chunks(_ map[string]any, text, model string, u usage) ([][]byte, error) {
	events := []any{
		map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_stratus_" + shortHash(text), "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": u.Input, "output_tokens": 1},
		}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
	}
	for _, piece := range splitText(text) {
		events = append(events, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": piece}})
	}
	events = append(events,
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": u.Output}},
		map[string]any{"type": "message_stop", "amazon-bedrock-invocationMetrics": invocationMetrics(u)},
	)
	return marshalAll(events)
}

// --- Amazon Nova ---

type novaFamily struct{}

func (novaFamily) prompt(req map[string]any) string {
	parts := blockTexts(req["system"])
	for _, m := range list(req["messages"]) {
		parts = append(parts, blockTexts(object(m)["content"])...)
	}
	return joinText(parts)
}

func (novaFamily) render(_ map[string]any, text, _ string, u usage) ([]byte, error) {
	return json.Marshal(map[string]any{
		"output":     map[string]any{"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"text": text}}}},
		"stopReason": "end_turn",
		"usage":      map[string]any{"inputTokens": u.Input, "outputTokens": u.Output, "totalTokens": u.Input + u.Output},
	})
}

func (novaFamily) answer(resp map[string]any) string {
	return strings.Join(blockTexts(object(object(resp["output"])["message"])["content"]), "")
}

func (novaFamily) chunks(_ map[string]any, text, _ string, u usage) ([][]byte, error) {
	events := []any{map[string]any{"messageStart": map[string]any{"role": "assistant"}}}
	for _, piece := range splitText(text) {
		events = append(events, map[string]any{"contentBlockDelta": map[string]any{"delta": map[string]any{"text": piece}, "contentBlockIndex": 0}})
	}
	events = append(events,
		map[string]any{"contentBlockStop": map[string]any{"contentBlockIndex": 0}},
		map[string]any{"messageStop": map[string]any{"stopReason": "end_turn"}},
		map[string]any{
			"metadata":                         map[string]any{"usage": map[string]any{"inputTokens": u.Input, "outputTokens": u.Output}, "metrics": map[string]any{}, "trace": map[string]any{}},
			"amazon-bedrock-invocationMetrics": invocationMetrics(u),
		},
	)
	return marshalAll(events)
}

// --- Amazon Titan Text ---

type titanTextFamily struct{}

func (titanTextFamily) prompt(req map[string]any) string { return str(req["inputText"]) }

func (titanTextFamily) render(_ map[string]any, text, _ string, u usage) ([]byte, error) {
	return json.Marshal(map[string]any{
		"inputTextTokenCount": u.Input,
		"results":             []any{map[string]any{"tokenCount": u.Output, "outputText": text, "completionReason": "FINISH"}},
	})
}

func (titanTextFamily) answer(resp map[string]any) string {
	var parts []string
	for _, r := range list(resp["results"]) {
		parts = append(parts, str(object(r)["outputText"]))
	}
	return strings.Join(parts, "")
}

func (titanTextFamily) chunks(_ map[string]any, text, _ string, u usage) ([][]byte, error) {
	pieces := splitText(text)
	events := make([]any, len(pieces))
	for i, piece := range pieces {
		chunk := map[string]any{"outputText": piece, "index": 0, "totalOutputTextTokenCount": u.Output, "completionReason": nil, "inputTextTokenCount": nil}
		if i == 0 {
			chunk["inputTextTokenCount"] = u.Input
		}
		if i == len(pieces)-1 {
			chunk["completionReason"] = "FINISH"
			chunk["amazon-bedrock-invocationMetrics"] = invocationMetrics(u)
		}
		events[i] = chunk
	}
	return marshalAll(events)
}

// --- Meta Llama ---

type llamaFamily struct{}

func (llamaFamily) prompt(req map[string]any) string { return str(req["prompt"]) }

func (llamaFamily) render(_ map[string]any, text, _ string, u usage) ([]byte, error) {
	return json.Marshal(map[string]any{"generation": text, "prompt_token_count": u.Input, "generation_token_count": u.Output, "stop_reason": "stop"})
}

func (llamaFamily) answer(resp map[string]any) string { return str(resp["generation"]) }

func (llamaFamily) chunks(_ map[string]any, text, _ string, u usage) ([][]byte, error) {
	pieces := splitText(text)
	events := make([]any, len(pieces))
	for i, piece := range pieces {
		chunk := map[string]any{"generation": piece, "prompt_token_count": nil, "generation_token_count": estimateTokens(strings.Join(pieces[:i+1], "")), "stop_reason": nil}
		if i == 0 {
			chunk["prompt_token_count"] = u.Input
		}
		if i == len(pieces)-1 {
			chunk["stop_reason"] = "stop"
			chunk["amazon-bedrock-invocationMetrics"] = invocationMetrics(u)
		}
		events[i] = chunk
	}
	return marshalAll(events)
}

// --- Mistral (text completion and chat) ---

type mistralFamily struct{}

func (mistralFamily) prompt(req map[string]any) string {
	if p, ok := req["prompt"].(string); ok {
		return p
	}
	var parts []string
	for _, m := range list(req["messages"]) {
		parts = append(parts, str(object(m)["content"]))
	}
	return joinText(parts)
}

func isMistralChat(req map[string]any) bool {
	_, ok := req["messages"]
	return ok
}

func (mistralFamily) render(req map[string]any, text, _ string, _ usage) ([]byte, error) {
	if isMistralChat(req) {
		return json.Marshal(map[string]any{"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "stop_reason": "stop",
		}}})
	}
	return json.Marshal(map[string]any{"outputs": []any{map[string]any{"text": text, "stop_reason": "stop"}}})
}

func (mistralFamily) answer(resp map[string]any) string {
	var parts []string
	for _, o := range list(resp["outputs"]) {
		parts = append(parts, str(object(o)["text"]))
	}
	for _, c := range list(resp["choices"]) {
		parts = append(parts, str(object(object(c)["message"])["content"]))
	}
	return strings.Join(parts, "")
}

func (mistralFamily) chunks(req map[string]any, text, _ string, u usage) ([][]byte, error) {
	pieces := splitText(text)
	events := make([]any, len(pieces))
	for i, piece := range pieces {
		var stop any
		if i == len(pieces)-1 {
			stop = "stop"
		}
		var chunk map[string]any
		if isMistralChat(req) {
			chunk = map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": piece}, "stop_reason": stop}}}
		} else {
			chunk = map[string]any{"outputs": []any{map[string]any{"text": piece, "stop_reason": stop}}}
		}
		if stop != nil {
			chunk["amazon-bedrock-invocationMetrics"] = invocationMetrics(u)
		}
		events[i] = chunk
	}
	return marshalAll(events)
}

// --- Embeddings ---

type titanEmbedder struct{}

func (titanEmbedder) embed(req map[string]any, model string) ([]byte, int, error) {
	text := str(req["inputText"])
	if text == "" {
		return nil, 0, validation("inputText is required for " + model)
	}
	dims := 1024
	if strings.HasPrefix(BaseModelID(model), "amazon.titan-embed-text-v1") {
		dims = 1536
	}
	if d, ok := req["dimensions"].(float64); ok {
		switch int(d) {
		case 256, 512, 1024:
			dims = int(d)
		default:
			return nil, 0, validation("dimensions must be 256, 512 or 1024")
		}
	}
	vec := embedding(model, text, dims)
	tokens := estimateTokens(text)
	body, err := json.Marshal(map[string]any{
		"embedding":           vec,
		"embeddingsByType":    map[string]any{"float": vec},
		"inputTextTokenCount": tokens,
	})
	return body, tokens, err
}

type cohereEmbedder struct{}

func (cohereEmbedder) embed(req map[string]any, model string) ([]byte, int, error) {
	var texts []string
	for _, t := range list(req["texts"]) {
		texts = append(texts, str(t))
	}
	if len(texts) == 0 {
		return nil, 0, validation("texts is required for " + model)
	}
	dims := 1024
	if d, ok := req["output_dimension"].(float64); ok && d > 0 {
		dims = int(d)
	}
	vectors := make([][]float64, len(texts))
	tokens := 0
	for i, t := range texts {
		vectors[i] = embedding(model, t, dims)
		tokens += estimateTokens(t)
	}
	resp := map[string]any{
		"id":            "stratus-" + shortHash(strings.Join(texts, "\x00")),
		"texts":         texts,
		"embeddings":    vectors,
		"response_type": "embeddings_floats",
	}
	if len(list(req["embedding_types"])) > 0 {
		resp["embeddings"] = map[string]any{"float": vectors}
		resp["response_type"] = "embeddings_by_type"
	}
	body, err := json.Marshal(resp)
	return body, tokens, err
}

// embedding derives a unit vector from model and text. The same input
// always yields the same vector; different inputs yield unrelated ones.
func embedding(model, text string, dims int) []float64 {
	seed := sha256.Sum256([]byte(BaseModelID(model) + "\x00" + text))
	state := binary.BigEndian.Uint64(seed[:8])
	vec := make([]float64, dims)
	var norm float64
	for i := range vec {
		state += 0x9e3779b97f4a7c15 // splitmix64
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		vec[i] = float64(z>>11)/float64(1<<53)*2 - 1
		norm += vec[i] * vec[i]
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = math.Round(vec[i]/norm*1e6) / 1e6
	}
	return vec
}

// --- Converse (model independent) ---

// conversePrompt flattens a Converse request: system prompt, message text
// and tool results.
func conversePrompt(req map[string]any) string {
	parts := blockTexts(req["system"])
	for _, m := range list(req["messages"]) {
		for _, b := range list(object(m)["content"]) {
			block := object(b)
			switch {
			case block["text"] != nil:
				parts = append(parts, str(block["text"]))
			case block["toolResult"] != nil:
				for _, c := range list(object(block["toolResult"])["content"]) {
					item := object(c)
					if item["json"] != nil {
						raw, _ := json.Marshal(item["json"])
						parts = append(parts, string(raw))
					} else {
						parts = append(parts, str(item["text"]))
					}
				}
			}
		}
	}
	return joinText(parts)
}

func renderConverse(text string, u usage) ([]byte, error) {
	return json.Marshal(map[string]any{
		"output":     map[string]any{"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"text": text}}}},
		"stopReason": "end_turn",
		"usage":      map[string]any{"inputTokens": u.Input, "outputTokens": u.Output, "totalTokens": u.Input + u.Output},
		"metrics":    map[string]any{"latencyMs": 0},
	})
}

// converseResponse is the part of a Converse response that streaming and
// the log need.
type converseResponse struct {
	Output struct {
		Message struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason                    string         `json:"stopReason"`
	Usage                         map[string]any `json:"usage"`
	AdditionalModelResponseFields any            `json:"additionalModelResponseFields,omitempty"`
}

func converseAnswer(body []byte) string {
	var resp converseResponse
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	var parts []string
	for _, block := range resp.Output.Message.Content {
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

// streamEvent is one ConverseStream event.
type streamEvent struct {
	Type    string
	Payload any
}

// converseEvents replays a Converse response as the ConverseStream events
// Bedrock would send: text and tool input arrive as deltas.
func converseEvents(body []byte) ([]streamEvent, error) {
	var resp converseResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	role := resp.Output.Message.Role
	if role == "" {
		role = "assistant"
	}
	events := []streamEvent{{"messageStart", map[string]any{"role": role}}}
	for i, block := range resp.Output.Message.Content {
		switch {
		case block["text"] != nil:
			for _, piece := range splitText(str(block["text"])) {
				events = append(events, streamEvent{"contentBlockDelta", map[string]any{"contentBlockIndex": i, "delta": map[string]any{"text": piece}}})
			}
		case block["toolUse"] != nil:
			tool := object(block["toolUse"])
			events = append(events, streamEvent{"contentBlockStart", map[string]any{"contentBlockIndex": i, "start": map[string]any{
				"toolUse": map[string]any{"toolUseId": tool["toolUseId"], "name": tool["name"]},
			}}})
			input, _ := json.Marshal(tool["input"])
			events = append(events, streamEvent{"contentBlockDelta", map[string]any{"contentBlockIndex": i, "delta": map[string]any{
				"toolUse": map[string]any{"input": string(input)},
			}}})
		case block["reasoningContent"] != nil:
			reasoning := object(object(block["reasoningContent"])["reasoningText"])
			for _, piece := range splitText(str(reasoning["text"])) {
				events = append(events, streamEvent{"contentBlockDelta", map[string]any{"contentBlockIndex": i, "delta": map[string]any{"reasoningContent": map[string]any{"text": piece}}}})
			}
			if sig := str(reasoning["signature"]); sig != "" {
				events = append(events, streamEvent{"contentBlockDelta", map[string]any{"contentBlockIndex": i, "delta": map[string]any{"reasoningContent": map[string]any{"signature": sig}}}})
			}
		default:
			continue
		}
		events = append(events, streamEvent{"contentBlockStop", map[string]any{"contentBlockIndex": i}})
	}
	stop := map[string]any{"stopReason": resp.StopReason}
	if resp.StopReason == "" {
		stop["stopReason"] = "end_turn"
	}
	if resp.AdditionalModelResponseFields != nil {
		stop["additionalModelResponseFields"] = resp.AdditionalModelResponseFields
	}
	events = append(events,
		streamEvent{"messageStop", stop},
		streamEvent{"metadata", map[string]any{"usage": resp.Usage, "metrics": map[string]any{"latencyMs": 0}}},
	)
	return events, nil
}

// genericPrompt flattens an unknown provider's body by collecting the text
// under the keys providers use for prompts.
func genericPrompt(req map[string]any) string {
	var parts []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch val := v.(type) {
		case string:
			switch key {
			case "prompt", "inputText", "text", "content", "message", "system", "input":
				parts = append(parts, val)
			}
		case []any:
			for _, item := range val {
				walk(key, item)
			}
		case map[string]any:
			keys := make([]string, 0, len(val))
			for k := range val {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(k, val[k])
			}
		}
	}
	walk("", req)
	return joinText(parts)
}

// --- helpers ---

// splitText cuts text into stream-sized pieces, preferring to cut after a
// space, so streaming clients see several deltas.
func splitText(text string) []string {
	const size = 48
	if text == "" {
		return []string{""}
	}
	var pieces []string
	for len(text) > size {
		cut := strings.LastIndex(text[:size], " ")
		if cut <= 0 {
			cut = size
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		} else {
			cut++
		}
		pieces = append(pieces, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		pieces = append(pieces, text)
	}
	return pieces
}

func invocationMetrics(u usage) map[string]any {
	return map[string]any{"inputTokenCount": u.Input, "outputTokenCount": u.Output, "invocationLatency": 0, "firstByteLatency": 0}
}

func blockTexts(v any) []string {
	var parts []string
	for _, b := range list(v) {
		if text := str(object(b)["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return parts
}

func joinText(parts []string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

func marshalAll(events []any) ([][]byte, error) {
	out := make([][]byte, len(events))
	for i, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		out[i] = raw
	}
	return out, nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
