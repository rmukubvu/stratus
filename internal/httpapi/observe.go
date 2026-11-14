package httpapi

import (
	"net/http"
	"time"
)

// RequestObserver is told about every finished AWS request, after the
// response is written, so load runs can count traffic. It is a port: the
// loadrun package adapts to it. Observe runs on the request path and must
// be quick.
type RequestObserver interface {
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
}

func (s *Server) observe(start time.Time, md RequestMetadata, status int, code string, header http.Header) {
	if s.observer == nil || md.Classification.Service == "" {
		return
	}
	s.observer.Observe(RequestOutcome{
		Time: start, Service: md.Classification.Service, Operation: md.Classification.Operation,
		Status: status, Code: code, Duration: time.Since(start),
		Fault: header.Get(FaultHeader) != "", LimitRule: header.Get(LimitHeader),
	})
}
