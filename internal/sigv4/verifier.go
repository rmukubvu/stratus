package sigv4

import (
	"bufio"
	"crypto/hmac"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxSkew is how far a request's signing time may be from the server's
// clock, as in AWS.
const MaxSkew = 15 * time.Minute

// Credential is what the verifier needs to know about an access key.
type Credential struct {
	SecretAccessKey string
	// SessionToken, when set, must be sent with every request signed by
	// this key (temporary credentials).
	SessionToken string
	// IgnoreToken accepts any session token, or none. It suits the
	// placeholder credentials local tools use, where a stray
	// AWS_SESSION_TOKEN in the environment is not a bug worth failing on.
	IgnoreToken bool
	// Expires is when temporary credentials stop working; zero never.
	Expires time.Time
	// Inactive is a deactivated access key.
	Inactive bool
}

// CredentialStore finds the credential for an access key.
type CredentialStore interface {
	Lookup(accessKeyID string) (Credential, bool)
}

// Rules are the service-specific parts of verification.
type Rules struct {
	// S3 applies S3's rules: the path is decoded and each segment encoded
	// once (other services encode it once more as sent), the body hash
	// comes from x-amz-content-sha256 (required on signed requests),
	// x-amz-* headers must be signed, presigned URLs need X-Amz-Expires,
	// and aws-chunked bodies are verified chunk by chunk.
	S3 bool
}

// Result describes a request that verified.
type Result struct {
	Auth       *Auth
	Credential Credential
}

// Verifier checks SigV4 signatures against known credentials.
type Verifier struct {
	credentials CredentialStore
	now         func() time.Time
}

// NewVerifier returns a verifier; now may be nil for the real clock.
func NewVerifier(credentials CredentialStore, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{credentials: credentials, now: now}
}

// Verify checks the signature of r. It may read the body to hash or
// verify it; when it does, r.Body is replaced with a replay of the same
// bytes, so the handler sees the body unchanged, verified or not. The
// replacement should be closed when the request is done. A non-nil error
// is an I/O failure, not a verification failure.
func (v *Verifier) Verify(r *http.Request, rules Rules) (*Result, *Failure, error) {
	var auth *Auth
	var f *Failure
	switch DetectScheme(r) {
	case SchemeHeader:
		auth, f = ParseHeader(r)
	case SchemePresigned:
		auth, f = ParsePresigned(r, rules.S3)
	case SchemeNone:
		return nil, fail(KindMissingAuthentication, "the request is not signed"), nil
	case SchemeV4A:
		return nil, fail(KindUnsupportedAlgorithm, "SigV4a ("+AlgorithmV4A+") signatures cannot be verified; sign with "+Algorithm), nil
	default:
		return nil, fail(KindUnsupportedAlgorithm, "The authorization mechanism you have provided is not supported. Please use "+Algorithm+"."), nil
	}
	if f != nil {
		return nil, f, nil
	}
	tag := func(f *Failure) *Failure {
		f.Presigned = auth.Presigned
		f.AccessKeyID = auth.AccessKeyID
		return f
	}

	cred, f := v.credential(auth)
	if f != nil {
		return nil, tag(f), nil
	}
	if f := v.checkTime(auth); f != nil {
		return nil, tag(f), nil
	}
	if rules.S3 && !auth.Presigned {
		if f := checkAmzHeadersSigned(r, auth); f != nil {
			return nil, tag(f), nil
		}
	}

	payloadHash, kind, f, err := payloadHashFor(r, auth, rules)
	if err != nil || f != nil {
		if f != nil {
			f = tag(f)
		}
		return nil, f, err
	}

	key := SigningKey(cred.SecretAccessKey, auth.Date, auth.Region, auth.Service)
	style := PathDoubleEncoded
	if rules.S3 {
		style = PathReencoded
	}
	canonical := CanonicalRequest(r, auth, style, payloadHash)
	stringToSign := StringToSign(auth, canonical)
	if !equalSignatures(auth.Signature, Signature(key, stringToSign)) {
		f := tag(fail(KindSignatureMismatch, "The request signature we calculated does not match the signature you provided. Check your key and signing method."))
		f.SignatureProvided = auth.Signature
		f.CanonicalRequest = canonical
		f.StringToSign = stringToSign
		return nil, f, nil
	}

	if f, err := verifyPayload(r, auth, kind, payloadHash, key); err != nil || f != nil {
		if f != nil {
			f = tag(f)
		}
		return nil, f, err
	}
	return &Result{Auth: auth, Credential: cred}, nil, nil
}

func (v *Verifier) credential(auth *Auth) (Credential, *Failure) {
	cred, ok := v.credentials.Lookup(auth.AccessKeyID)
	if !ok {
		return Credential{}, fail(KindUnknownAccessKey, "The access key "+strconv.Quote(auth.AccessKeyID)+" does not exist in Stratus: it is not an IAM user key, an STS session key, a configured credential or a root key (a twelve-digit account ID or \"test\").")
	}
	if cred.Inactive {
		return Credential{}, fail(KindUnknownAccessKey, "The access key "+strconv.Quote(auth.AccessKeyID)+" is inactive.")
	}
	switch {
	case cred.SessionToken != "" && auth.SecurityToken == "":
		return Credential{}, fail(KindInvalidToken, "The access key is temporary (STS) credentials, but the request has no X-Amz-Security-Token.")
	case cred.SessionToken != "" && !hmac.Equal([]byte(cred.SessionToken), []byte(auth.SecurityToken)):
		return Credential{}, fail(KindInvalidToken, "The X-Amz-Security-Token is not the session token issued with this access key.")
	case cred.SessionToken == "" && !cred.IgnoreToken && auth.SecurityToken != "":
		return Credential{}, fail(KindInvalidToken, "The access key is a long-term key, but the request carries an X-Amz-Security-Token.")
	}
	if !cred.Expires.IsZero() && v.now().After(cred.Expires) {
		return Credential{}, fail(KindExpiredToken, "The security token included in the request expired at "+cred.Expires.UTC().Format(time.RFC3339)+".")
	}
	return cred, nil
}

func (v *Verifier) checkTime(auth *Auth) *Failure {
	now := v.now().UTC()
	signed := auth.RequestTime.UTC()
	if auth.Presigned && auth.Expires > 0 {
		if expiry := signed.Add(auth.Expires); now.After(expiry) {
			return fail(KindPresignExpired, fmt.Sprintf("Request has expired: X-Amz-Date %s plus X-Amz-Expires %d seconds is %s, and the server time is %s.",
				auth.AmzDate, int(auth.Expires/time.Second), expiry.Format(amzDateFormat), now.Format(amzDateFormat)))
		}
		if signed.After(now.Add(MaxSkew)) {
			return fail(KindRequestTimeSkewed, "Request is not yet valid: X-Amz-Date "+auth.AmzDate+" is more than 15 minutes after the server time "+now.Format(amzDateFormat)+".")
		}
		return nil
	}
	switch {
	case signed.Before(now.Add(-MaxSkew)):
		return fail(KindRequestTimeSkewed, fmt.Sprintf("Signature expired: %s is now earlier than %s (%s - 15 min.)",
			auth.AmzDate, now.Add(-MaxSkew).Format(amzDateFormat), now.Format(amzDateFormat)))
	case signed.After(now.Add(MaxSkew)):
		return fail(KindRequestTimeSkewed, fmt.Sprintf("Signature not yet current: %s is still later than %s (%s + 15 min.)",
			auth.AmzDate, now.Add(MaxSkew).Format(amzDateFormat), now.Format(amzDateFormat)))
	}
	return nil
}

// checkAmzHeadersSigned enforces S3's rule that every x-amz-* header the
// request carries is signed.
func checkAmzHeadersSigned(r *http.Request, auth *Auth) *Failure {
	var unsigned []string
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") && !auth.signs(lower) {
			unsigned = append(unsigned, lower)
		}
	}
	if len(unsigned) == 0 {
		return nil
	}
	sort.Strings(unsigned)
	return fail(KindUnsignedHeaders, "There were headers present in the request which were not signed: "+strings.Join(unsigned, ", "))
}

