// Package sigv4 verifies AWS Signature Version 4 requests: Authorization
// headers, presigned URLs, and S3's signed and unsigned payload variants,
// including aws-chunked streaming uploads.
//
// The package knows nothing about AWS services or error formats. It reports
// a Failure with a protocol-neutral Kind, and the caller turns that into the
// error the service in question would return.
package sigv4

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Algorithm is the only signing algorithm Verify accepts.
const Algorithm = "AWS4-HMAC-SHA256"

// AlgorithmV4A is SigV4a, the asymmetric multi-region variant.
const AlgorithmV4A = "AWS4-ECDSA-P256-SHA256"

const (
	amzDateFormat   = "20060102T150405Z"
	scopeTerminator = "aws4_request"
	// MaxPresignExpiry is the longest X-Amz-Expires AWS accepts: a week.
	MaxPresignExpiry = 7 * 24 * time.Hour
)

// Scheme is how a request carries its credentials.
type Scheme int

const (
	// SchemeNone is an anonymous request.
	SchemeNone Scheme = iota
	// SchemeHeader is SigV4 in the Authorization header.
	SchemeHeader
	// SchemePresigned is SigV4 in the query string.
	SchemePresigned
	// SchemeV4A is SigV4a, which Verify cannot check.
	SchemeV4A
	// SchemeOther is any other signature, such as SigV2.
	SchemeOther
)

// DetectScheme tells how r is authenticated, without validating anything.
func DetectScheme(r *http.Request) Scheme {
	if header := r.Header.Get("Authorization"); header != "" {
		switch {
		case strings.HasPrefix(header, Algorithm+" "):
			return SchemeHeader
		case strings.HasPrefix(header, AlgorithmV4A+" "):
			return SchemeV4A
		}
		return SchemeOther
	}
	q := r.URL.Query()
	switch q.Get("X-Amz-Algorithm") {
	case Algorithm:
		return SchemePresigned
	case AlgorithmV4A:
		return SchemeV4A
	case "":
	default:
		return SchemeOther
	}
	if q.Has("X-Amz-Signature") || q.Has("X-Amz-Credential") {
		return SchemePresigned
	}
	// SigV2: Signature + AWSAccessKeyId (S3) or SignatureVersion (query).
	if q.Has("AWSAccessKeyId") || q.Has("SignatureVersion") {
		return SchemeOther
	}
	return SchemeNone
}

// Auth is the parsed signature of a request.
type Auth struct {
	AccessKeyID string
	// Date, Region and Service form the credential scope with Date in
	// YYYYMMDD form.
	Date    string
	Region  string
	Service string
	// SignedHeaders are lower-case header names in the order signed.
	SignedHeaders []string
	Signature     string
	// RequestTime is X-Amz-Date (or the Date header).
	RequestTime time.Time
	// AmzDate is RequestTime as the client wrote it, in ISO 8601 basic form.
	AmzDate string
	// Presigned is true for query-string authentication.
	Presigned bool
	// Expires is X-Amz-Expires for presigned requests; zero if absent.
	Expires       time.Duration
	SecurityToken string
}

// Scope is the credential scope string, date/region/service/aws4_request.
func (a *Auth) Scope() string {
	return a.Date + "/" + a.Region + "/" + a.Service + "/" + scopeTerminator
}

// ParseHeader parses an AWS4-HMAC-SHA256 Authorization header and the
// date and token headers that go with it.
func ParseHeader(r *http.Request) (*Auth, *Failure) {
	header := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(header, Algorithm+" ")
	if !ok {
		return nil, fail(KindUnsupportedAlgorithm, "The authorization mechanism you have provided is not supported. Please use "+Algorithm+".")
	}
	params := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if found {
			params[name] = strings.TrimSpace(value)
		}
	}
	var missing []string
	for _, name := range []string{"Credential", "SignedHeaders", "Signature"} {
		if params[name] == "" {
			missing = append(missing, "Authorization header requires '"+name+"' parameter.")
		}
	}
	if len(missing) > 0 {
		return nil, fail(KindIncompleteSignature, strings.Join(missing, " ")+" Authorization="+header)
	}

	auth := &Auth{Signature: params["Signature"], SecurityToken: r.Header.Get("X-Amz-Security-Token")}
	if f := auth.parseCredential(params["Credential"]); f != nil {
		return nil, f
	}
	auth.SignedHeaders = splitSignedHeaders(params["SignedHeaders"])

	switch {
	case r.Header.Get("X-Amz-Date") != "":
		auth.AmzDate = r.Header.Get("X-Amz-Date")
		t, err := time.Parse(amzDateFormat, auth.AmzDate)
		if err != nil {
			return nil, fail(KindIncompleteSignature, "X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\", got "+strconv.Quote(auth.AmzDate))
		}
		auth.RequestTime = t
	case r.Header.Get("Date") != "":
		t, err := http.ParseTime(r.Header.Get("Date"))
		if err != nil {
			return nil, fail(KindIncompleteSignature, "the Date header is not a valid HTTP date: "+strconv.Quote(r.Header.Get("Date")))
		}
		auth.RequestTime = t.UTC()
		auth.AmzDate = auth.RequestTime.Format(amzDateFormat)
	default:
		return nil, fail(KindIncompleteSignature, "Authorization header requires existence of either a 'X-Amz-Date' or a 'Date' header. Authorization="+header)
	}
	if f := auth.checkCommon(); f != nil {
		return nil, f
	}
	return auth, nil
}

