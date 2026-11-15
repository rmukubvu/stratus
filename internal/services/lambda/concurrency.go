package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
)

const concurrencyBucket = "lambda-concurrency"

// Account settings Lambda reports and enforces when reserving concurrency.
const (
	accountConcurrency       = 1000
	minUnreservedConcurrency = 100
)

// ConcurrencyGate decides whether a function has a free concurrent
// execution. It is a port: the limits package adapts to it, and with no
// gate set every invocation runs.
type ConcurrencyGate interface {
	// Acquire takes one execution, returning a release function, or a
	// throttle when none is free.
	Acquire(ctx context.Context, inv Invocation) (release func(), throttle *ConcurrencyThrottle)
}

// Invocation describes an invocation for concurrency purposes.
type Invocation struct {
	Account  string
	Region   string
	Function string
	// Reserved is the function's reserved concurrency, nil when it has
	// none and shares the account's unreserved pool.
	Reserved *int
	// ReservedTotal is what every function in the account and region has
	// reserved.
	ReservedTotal int
}

// ConcurrencyThrottle says why an invocation could not run, with the
// reason Lambda puts in a 429.
type ConcurrencyThrottle struct{ Reason string }

// SetConcurrency installs the concurrency gate; nil turns it off.
func (s *Service) SetConcurrency(g ConcurrencyGate) { s.concurrency = g }

type concurrencyRecord struct {
	Reserved int `json:"reserved"`
}

// execute runs one invocation within the function's concurrency. A
// throttled invocation returns errTooManyRequests and never starts.
func (s *Service) execute(ctx context.Context, record functionRecord, payload []byte) (InvokeResult, error) {
	release, err := s.acquire(ctx, record.FunctionName)
	if err != nil {
		return InvokeResult{}, err
	}
	defer release()
	return s.invoke(ctx, record, payload)
}

func (s *Service) acquire(ctx context.Context, function string) (func(), error) {
	noop := func() {}
	if s.concurrency == nil {
		return noop, nil
	}
	reserved, total, err := s.reservations(function)
	if err != nil {
		return noop, err
	}
	release, throttle := s.concurrency.Acquire(ctx, Invocation{
		Account: s.scope.Account, Region: s.scope.Region, Function: function, Reserved: reserved, ReservedTotal: total,
	})
	if throttle != nil {
		return noop, tooManyRequests(throttle.Reason)
	}
	return release, nil
}

// Async invocations and event source mappings retry a throttled invocation
// with backoff, as Lambda's internal queue and pollers do.
const (
	throttleBackoffStart = 100 * time.Millisecond
	throttleBackoffMax   = 5 * time.Second
	defaultMaxEventAge   = 6 * time.Hour
)

// executeWithRetry runs an invocation that nobody waits on, retrying while
// it is throttled until it runs or maxAge passes.
func (s *Service) executeWithRetry(ctx context.Context, record functionRecord, payload []byte, maxAge time.Duration) (InvokeResult, error) {
	deadline := s.now().Add(maxAge)
	backoff := throttleBackoffStart
	for {
		release, err := s.acquire(ctx, record.FunctionName)
		if err == nil {
			callCtx, cancel := context.WithTimeout(ctx, time.Duration(record.Timeout+5)*time.Second)
			result, err := s.invoke(callCtx, record, payload)
			cancel()
			release()
			return result, err
		}
		if !isThrottle(err) || !s.now().Add(backoff).Before(deadline) {
			return InvokeResult{}, err
		}
		select {
		case <-ctx.Done():
			return InvokeResult{}, err
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, throttleBackoffMax)
	}
}

// maxEventAge is how long an async invocation may wait for concurrency.
func (s *Service) maxEventAge(record functionRecord) time.Duration {
	for _, qualifier := range []string{record.Version, "$LATEST"} {
		if config, err := s.loadEventInvokeConfig(record.FunctionName, qualifier); err == nil && config.MaxEventAge > 0 {
			return time.Duration(config.MaxEventAge) * time.Second
		}
	}
	return defaultMaxEventAge
}

// reservations returns the function's reserved concurrency (nil when it
// has none) and the total reserved in the account and region.
func (s *Service) reservations(function string) (*int, int, error) {
	var reserved *int
	total := 0
	err := s.metadata.Scan(concurrencyBucket, "", func(k, v []byte) error {
		var rec concurrencyRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		total += rec.Reserved
		if string(k) == function {
			n := rec.Reserved
			reserved = &n
		}
		return nil
	})
	if err != nil {
		return nil, 0, internal(err)
	}
	return reserved, total, nil
}