// payloadHashFor works out the last line of the canonical request.
func payloadHashFor(r *http.Request, auth *Auth, rules Rules) (string, payloadKind, *Failure, error) {
	if auth.Presigned {
		if rules.S3 {
			if v := r.URL.Query().Get("X-Amz-Content-Sha256"); v != "" {
				kind, f := classifyContentHash(v)
				return v, kind, f, nil
			}
			return UnsignedPayload, payloadUnsigned, nil, nil
		}
		hash, err := hashBody(r)
		return hash, payloadUnsigned, nil, err
	}
	declared := r.Header.Get("X-Amz-Content-Sha256")
	if declared == "" {
		if rules.S3 {
			return "", 0, fail(KindMissingContentHash, "Missing required header for this request: x-amz-content-sha256"), nil
		}
		hash, err := hashBody(r)
		return hash, payloadUnsigned, nil, err
	}
	kind, f := classifyContentHash(declared)
	return declared, kind, f, nil
}

// hashBody reads the body, replays it, and returns its SHA-256.
func hashBody(r *http.Request) (string, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return EmptyPayloadHash, nil
	}
	var s spool
	h, err := hashingCopy(&s, r.Body)
	if err != nil {
		s.discard()
		return "", fmt.Errorf("read request body: %w", err)
	}
	if err := replaceBody(r, &s); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyPayload checks a body against the signature that covers it.