// ParsePresigned parses the X-Amz-* parameters of a presigned URL.
// requireExpires makes X-Amz-Expires mandatory, as S3 does.
func ParsePresigned(r *http.Request, requireExpires bool) (*Auth, *Failure) {
	q := r.URL.Query()
	required := []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Signature", "X-Amz-Date", "X-Amz-SignedHeaders"}
	if requireExpires {
		required = append(required, "X-Amz-Expires")
	}
	for _, name := range required {
		if q.Get(name) == "" {
			f := fail(KindIncompleteSignature, "Query-string authentication version 4 requires the "+strings.Join(required, ", ")+" parameters.")
			f.Presigned = true
			return nil, f
		}
	}
	presignedFail := func(kind Kind, message string) (*Auth, *Failure) {
		f := fail(kind, message)
		f.Presigned = true
		return nil, f
	}
	if q.Get("X-Amz-Algorithm") != Algorithm {
		return presignedFail(KindUnsupportedAlgorithm, "X-Amz-Algorithm only supports \""+Algorithm+"\"")
	}
	auth := &Auth{
		Presigned:     true,
		Signature:     q.Get("X-Amz-Signature"),
		SignedHeaders: splitSignedHeaders(q.Get("X-Amz-SignedHeaders")),
		SecurityToken: q.Get("X-Amz-Security-Token"),
		AmzDate:       q.Get("X-Amz-Date"),
	}
	if f := auth.parseCredential(q.Get("X-Amz-Credential")); f != nil {
		f.Presigned = true
		return nil, f
	}
	t, err := time.Parse(amzDateFormat, auth.AmzDate)
	if err != nil {
		return presignedFail(KindIncompleteSignature, "X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\"")
	}
	auth.RequestTime = t
	if raw := q.Get("X-Amz-Expires"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		switch {
		case err != nil || seconds < 0:
			return presignedFail(KindIncompleteSignature, "X-Amz-Expires should be a number")
		case time.Duration(seconds)*time.Second > MaxPresignExpiry:
			return presignedFail(KindIncompleteSignature, "X-Amz-Expires must be less than a week (in seconds) that is 604800")
		}
		auth.Expires = time.Duration(seconds) * time.Second
	}
	if f := auth.checkCommon(); f != nil {
		f.Presigned = true
		return nil, f
	}
	return auth, nil
}

// parseCredential reads <key>/<date>/<region>/<service>/aws4_request.
func (a *Auth) parseCredential(credential string) *Failure {
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || parts[0] == "" || parts[2] == "" || parts[3] == "" {
		return fail(KindIncompleteSignature, "Credential should be scoped to a valid region, service and date, as <key>/<YYYYMMDD>/<region>/<service>/aws4_request; got "+strconv.Quote(credential))
	}
	if parts[4] != scopeTerminator {
		return fail(KindIncompleteSignature, "Credential should be scoped with a valid terminator: 'aws4_request', not "+strconv.Quote(parts[4]))
	}
	if _, err := time.Parse("20060102", parts[1]); err != nil {
		return fail(KindIncompleteSignature, "Credential date "+strconv.Quote(parts[1])+" is not in YYYYMMDD format")
	}
	a.AccessKeyID, a.Date, a.Region, a.Service = parts[0], parts[1], parts[2], parts[3]
	return nil
}

// checkCommon validates what header and presigned requests share.
func (a *Auth) checkCommon() *Failure {
	if a.Date != a.AmzDate[:min(8, len(a.AmzDate))] {
		return fail(KindIncompleteSignature, "Date in Credential scope does not match YYYYMMDD from ISO-8601 version of date from HTTP: '"+a.Date+"' != '"+a.AmzDate[:min(8, len(a.AmzDate))]+"', from '"+a.AmzDate+"'.")
	}
	if !a.signs("host") {
		return fail(KindIncompleteSignature, "'Host' or ':authority' must be a 'SignedHeader' in the AWS Authorization.")
	}
	return nil
}

func (a *Auth) signs(header string) bool {
	for _, h := range a.SignedHeaders {
		if h == header {
			return true
		}
	}
	return false
}

func splitSignedHeaders(raw string) []string {
	var out []string
	for _, h := range strings.Split(raw, ";") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}
	return out
}
