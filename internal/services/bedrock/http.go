package bedrock

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stratus/internal/apierror"
)

// routes maps the last path segment of /model/{modelId}/{action} to its
// operation.
var routes = map[string]string{
	"invoke":                      OpInvokeModel,
	"invoke-with-response-stream": OpInvokeModelWithResponseStream,
	"converse":                    OpConverse,
	"converse-stream":             OpConverseStream,
	"count-tokens":                OpCountTokens,
}

// Operation names the Bedrock Runtime operation for a request, or "" when
// the path is not one Stratus serves.
func Operation(method, escapedPath string) string {
	if method != http.MethodPost {
		return ""
	}
	_, action, ok := splitModelPath(escapedPath)
	if !ok {
		return ""
	}
	return routes[action]
}

// splitModelPath parses /model/{modelId}/{action}. The model id stays
// escaped in the path because ARNs contain '/'.
func splitModelPath(escapedPath string) (model, action string, ok bool) {
	rest, found := strings.CutPrefix(escapedPath, "/model/")
	if !found {
		return "", "", false
	}
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		return "", "", false
	}
	model, err := url.PathUnescape(rest[:i])
	if err != nil {
		return "", "", false
	}
	return model, rest[i+1:], true
}

// Handle serves one Bedrock Runtime request.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) error {
	model, action, ok := splitModelPath(r.URL.EscapedPath())
	operation := routes[action]
	if !ok || operation == "" || r.Method != http.MethodPost {
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "UnknownOperationException",
			Message: "Stratus serves InvokeModel, InvokeModelWithResponseStream, Converse, ConverseStream and CountTokens; " + r.Method + " " + r.URL.Path + " is not one of them"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return validation("read request body: " + err.Error())
	}
	if len(body) > maxBodyBytes {
		return validation("the request body is larger than 25 MB")
	}

	if operation == OpCountTokens {
		tokens, err := s.CountTokens(model, body)
		if err != nil {
			return err
		}
		return writeJSON(w, map[string]any{"inputTokens": tokens})
	}

	ans, err := s.Respond(r.Context(), operation, model, body)
	if err != nil {
		return err
	}
	switch operation {
	case OpInvokeModel:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-Bedrock-Input-Token-Count", strconv.Itoa(ans.usage.Input))
		w.Header().Set("X-Amzn-Bedrock-Output-Token-Count", strconv.Itoa(ans.usage.Output))
		w.Header().Set("X-Amzn-Bedrock-Invocation-Latency", "0")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(ans.Body)
		return err
	case OpInvokeModelWithResponseStream:
		return s.streamInvoke(w, model, ans)
	case OpConverse:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write(ans.Body)
		return err
	default:
		return streamConverse(w, ans)
	}
}

// streamInvoke sends the answer as InvokeModelWithResponseStream chunks in
// the provider's streaming format. A provider Stratus does not know gets
// the whole body as one chunk.
func (s *Service) streamInvoke(w http.ResponseWriter, model string, ans Answer) error {
	payloads := [][]byte{ans.Body}
	if ans.family != nil {
		chunks, err := ans.family.chunks(ans.request, ans.Text, BaseModelID(model), ans.usage)
		if err != nil {
			return internal("build stream: " + err.Error())
		}
		payloads = chunks
	}
	frames := make([][]byte, len(payloads))
	for i, p := range payloads {
		part, err := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(p)})
		if err != nil {
			return internal(err.Error())
		}
		frames[i] = eventMessage("chunk", part)
	}
	w.Header().Set("X-Amzn-Bedrock-Content-Type", "application/json")
	return writeStream(w, frames)
}

// streamConverse replays the Converse answer as ConverseStream events.
func streamConverse(w http.ResponseWriter, ans Answer) error {
	events, err := converseEvents(ans.Body)
	if err != nil {
		return internal("build stream: " + err.Error())
	}
	frames := make([][]byte, len(events))
	for i, e := range events {
		payload, err := json.Marshal(e.Payload)
		if err != nil {
			return internal(err.Error())
		}
		frames[i] = eventMessage(e.Type, payload)
	}
	return writeStream(w, frames)
}

// writeStream writes event-stream frames, flushing each so a streaming
// client sees them arrive one by one.
func writeStream(w http.ResponseWriter, frames [][]byte) error {
	w.Header().Set("Content-Type", EventStreamContentType)
	w.WriteHeader(http.StatusOK)
	flusher := http.NewResponseController(w)
	for _, frame := range frames {
		if _, err := w.Write(frame); err != nil {
			return nil // the client went away; the status is already sent
		}
		_ = flusher.Flush()
	}
	return nil
}

func writeJSON(w http.ResponseWriter, payload any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(payload)
}
