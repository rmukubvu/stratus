package authn

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/sigv4"
)

// Authenticator verifies AWS API requests in the mode it is set to. The
// mode can change at runtime through the control API.
type Authenticator struct {
	verifier *sigv4.Verifier
	logger   *slog.Logger
	now      func() time.Time

	mu      sync.RWMutex
	mode    Mode
	journal *journal
}

// New returns an authenticator. now may be nil for the real clock; logger
// may be nil.
func New(mode Mode, credentials sigv4.CredentialStore, logger *slog.Logger, now func() time.Time) *Authenticator {
	if now == nil {
		now = time.Now
	}
	return &Authenticator{
		verifier: sigv4.NewVerifier(credentials, now),
		logger:   logger,
		now:      now,
		mode:     mode,
		journal:  newJournal(),
	}
}

var _ httpapi.Authenticator = (*Authenticator)(nil)

// Mode is the current mode.
func (a *Authenticator) Mode() Mode {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mode
}

// Set switches mode and starts a fresh journal, so the status that
// follows describes only requests made in the new mode.
func (a *Authenticator) Set(mode Mode) Status {
	a.mu.Lock()
	a.mode = mode
	a.journal = newJournal()
	a.mu.Unlock()
	return a.Status()
}

// Clear turns verification off and returns the final status. The journal
// is kept, so diagnostics can still explain earlier refusals.
func (a *Authenticator) Clear() Status {
	a.mu.Lock()
	a.mode = Off
	a.mu.Unlock()
	return a.Status()
}

// Status reports the mode, counters and recent failures.
func (a *Authenticator) Status() Status {
	a.mu.RLock()
	mode, j := a.mode, a.journal
	a.mu.RUnlock()
	return j.status(mode)
}

// Refusals maps the request IDs of refused requests to their failure, for
// diagnostics.
func (a *Authenticator) Refusals() map[string]FailureRecord {
	a.mu.RLock()
	j := a.journal
	a.mu.RUnlock()
	return j.refusals()
}

// Authenticate implements httpapi.Authenticator.
func (a *Authenticator) Authenticate(r *http.Request, md httpapi.RequestMetadata) error {
	mode := a.Mode()
	if mode == Off {
		return nil
	}
	j := a.journalRef()
	if exempt(r, md) {
		j.skip()
		return nil
	}
	if sigv4.DetectScheme(r) == sigv4.SchemeV4A {
		// SigV4a derives an ECDSA key from the secret; Stratus does not
		// verify it, and says so rather than failing correct code.
		j.skip()
		a.log(slog.LevelDebug, "sigv4a request not verified", md, nil)
		return nil
	}

	_, failure, err := a.verifier.Verify(r, sigv4.Rules{S3: md.Classification.Protocol == httpapi.ProtocolS3})
	if err != nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "IncompleteBody", Message: "could not read the request body to verify its signature: " + err.Error()}
	}
	if failure == nil {
		j.pass()
		return nil
	}

	translated := translate(failure, md.Classification)
	var apiErr *apierror.Error
	errors.As(translated, &apiErr)
	refused := mode == Strict
	j.fail(FailureRecord{
		Time:             a.now().UTC(),
		RequestID:        md.RequestID,
		Service:          md.Classification.Service,
		Operation:        md.Classification.Operation,
		Method:           r.Method,
		Path:             r.URL.Path,
		AccessKeyID:      failure.AccessKeyID,
		Presigned:        failure.Presigned,
		Kind:             string(failure.Kind),
		Code:             apiErr.Code,
		Reason:           failure.Message,
		Refused:          refused,
		CanonicalRequest: failure.CanonicalRequest,
		StringToSign:     failure.StringToSign,
	})
	a.log(slog.LevelWarn, "sigv4 verification failed", md, failure, "mode", string(mode), "code", apiErr.Code, "refused", refused)
	if !refused {
		return nil
	}
	return translated
}

func (a *Authenticator) journalRef() *journal {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.journal
}

func (a *Authenticator) log(level slog.Level, msg string, md httpapi.RequestMetadata, f *sigv4.Failure, extra ...any) {
	if a.logger == nil {
		return
	}
	args := []any{"request_id", md.RequestID, "service", md.Classification.Service, "operation", md.Classification.Operation}
	if f != nil {
		args = append(args, "kind", string(f.Kind), "access_key", f.AccessKeyID, "reason", f.Message)
	}
	a.logger.Log(context.Background(), level, msg, append(args, extra...)...)
}

// anonymousOperations are AWS operations that accept unsigned requests:
// the caller proves who it is with a token in the body instead.
var anonymousOperations = map[string]map[string]bool{
	"sts": {"AssumeRoleWithWebIdentity": true, "AssumeRoleWithSAML": true},
	"cognitoidp": {
		"InitiateAuth": true, "RespondToAuthChallenge": true, "SignUp": true, "ConfirmSignUp": true,
		"ResendConfirmationCode": true, "ForgotPassword": true, "ConfirmForgotPassword": true,
		"GetUser": true, "ChangePassword": true, "GlobalSignOut": true, "RevokeToken": true,
		"UpdateUserAttributes": true, "DeleteUser": true, "GetUserAttributeVerificationCode": true,
		"VerifyUserAttribute": true, "AssociateSoftwareToken": true, "VerifySoftwareToken": true,
		"SetUserMFAPreference": true, "ConfirmDevice": true, "ListDevices": true, "ForgetDevice": true,
		"GetDevice": true, "UpdateDeviceStatus": true, "DeleteUserAttributes": true,
	},
}

// exempt tells requests that are not AWS API calls, or that AWS accepts
// unsigned, from those that must verify.
func exempt(r *http.Request, md httpapi.RequestMetadata) bool {
	c := md.Classification
	switch {
	case c.Protocol == httpapi.ProtocolInternal, c.Service == "", c.Service == "stratus":
		return true
	case strings.HasPrefix(r.URL.Path, "/_aws/"):
		// Invocations of deployed API Gateway APIs: application traffic.
		return true
	}
	if sigv4.DetectScheme(r) != sigv4.SchemeNone {
		return false
	}
	if anonymousOperations[c.Service][c.Operation] {
		return true
	}
	// Browser uploads with an S3 POST policy sign the form, not the request.
	return c.Service == "s3" && r.Method == http.MethodPost &&
		strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data")
}