// SetReservedConcurrency reserves concurrency for a function, as
// PutFunctionConcurrency does; CloudFormation uses it too.
func (s *Service) SetReservedConcurrency(function string, reserved int) error {
	if _, err := s.loadRecord(function); err != nil {
		return err
	}
	if reserved < 0 {
		return badRequest("InvalidParameterValueException", "ReservedConcurrentExecutions must be greater than or equal to 0")
	}
	current, total, err := s.reservations(function)
	if err != nil {
		return err
	}
	if current != nil {
		total -= *current
	}
	if accountConcurrency-(total+reserved) < minUnreservedConcurrency {
		return badRequest("InvalidParameterValueException", fmt.Sprintf(
			"Specified ReservedConcurrentExecutions for function decreases account's UnreservedConcurrentExecution below its minimum value of [%d].", minUnreservedConcurrency))
	}
	raw, _ := json.Marshal(concurrencyRecord{Reserved: reserved})
	if err := s.metadata.Put(concurrencyBucket, function, raw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) putFunctionConcurrency(w http.ResponseWriter, r *http.Request) error {
	name, err := concurrencyFunction(r.URL.Path)
	if err != nil {
		return err
	}
	var in struct {
		ReservedConcurrentExecutions *int `json:"ReservedConcurrentExecutions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	if in.ReservedConcurrentExecutions == nil {
		return badRequest("InvalidParameterValueException", "ReservedConcurrentExecutions is required")
	}
	if err := s.SetReservedConcurrency(name, *in.ReservedConcurrentExecutions); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]int{"ReservedConcurrentExecutions": *in.ReservedConcurrentExecutions})
	return nil
}

func (s *Service) getFunctionConcurrency(w http.ResponseWriter, r *http.Request) error {
	name, err := concurrencyFunction(r.URL.Path)
	if err != nil {
		return err
	}
	if _, err := s.loadRecord(name); err != nil {
		return err
	}
	reserved, _, err := s.reservations(name)
	if err != nil {
		return err
	}
	out := map[string]int{}
	if reserved != nil {
		out["ReservedConcurrentExecutions"] = *reserved
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Service) deleteFunctionConcurrency(w http.ResponseWriter, r *http.Request) error {
	name, err := concurrencyFunction(r.URL.Path)
	if err != nil {
		return err
	}
	if _, err := s.loadRecord(name); err != nil {
		return err
	}
	if err := s.metadata.Delete(concurrencyBucket, name); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) getAccountSettings(w http.ResponseWriter, _ *http.Request) error {
	_, reserved, err := s.reservations("")
	if err != nil {
		return err
	}
	var codeSize int64
	functions := 0
	if err := s.metadata.Scan(functionsBucket, "", func(_, v []byte) error {
		var rec functionRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		codeSize += rec.CodeSize
		functions++
		return nil
	}); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"AccountLimit": map[string]int64{
			"TotalCodeSize":                  80530636800,
			"CodeSizeUnzipped":               262144000,
			"CodeSizeZipped":                 52428800,
			"ConcurrentExecutions":           accountConcurrency,
			"UnreservedConcurrentExecutions": int64(accountConcurrency - reserved),
		},
		"AccountUsage": map[string]int64{"TotalCodeSize": codeSize, "FunctionCount": int64(functions)},
	})
	return nil
}

// concurrencyResponse is the Concurrency block of GetFunction; nil when
// the function has no reserved concurrency.
func (s *Service) concurrencyResponse(function string) map[string]int {
	reserved, _, err := s.reservations(function)
	if err != nil || reserved == nil {
		return nil
	}
	return map[string]int{"ReservedConcurrentExecutions": *reserved}
}

// concurrencyFunction reads the function from
// /2017-10-31/functions/{name}/concurrency (or the 2019-09-30 path).
func concurrencyFunction(p string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(path.Clean(p), "/"), "/")
	if len(parts) != 4 || parts[1] != "functions" || parts[3] != "concurrency" || parts[2] == "" {
		return "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Function not found"}
	}
	name, _ := functionRefParts(parts[2])
	return name, nil
}

// throttledError is Lambda's 429, with the Reason a client can read.
type throttledError struct {
	apiErr apierror.Error
	reason string
}

func tooManyRequests(reason string) error {
	return &throttledError{
		apiErr: apierror.Error{StatusCode: http.StatusTooManyRequests, Code: "TooManyRequestsException", Message: "Rate Exceeded."},
		reason: reason,
	}
}

func (e *throttledError) Error() string { return e.apiErr.Error() }
func (e *throttledError) Unwrap() error { return &e.apiErr }
func (e *throttledError) WriteErrorResponse(w http.ResponseWriter) {
	w.Header().Set("X-Amzn-ErrorType", e.apiErr.Code)
	writeJSON(w, e.apiErr.StatusCode, map[string]string{
		"__type": e.apiErr.Code, "message": e.apiErr.Message, "Reason": e.reason, "Type": "User",
	})
}

func isThrottle(err error) bool {
	_, ok := err.(*throttledError)
	return ok
}
