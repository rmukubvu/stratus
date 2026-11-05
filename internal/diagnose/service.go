package diagnose

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/control"
)

// FailureSource lists recent failed requests, newest first.
type FailureSource func() []Failure

// InjectionSource maps request IDs to the chaos rule that faulted them.
type InjectionSource func() map[string]Injection

// Service answers diagnosis queries from live server data.
type Service struct {
	failures   FailureSource
	injections InjectionSource
	now        func() time.Time
}

// NewService builds a Service. injections may be nil when chaos is off.
func NewService(failures FailureSource, injections InjectionSource, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	if injections == nil {
		injections = func() map[string]Injection { return nil }
	}
	return &Service{failures: failures, injections: injections, now: now}
}

// Diagnose runs a diagnosis over the server's recent failures.
func (s *Service) Diagnose(opts Options) Report {
	return Diagnose(s.failures(), s.injections(), opts)
}

// Router is where diagnostics registers its endpoint; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes the service as
//
//	GET /_stratus/control/v1/diagnostics?since=5m&service=sqs&exclude_injected=true&max_groups=10
//
// since accepts a Go duration (look back that far) or an RFC 3339 time.
func RegisterRoutes(r Router, s *Service) {
	r.HandleFunc("GET "+control.PathPrefix+"/diagnostics", func(w http.ResponseWriter, req *http.Request) {
		opts, err := s.parseOptions(req)
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, s.Diagnose(opts))
	})
}

func (s *Service) parseOptions(req *http.Request) (Options, error) {
	q := req.URL.Query()
	opts := Options{Service: strings.ToLower(strings.TrimSpace(q.Get("service")))}

	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		since, err := ParseSince(raw, s.now())
		if err != nil {
			return Options{}, err
		}
		opts.Since = since
	}
	if raw := q.Get("exclude_injected"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Options{}, badParam("exclude_injected", "use true or false")
		}
		opts.ExcludeInjected = v
	}
	if raw := q.Get("max_groups"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return Options{}, badParam("max_groups", "use a number from 1 to 100")
		}
		opts.MaxGroups = n
	}
	return opts, nil
}

// ParseSince accepts a duration to look back ("5m") or an RFC 3339 time.
func ParseSince(raw string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Time{}, badParam("since", "use a duration such as 5m or an RFC 3339 time")
}

func badParam(name, hint string) *control.Error {
	return &control.Error{
		Kind:    control.KindInvalid,
		Code:    "InvalidParameter",
		Message: "invalid value for " + name,
		Hint:    hint,
	}
}
