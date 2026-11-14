package authn

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/services/iam"
	"github.com/stratus/internal/services/sts"
	"github.com/stratus/internal/sigv4"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return now }

// sign signs r as a non-S3 SDK would, for service in us-east-1.
func sign(r *http.Request, key, secret, service string) {
	amzDate := now.Format("20060102T150405Z")
	r.Header.Set("X-Amz-Date", amzDate)
	auth := &sigv4.Auth{AccessKeyID: key, Date: amzDate[:8], Region: "us-east-1", Service: service, SignedHeaders: []string{"host", "x-amz-date"}, AmzDate: amzDate}
	sts := sigv4.StringToSign(auth, sigv4.CanonicalRequest(r, auth, sigv4.PathDoubleEncoded, sigv4.EmptyPayloadHash))
	sig := sigv4.Signature(sigv4.SigningKey(secret, auth.Date, auth.Region, auth.Service), sts)
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=host;x-amz-date, Signature=%s", sigv4.Algorithm, key, auth.Scope(), sig))
}

func queryMeta(service, op string) httpapi.RequestMetadata {
	return httpapi.RequestMetadata{RequestID: "req-1", Classification: httpapi.Classification{Protocol: httpapi.ProtocolQuery, Service: service, Operation: op}}
}

func newAuthenticator(mode Mode) *Authenticator {
	return New(mode, Directory{}, nil, clock)
}

func getRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "http://localhost:4566/?Action=ListQueues", nil)
}

func apiError(t *testing.T, err error) *apierror.Error {
	t.Helper()
	var e *apierror.Error
	if !errors.As(err, &e) {
		t.Fatalf("want an *apierror.Error, got %v", err)
	}
	return e
}

func TestOffAcceptsAnything(t *testing.T) {
	a := newAuthenticator(Off)
	if err := a.Authenticate(getRequest(), queryMeta("sqs", "ListQueues")); err != nil {
		t.Fatalf("off mode refused: %v", err)
	}
	if st := a.Status(); st.Verified+st.Failed+st.Skipped != 0 {
		t.Fatalf("off mode should not count anything: %+v", st)
	}
}

func TestStrictAcceptsAGoodSignature(t *testing.T) {
	a := newAuthenticator(Strict)
	r := getRequest()
	sign(r, "test", "test", "sqs")
	if err := a.Authenticate(r, queryMeta("sqs", "ListQueues")); err != nil {
		t.Fatalf("strict refused a good signature: %v", err)
	}
	if st := a.Status(); st.Verified != 1 || st.Failed != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestStrictRefusesWithTheServiceErrorCode(t *testing.T) {
	cases := []struct {
		name     string
		protocol httpapi.Protocol
		service  string
		code     string
		status   int
	}{
		{"query", httpapi.ProtocolQuery, "sqs", "SignatureDoesNotMatch", 403},
		{"json", httpapi.ProtocolJSON, "dynamodb", "InvalidSignatureException", 400},
		{"rest-json", httpapi.ProtocolREST, "lambda", "InvalidSignatureException", 403},
		{"ec2", httpapi.ProtocolQuery, "ec2", "AuthFailure", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAuthenticator(Strict)
			r := getRequest()
			sign(r, "test", "not-the-secret", tc.service)
			md := httpapi.RequestMetadata{RequestID: "req-1", Classification: httpapi.Classification{Protocol: tc.protocol, Service: tc.service, Operation: "Op"}}
			e := apiError(t, a.Authenticate(r, md))
			if e.Code != tc.code || e.StatusCode != tc.status {
				t.Fatalf("want %d %s, got %d %s", tc.status, tc.code, e.StatusCode, e.Code)
			}
			if tc.protocol != httpapi.ProtocolQuery || tc.service == "sqs" {
				if !strings.Contains(e.Message, "The Canonical String for this request should have been") {
					t.Fatalf("message should show the expected canonical request: %s", e.Message)
				}
			}
		})
	}
}

func TestStrictRefusesUnknownKeysAndUnsignedRequests(t *testing.T) {
	a := newAuthenticator(Strict)
	r := getRequest()
	sign(r, "AKIAUNKNOWN", "x", "sqs")
	if e := apiError(t, a.Authenticate(r, queryMeta("sqs", "ListQueues"))); e.Code != "InvalidClientTokenId" {
		t.Fatalf("unknown key: got %s", e.Code)
	}
	if e := apiError(t, a.Authenticate(getRequest(), queryMeta("sqs", "ListQueues"))); e.Code != "MissingAuthenticationToken" {
		t.Fatalf("unsigned: got %s", e.Code)
	}
}

func TestAuditRecordsButLetsThrough(t *testing.T) {
	a := newAuthenticator(Audit)
	r := getRequest()
	sign(r, "test", "wrong", "sqs")
	if err := a.Authenticate(r, queryMeta("sqs", "ListQueues")); err != nil {
		t.Fatalf("audit refused: %v", err)
	}
	st := a.Status()
	if st.Failed != 1 || len(st.Recent) != 1 || st.Recent[0].Refused || st.Recent[0].Code != "SignatureDoesNotMatch" || st.Recent[0].CanonicalRequest == "" {
		t.Fatalf("audit should record the failure: %+v", st)
	}
	if len(a.Refusals()) != 0 {
		t.Fatal("audit failures were not refused, so diagnostics should not attribute them")
	}
}

