// Package authn checks the SigV4 signature of AWS API requests against the
// credentials Stratus knows, when strict signature mode is on.
//
// By default Stratus accepts any signature, as local emulators do. That
// hides signing bugs (a header changed after signing, a wrong region in the
// credential scope, a presigned URL used after it expired) until the code
// meets real AWS. With STRATUS_SIGV4=strict, Stratus refuses such requests
// with the error code the AWS service would return; with audit it lets them
// through and records what it would have refused.
package authn

import (
	"fmt"
	"strings"
)

// Mode selects what happens to a request whose signature does not verify.
type Mode string

const (
	// Off checks nothing: any signature, or none, is accepted.
	Off Mode = "off"
	// Audit verifies every request and records failures, but lets them
	// through.
	Audit Mode = "audit"
	// Strict refuses requests that do not verify, as AWS would.
	Strict Mode = "strict"
)

// ParseMode reads STRATUS_SIGV4 or --sigv4.
func ParseMode(raw string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", Off:
		return Off, nil
	case Audit:
		return Audit, nil
	case Strict:
		return Strict, nil
	}
	return Off, fmt.Errorf("STRATUS_SIGV4 must be off, audit or strict, got %q", raw)
}