func verifyPayload(r *http.Request, auth *Auth, kind payloadKind, declared string, key []byte) (*Failure, error) {
	switch kind {
	case payloadHashed:
		if r.Body == nil || r.Body == http.NoBody {
			if declared != EmptyPayloadHash {
				return contentHashMismatch(declared, EmptyPayloadHash), nil
			}
			return nil, nil
		}
		var s spool
		h, err := hashingCopy(&s, r.Body)
		if err != nil {
			s.discard()
			return nil, fmt.Errorf("read request body: %w", err)
		}
		if err := replaceBody(r, &s); err != nil {
			return nil, err
		}
		if actual := hex.EncodeToString(h.Sum(nil)); actual != declared {
			return contentHashMismatch(declared, actual), nil
		}
	case payloadStreaming, payloadStreamingTrailer:
		var s spool
		src := bufio.NewReader(io.TeeReader(r.Body, &s))
		signer := &chunkSigner{key: key, amzDate: auth.AmzDate, scope: auth.Scope(), previous: auth.Signature, trailer: kind == payloadStreamingTrailer}
		decoded, f, err := signer.verify(src)
		if err == nil {
			// Keep the whole body for the handler, whatever the outcome.
			_, err = io.Copy(io.Discard, src)
		}
		if err != nil {
			s.discard()
			return nil, fmt.Errorf("read request body: %w", err)
		}
		if err := replaceBody(r, &s); err != nil {
			return nil, err
		}
		if f != nil {
			return f, nil
		}
		if raw := r.Header.Get("X-Amz-Decoded-Content-Length"); raw != "" && raw != strconv.FormatInt(decoded, 10) {
			return malformedChunk("the chunks hold " + strconv.FormatInt(decoded, 10) + " bytes, but X-Amz-Decoded-Content-Length is " + raw), nil
		}
	}
	return nil, nil
}

func contentHashMismatch(declared, actual string) *Failure {
	return fail(KindContentHashMismatch, "The provided 'x-amz-content-sha256' header does not match what was computed: the request declared "+declared+", the body hashes to "+actual+".")
}

func replaceBody(r *http.Request, s *spool) error {
	body, err := s.replay()
	if err != nil {
		return fmt.Errorf("spool request body: %w", err)
	}
	if r.Body != nil {
		_ = r.Body.Close()
	}
	r.Body = body
	return nil
}

func equalSignatures(provided, expected string) bool {
	return hmac.Equal([]byte(provided), []byte(expected))
}