func TestRefusalsFeedDiagnostics(t *testing.T) {
	a := newAuthenticator(Strict)
	r := getRequest()
	sign(r, "test", "wrong", "sqs")
	_ = a.Authenticate(r, queryMeta("sqs", "ListQueues"))
	rec, ok := a.Refusals()["req-1"]
	if !ok || rec.Kind != string(sigv4.KindSignatureMismatch) || rec.Code != "SignatureDoesNotMatch" {
		t.Fatalf("refusals = %+v", a.Refusals())
	}
}

func TestSetStartsAFreshJournalAndClearKeepsIt(t *testing.T) {
	a := newAuthenticator(Strict)
	_ = a.Authenticate(getRequest(), queryMeta("sqs", "ListQueues"))
	if st := a.Clear(); st.Mode != Off || st.Failed != 1 {
		t.Fatalf("clear should keep the journal: %+v", st)
	}
	if st := a.Set(Audit); st.Mode != Audit || st.Failed != 0 || len(st.Recent) != 0 {
		t.Fatalf("set should reset the journal: %+v", st)
	}
}

func TestExemptions(t *testing.T) {
	a := newAuthenticator(Strict)
	cases := []struct {
		name string
		r    *http.Request
		md   httpapi.RequestMetadata
	}{
		{"health", httptest.NewRequest(http.MethodGet, "/_stratus/health", nil), httpapi.RequestMetadata{Classification: httpapi.Classification{Protocol: httpapi.ProtocolInternal, Service: "stratus"}}},
		{"deployed API", httptest.NewRequest(http.MethodGet, "/_aws/execute-api/abc/prod/orders", nil), httpapi.RequestMetadata{Classification: httpapi.Classification{Protocol: httpapi.ProtocolREST, Service: "apigatewayv2"}}},
		{"web identity", httptest.NewRequest(http.MethodPost, "/", nil), queryMeta("sts", "AssumeRoleWithWebIdentity")},
		{"cognito login", httptest.NewRequest(http.MethodPost, "/", nil), httpapi.RequestMetadata{Classification: httpapi.Classification{Protocol: httpapi.ProtocolJSON, Service: "cognitoidp", Operation: "InitiateAuth"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := a.Authenticate(tc.r, tc.md); err != nil {
				t.Fatalf("should be exempt: %v", err)
			}
		})
	}
	// A signed request to an anonymous operation is still verified.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	sign(r, "test", "wrong", "sts")
	if err := a.Authenticate(r, queryMeta("sts", "AssumeRoleWithWebIdentity")); err == nil {
		t.Fatal("a badly signed request is refused even for an operation that allows anonymous calls")
	}
}

func TestS3SignatureMismatchCarriesCanonicalRequestInXML(t *testing.T) {
	a := newAuthenticator(Strict)
	r := httptest.NewRequest(http.MethodGet, "http://localhost:4566/bucket/key", nil)
	r.Header.Set("X-Amz-Content-Sha256", sigv4.UnsignedPayload)
	amzDate := now.Format("20060102T150405Z")
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/"+amzDate[:8]+"/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+strings.Repeat("0", 64))
	md := httpapi.RequestMetadata{Classification: httpapi.Classification{Protocol: httpapi.ProtocolS3, Service: "s3"}}
	err := a.Authenticate(r, md)
	responder, ok := err.(httpapi.ErrorResponder)
	if !ok {
		t.Fatalf("want an ErrorResponder, got %T", err)
	}
	if e := apiError(t, err); e.Code != "SignatureDoesNotMatch" || e.StatusCode != 403 {
		t.Fatalf("got %+v", e)
	}
	w := httptest.NewRecorder()
	responder.WriteErrorResponse(w)
	var body s3SignatureBody
	if err := xml.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if body.AWSAccessKeyID != "test" || !strings.HasPrefix(body.CanonicalRequest, "GET\n/bucket/key\n") || body.StringToSign == "" || body.SignatureProvided == "" {
		t.Fatalf("S3 error body = %+v", body)
	}
}

