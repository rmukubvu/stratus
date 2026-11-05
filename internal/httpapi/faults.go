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

// injectFaults applies the chaos decision for a request. It returns drop=true
// when the connection should be closed without a response, or an error to
// send instead of handling the request.
func (s *Server) injectFaults(w http.ResponseWriter, r *http.Request, md RequestMetadata) (drop bool, err error) {
	// Stratus's own endpoints (health, ping) are never faulted, so a "*"
	// rule cannot break readiness probes.
	if s.faults == nil || md.Classification.Protocol == ProtocolInternal {
		return false, nil
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
		return false, nil
	}
	w.Header().Set(FaultHeader, strings.Join(decision.RuleIDs, ","))

	if decision.Delay > 0 {
		timer := time.NewTimer(decision.Delay)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return false, r.Context().Err()
		}
	}
	if decision.ColdStart != "" && s.coldStart != nil {
		if err := s.coldStart(r.Context(), decision.ColdStart); err != nil {
			s.logger.Warn("chaos cold start failed", "function", decision.ColdStart, "error", err)
		}
	}
	if decision.Drop {
		return true, nil
	}
	if e := decision.Error; e != nil {
		return false, &apierror.Error{StatusCode: e.StatusCode, Code: e.Code, Message: e.Message}
	}
	return false, nil
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
