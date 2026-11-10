package contract

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBedrockReplaysAndRecordsThroughTheCLI drives Bedrock Runtime with the
// AWS CLI: fixture files answer Converse and InvokeModel in the model's own
// format, embeddings need no fixture, a miss names its request hash, and
// record mode saves real answers to the fixture directory. A second Stratus
// stands in for real Bedrock, so the recording path runs signed requests
// end to end without AWS credentials.
func TestBedrockReplaysAndRecordsThroughTheCLI(t *testing.T) {
	upstream := startHarness(t)
	defer upstream.Close()

	fixtures := t.TempDir()
	writeFile(t, filepath.Join(fixtures, "support.json"),
		`{"model":"anthropic.claude-*","match":{"contains":"refund"},"text":"Refunds take five business days."}`)
	h := startHarnessWithEnv(t, t.TempDir(),
		"STRATUS_BEDROCK_FIXTURES="+fixtures,
		"STRATUS_BEDROCK_ENDPOINT="+upstream.baseURL,
		"STRATUS_BEDROCK_ACCESS_KEY_ID=AKIDRECORDER",
		"STRATUS_BEDROCK_SECRET_ACCESS_KEY=recorder-secret",
	)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"
	const haiku = "anthropic.claude-3-haiku-20240307-v1:0"

	// Converse answers from the fixture file.
	out := runAWS(t, h.baseURL, "bedrock-runtime", "converse", "--model-id", haiku,
		"--messages", `[{"role":"user","content":[{"text":"Where is my refund?"}]}]`, "--output", "json")
	var converse struct {
		Output struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		} `json:"output"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(out, &converse); err != nil || len(converse.Output.Message.Content) != 1 ||
		converse.Output.Message.Content[0].Text != "Refunds take five business days." || converse.StopReason != "end_turn" {
		t.Fatalf("converse: %v\n%s", err, out)
	}

	// InvokeModel answers the same fixture in the Anthropic Messages format.
	dir := t.TempDir()
	body := filepath.Join(dir, "body.json")
	writeFile(t, body, `{"anthropic_version":"bedrock-2023-05-31","max_tokens":50,"messages":[{"role":"user","content":"refund status?"}]}`)
	answer := filepath.Join(dir, "answer.json")
	runAWS(t, h.baseURL, "bedrock-runtime", "invoke-model", "--model-id", haiku, "--body", "fileb://"+body, answer)
	if got := readFile(t, answer); !strings.Contains(got, `"type":"message"`) || !strings.Contains(got, "Refunds take five business days.") {
		t.Fatalf("invoke-model should answer in the Messages format: %s", got)
	}

	// Embeddings are synthesized: same text, same vector.
	writeFile(t, body, `{"inputText":"hello","dimensions":256}`)
	runAWS(t, h.baseURL, "bedrock-runtime", "invoke-model", "--model-id", "amazon.titan-embed-text-v2:0", "--body", "fileb://"+body, answer)
	first := readFile(t, answer)
	runAWS(t, h.baseURL, "bedrock-runtime", "invoke-model", "--model-id", "amazon.titan-embed-text-v2:0", "--body", "fileb://"+body, answer)
	var embedding struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.Unmarshal([]byte(first), &embedding); err != nil || len(embedding.Embedding) != 256 || readFile(t, answer) != first {
		t.Fatalf("embeddings should be deterministic 256-d vectors: %v %.80s", err, first)
	}

	// ConverseStream sends AWS event-stream frames through the full server.
	status, stream := awsCall(t, http.MethodPost, h.baseURL+"/model/"+strings.ReplaceAll(haiku, ":", "%3A")+"/converse-stream", "bedrock", "application/json",
		`{"messages":[{"role":"user","content":[{"text":"refund?"}]}]}`)
	if status != http.StatusOK || !strings.Contains(stream, "messageStart") || !strings.Contains(stream, "contentBlockDelta") || !strings.Contains(stream, "five business") {
		t.Fatalf("converse-stream: %d %q", status, stream)
	}

	// A request with no fixture fails and says how to fix it.
	nova := []string{"bedrock-runtime", "converse", "--model-id", "amazon.nova-lite-v1:0",
		"--messages", `[{"role":"user","content":[{"text":"Weather in Paris?"}]}]`, "--output", "json"}
	if miss := runAWSExpectingFailure(t, h.baseURL, nova...); !strings.Contains(miss, "ResourceNotFoundException") || !strings.Contains(miss, "request_hash sha256:") {
		t.Fatalf("a miss should name its request hash: %s", miss)
	}

	// Record mode asks the upstream once and writes a fixture file.
	mustStatus(t, http.MethodPost, upstream.baseURL+"/_stratus/control/v1/bedrock/fixtures", `{"id":"real","model":"amazon.nova-*","text":"Sunny, 21 degrees."}`, http.StatusCreated)
	mustStatus(t, http.MethodPut, control+"/bedrock/mode", `{"mode":"record"}`, http.StatusOK)
	if out := runAWS(t, h.baseURL, nova...); !strings.Contains(string(out), "Sunny, 21 degrees.") {
		t.Fatalf("record should return the upstream answer: %s", out)
	}
	recorded, _ := filepath.Glob(filepath.Join(fixtures, "converse-amazon.nova-lite-v1_0-*.json"))
	if len(recorded) != 1 || !strings.Contains(readFile(t, recorded[0]), `"request_hash": "sha256:`) {
		t.Fatalf("record mode should write one fixture file: %v", recorded)
	}

	// Back in replay mode the recording answers with the upstream gone.
	mustStatus(t, http.MethodPut, control+"/bedrock/mode", `{"mode":"replay"}`, http.StatusOK)
	mustStatus(t, http.MethodDelete, upstream.baseURL+"/_stratus/control/v1/bedrock/fixtures/real", "", http.StatusOK)
	if out := runAWS(t, h.baseURL, nova...); !strings.Contains(string(out), "Sunny, 21 degrees.") {
		t.Fatalf("replay should answer from the recording: %s", out)
	}

	var log struct {
		Items []struct {
			Operation string `json:"operation"`
			Outcome   string `json:"outcome"`
		} `json:"items"`
	}
	_ = json.Unmarshal([]byte(mustStatus(t, http.MethodGet, control+"/bedrock/invocations", "", http.StatusOK)), &log)
	var outcomes []string
	for _, inv := range log.Items {
		outcomes = append(outcomes, inv.Operation+":"+inv.Outcome)
	}
	want := "Converse:replayed,InvokeModel:replayed,InvokeModel:synthesized,InvokeModel:synthesized,ConverseStream:replayed,Converse:missed,Converse:recorded,Converse:replayed"
	if strings.Join(outcomes, ",") != want {
		t.Fatalf("invocation log\n got %s\nwant %s", strings.Join(outcomes, ","), want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
