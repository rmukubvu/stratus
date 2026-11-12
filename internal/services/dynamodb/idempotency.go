package dynamodb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/stratus/internal/apierror"
)

// idempotencyWindow is how long DynamoDB remembers a ClientRequestToken.
const idempotencyWindow = 10 * time.Minute

// idempotencyLedger remembers committed TransactWriteItems by
// ClientRequestToken, so a retried request is answered without being
// applied twice.
type idempotencyLedger struct {
	entries map[string]ledgerEntry
}

type ledgerEntry struct {
	fingerprint string
	output      any
	at          time.Time
}

// lookup returns the stored output when token was already committed with
// the same request, or IdempotentParameterMismatchException when it was
// committed with a different one.
func (l *idempotencyLedger) lookup(token, fingerprint string, now time.Time) (any, bool, error) {
	if token == "" || l.entries == nil {
		return nil, false, nil
	}
	e, ok := l.entries[token]
	if !ok || now.Sub(e.at) > idempotencyWindow {
		return nil, false, nil
	}
	if e.fingerprint != fingerprint {
		return nil, false, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "IdempotentParameterMismatchException",
			Message: "Specified idempotent token was used with different request parameters"}
	}
	return e.output, true, nil
}

func (l *idempotencyLedger) record(token, fingerprint string, output any, now time.Time) {
	if token == "" {
		return
	}
	if l.entries == nil {
		l.entries = map[string]ledgerEntry{}
	}
	for k, e := range l.entries {
		if now.Sub(e.at) > idempotencyWindow {
			delete(l.entries, k)
		}
	}
	l.entries[token] = ledgerEntry{fingerprint: fingerprint, output: output, at: now}
}

func requestFingerprint(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
