package authn

import (
	"sync"
	"time"
)

// FailureRecord is one request that did not verify.
type FailureRecord struct {
	Time        time.Time `json:"time"`
	RequestID   string    `json:"request_id"`
	Service     string    `json:"service"`
	Operation   string    `json:"operation,omitempty"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	AccessKeyID string    `json:"access_key_id,omitempty"`
	Presigned   bool      `json:"presigned,omitempty"`
	// Kind is the protocol-neutral reason, such as signature_mismatch.
	Kind string `json:"kind"`
	// Code is the AWS error code the request got (or would have got in
	// audit mode).
	Code   string `json:"code"`
	Reason string `json:"reason"`
	// Refused is false in audit mode: the request went through.
	Refused bool `json:"refused"`
	// CanonicalRequest and StringToSign are what Stratus expected the
	// client to sign; compare them with what the client's signer built.
	CanonicalRequest string `json:"canonical_request,omitempty"`
	StringToSign     string `json:"string_to_sign,omitempty"`
}

// Status is what GET /_stratus/control/v1/sigv4 returns.
type Status struct {
	Mode Mode `json:"mode"`
	// Verified requests had a valid signature.
	Verified int64 `json:"verified"`
	// Failed requests did not verify; in strict mode they were refused.
	Failed int64 `json:"failed"`
	// Skipped requests are not checked: health and control calls, deployed
	// API invocations, operations AWS accepts unsigned, and SigV4a.
	Skipped int64            `json:"skipped"`
	ByCode  map[string]int64 `json:"failures_by_code"`
	// Recent holds the latest failures, newest first.
	Recent []FailureRecord `json:"recent_failures"`
}

const maxRecent = 50

// journal counts outcomes and keeps the latest failures.
type journal struct {
	mu       sync.Mutex
	verified int64
	failed   int64
	skipped  int64
	byCode   map[string]int64
	recent   []FailureRecord // ring, oldest first
}

func newJournal() *journal { return &journal{byCode: map[string]int64{}} }

func (j *journal) pass() {
	j.mu.Lock()
	j.verified++
	j.mu.Unlock()
}

func (j *journal) skip() {
	j.mu.Lock()
	j.skipped++
	j.mu.Unlock()
}

func (j *journal) fail(rec FailureRecord) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failed++
	j.byCode[rec.Code]++
	j.recent = append(j.recent, rec)
	if len(j.recent) > maxRecent {
		j.recent = j.recent[len(j.recent)-maxRecent:]
	}
}

func (j *journal) status(mode Mode) Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	byCode := make(map[string]int64, len(j.byCode))
	for k, v := range j.byCode {
		byCode[k] = v
	}
	recent := make([]FailureRecord, len(j.recent))
	for i, rec := range j.recent {
		recent[len(j.recent)-1-i] = rec
	}
	return Status{Mode: mode, Verified: j.verified, Failed: j.failed, Skipped: j.skipped, ByCode: byCode, Recent: recent}
}

func (j *journal) refusals() map[string]FailureRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]FailureRecord{}
	for _, rec := range j.recent {
		if rec.Refused && rec.RequestID != "" {
			out[rec.RequestID] = rec
		}
	}
	return out
}
