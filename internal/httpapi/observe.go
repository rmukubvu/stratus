package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/reqnote"
)

// RequestObserver is told about every finished AWS request, after the
// response is written, so load runs can count and analyse traffic. It is
// a port: the loadrun package adapts to it. Observe runs on the request
// path and must be quick.
type RequestObserver interface {
	// Listening says whether anyone wants outcomes now. While it is
	// false, requests skip collecting the details an outcome carries.
	Listening() bool
	Observe(RequestOutcome)
}

// RequestOutcome is a finished AWS request.
type RequestOutcome struct {
	Time      time.Time
	Service   string
	Operation string
	Status    int
	Code      string
	Duration  time.Duration
	// Fault is true when a chaos rule acted on the request.
	Fault bool
	// LimitRule names the limits rule that throttled it at the HTTP layer.
	LimitRule string
	// InvocationID is the AWS SDK's amz-sdk-invocation-id, shared by every
	// attempt of one SDK call; Attempt and MaxAttempts come from its
	// amz-sdk-request header. All are empty for clients that are not an
	// AWS SDK.
	InvocationID         string
	Attempt, MaxAttempts int
	// InvocationType is a Lambda Invoke's X-Amz-Invocation-Type.
	InvocationType string
	// Keys are the resources and partitions the service noted.
	Keys []reqnote.Key
}

// trace carries what an observed request collects on its way through.
type trace struct {
	invocationID         string
	attempt, maxAttempts int
	invocationType       string
	notes                *reqnote.Notes
}

// startTrace begins collecting a request's details when an observer is
// listening, returning the request with a context that collects notes.
func (s *Server) startTrace(r *http.Request) (*http.Request, *trace) {
	if s.observer == nil || !s.observer.Listening() {
		return r, nil
	}
	t := &trace{
		invocationID:   r.Header.Get("amz-sdk-invocation-id"),
		invocationType: r.Header.Get("X-Amz-Invocation-Type"),
	}
	t.attempt, t.maxAttempts = parseSDKRequest(r.Header.Get("amz-sdk-request"))
	ctx, notes := reqnote.With(r.Context())
	t.notes = notes
	return r.WithContext(ctx), t
}

// parseSDKRequest reads "attempt=2; max=3".
func parseSDKRequest(v string) (attempt, maxAttempts int) {
	for part := range strings.SplitSeq(v, ";") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "attempt":
			attempt = n
		case "max":
			maxAttempts = n
		}
	}
	return attempt, maxAttempts
}

func (s *Server) observe(start time.Time, md RequestMetadata, t *trace, status int, code string, header http.Header) {
	if t == nil || md.Classification.Service == "" {
		return
	}
	s.observer.Observe(RequestOutcome{
		Time: start, Service: md.Classification.Service, Operation: md.Classification.Operation,
		Status: status, Code: code, Duration: time.Since(start),
		Fault: header.Get(FaultHeader) != "", LimitRule: header.Get(LimitHeader),
		InvocationID: t.invocationID, Attempt: t.attempt, MaxAttempts: t.maxAttempts,
		InvocationType: t.invocationType, Keys: t.notes.Keys(),
	})
}
