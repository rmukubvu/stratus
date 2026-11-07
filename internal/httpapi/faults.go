package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/operator"
)

// FaultDecider decides which chaos faults to inject into a request.
type FaultDecider interface {
	Decide(req chaos.Request) chaos.Decision
}

// FaultHeader names the chaos rules that fired for a response, so a test
// failure can be traced back to the fault that caused it.
const FaultHeader = "X-Stratus-Fault"

// faultOutcome says how request handling continues after chaos.
type faultOutcome int

const (
	// faultProceed: handle the request (unless an error is returned).
	faultProceed faultOutcome = iota
	// faultDrop: close the connection without a response.
	faultDrop
	// faultResponded: chaos wrote the response; don't handle the request.
	faultResponded
)

// injectFaults applies the chaos decision for a request. It says whether to
// proceed, drop the connection or stop because the response is written, or
// returns an error to send instead of handling the request.
func (s *Server) injectFaults(w http.ResponseWriter, r *http.Request, md RequestMetadata) (faultOutcome, error) {
	// Stratus's own endpoints (health, ping) are never faulted, so a "*"
	// rule cannot break readiness probes.
	if s.faults == nil || md.Classification.Protocol == ProtocolInternal {
		return faultProceed, nil
	}
	decision := s.faults.Decide(chaos.Request{
		RequestID: md.RequestID,
		Service:   md.Classification.Service,
		Operation: md.Classification.Operation,
		Path:      r.URL.Path,
		Bucket:    md.Classification.Bucket,
		Function:  lambdaFunctionName(md.Classification.Service, r.URL.Path),
	})
	if !decision.Faulted() {
		return faultProceed, nil
	}
	w.Header().Set(FaultHeader, strings.Join(decision.RuleIDs, ","))

	if decision.Delay > 0 {
		timer := time.NewTimer(decision.Delay)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return faultProceed, r.Context().Err()
		}
	}
	if decision.ColdStart != "" && s.coldStart != nil {
		if err := s.coldStart(r.Context(), decision.ColdStart); err != nil {
			s.logger.Warn("chaos cold start failed", "function", decision.ColdStart, "error", err)
		}
	}
	if decision.Drop {
		return faultDrop, nil
	}
	if e := decision.Error; e != nil {
		return faultProceed, &apierror.Error{StatusCode: e.StatusCode, Code: e.Code, Message: e.Message}
	}
	if decision.FunctionError != "" {
		writeFunctionError(w, decision.FunctionError)
		return faultResponded, nil
	}
	return faultProceed, nil
}

// writeFunctionError answers an Invoke the way Lambda does when the
// function throws: 200, X-Amz-Function-Error and the error as the payload.
func writeFunctionError(w http.ResponseWriter, message string) {
	w.Header().Set("X-Amz-Function-Error", "Unhandled")
	WriteJSON(w, http.StatusOK, map[string]any{
		"errorType":    chaos.FunctionErrorType,
		"errorMessage": message,
	})
}

func (s *Server) recordDropped(start time.Time, r *http.Request, md RequestMetadata) {
	s.logger.Info("request dropped by chaos rule",
		"request_id", md.RequestID,
		"service", md.Classification.Service,
		"operation", md.Classification.Operation,
		"path", r.URL.Path,
	)
	if s.operator == nil {
		return
	}
	s.operator.Record(operator.RequestRecord{
		Time:         start.UTC(),
		RequestID:    md.RequestID,
		Service:      md.Classification.Service,
		Operation:    md.Classification.Operation,
		Method:       r.Method,
		Path:         r.URL.Path,
		DurationMS:   time.Since(start).Milliseconds(),
		ErrorCode:    "ConnectionDropped",
		ErrorMessage: "connection closed by chaos rule",
	})
}

// lambdaFunctionName extracts the function name from a Lambda API path such
// as /2015-03-31/functions/{name-or-arn}/invocations.
func lambdaFunctionName(service, path string) string {
	if service != "lambda" {
		return ""
	}
	_, rest, ok := strings.Cut(path, "/functions/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	if i := strings.Index(name, ":function:"); i >= 0 {
		name = name[i+len(":function:"):]
		name, _, _ = strings.Cut(name, ":") // drop a version or alias qualifier
	}
	return name
}