func TestErrorTable(t *testing.T) {
	s3 := httpapi.Classification{Protocol: httpapi.ProtocolS3, Service: "s3"}
	query := httpapi.Classification{Protocol: httpapi.ProtocolQuery, Service: "sts"}
	jsonC := httpapi.Classification{Protocol: httpapi.ProtocolJSON, Service: "dynamodb"}
	cases := []struct {
		kind      sigv4.Kind
		presigned bool
		c         httpapi.Classification
		code      string
		status    int
	}{
		{sigv4.KindUnknownAccessKey, false, s3, "InvalidAccessKeyId", 403},
		{sigv4.KindUnknownAccessKey, false, query, "InvalidClientTokenId", 403},
		{sigv4.KindUnknownAccessKey, false, jsonC, "UnrecognizedClientException", 400},
		{sigv4.KindIncompleteSignature, false, query, "IncompleteSignature", 400},
		{sigv4.KindIncompleteSignature, false, s3, "AuthorizationHeaderMalformed", 400},
		{sigv4.KindIncompleteSignature, true, s3, "AuthorizationQueryParametersError", 400},
		{sigv4.KindRequestTimeSkewed, false, s3, "RequestTimeTooSkewed", 403},
		{sigv4.KindRequestTimeSkewed, false, jsonC, "InvalidSignatureException", 400},
		{sigv4.KindPresignExpired, true, s3, "AccessDenied", 403},
		{sigv4.KindPresignExpired, true, query, "RequestExpired", 400},
		{sigv4.KindExpiredToken, false, query, "ExpiredToken", 403},
		{sigv4.KindExpiredToken, false, jsonC, "ExpiredTokenException", 400},
		{sigv4.KindInvalidToken, false, s3, "InvalidToken", 400},
		{sigv4.KindContentHashMismatch, false, s3, "XAmzContentSHA256Mismatch", 400},
		{sigv4.KindMissingContentHash, false, s3, "InvalidRequest", 400},
		{sigv4.KindUnsignedHeaders, false, s3, "AccessDenied", 403},
		{sigv4.KindMalformedChunk, false, s3, "IncompleteBody", 400},
		{sigv4.KindUnsupportedAlgorithm, false, s3, "InvalidRequest", 400},
	}
	for _, tc := range cases {
		e := apiError(t, translate(&sigv4.Failure{Kind: tc.kind, Presigned: tc.presigned, Message: "detail"}, tc.c))
		if e.Code != tc.code || e.StatusCode != tc.status {
			t.Errorf("%s (%s, presigned=%v): want %d %s, got %d %s", tc.kind, tc.c.Protocol, tc.presigned, tc.status, tc.code, e.StatusCode, e.Code)
		}
	}
}

type fakeSessions map[string]sts.Session

func (f fakeSessions) Session(id string) (sts.Session, bool) { s, ok := f[id]; return s, ok }

type fakeKeys map[string]iam.AccessKey

func (f fakeKeys) Lookup(id string) (iam.AccessKey, bool) { k, ok := f[id]; return k, ok }

func TestDirectory(t *testing.T) {
	d := Directory{
		Sessions:   fakeSessions{"ASIA1": {SecretAccessKey: "s", SessionToken: "tok", Expiration: now}},
		Keys:       fakeKeys{"AKIA1": {SecretAccessKey: "u", Status: "Inactive"}},
		Static:     StaticCredentials{"AKIAFIXED": {AccessKeyID: "AKIAFIXED", SecretAccessKey: "fixed"}},
		RootSecret: "",
	}
	if c, ok := d.Lookup("ASIA1"); !ok || c.SessionToken != "tok" || c.Expires != now {
		t.Fatalf("session: %+v", c)
	}
	if c, ok := d.Lookup("AKIA1"); !ok || !c.Inactive || c.SecretAccessKey != "u" {
		t.Fatalf("iam key: %+v", c)
	}
	if c, ok := d.Lookup("AKIAFIXED"); !ok || c.SecretAccessKey != "fixed" || !c.IgnoreToken {
		t.Fatalf("static: %+v", c)
	}
	for _, root := range []string{"test", "111122223333"} {
		if c, ok := d.Lookup(root); !ok || c.SecretAccessKey != DefaultRootSecret {
			t.Fatalf("root %s: %+v", root, c)
		}
	}
	d.RootSecret = "custom"
	if c, _ := d.Lookup("test"); c.SecretAccessKey != "custom" {
		t.Fatalf("root secret should be configurable: %+v", c)
	}
	if _, ok := d.Lookup("AKIANOBODY"); ok {
		t.Fatal("an unknown key should not resolve")
	}
}

func TestParseStaticCredentials(t *testing.T) {
	creds, err := ParseStaticCredentials(" AKIA1:secret1 , AKIA2:secret2:111122223333,")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if creds["AKIA1"].SecretAccessKey != "secret1" || creds["AKIA2"].Account != "111122223333" {
		t.Fatalf("creds = %+v", creds)
	}
	if acct, ok := creds.AccountFor("AKIA2"); !ok || acct != "111122223333" {
		t.Fatalf("AccountFor = %s %v", acct, ok)
	}
	if _, ok := creds.AccountFor("AKIA1"); ok {
		t.Fatal("a key without an account uses the default account")
	}
	for _, bad := range []string{"AKIA1", "AKIA1:secret:12", ":secret", "a:b:c:d"} {
		_, err := ParseStaticCredentials(bad)
		if err == nil {
			t.Errorf("%q should not parse", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the secret: %v", err)
		}
	}
}

func TestParseMode(t *testing.T) {
	for raw, want := range map[string]Mode{"": Off, "off": Off, "AUDIT": Audit, " strict ": Strict} {
		if got, err := ParseMode(raw); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %s, %v", raw, got, err)
		}
	}
	if _, err := ParseMode("enforce"); err == nil {
		t.Error("enforce is an IAM mode, not a signature mode")
	}
}
