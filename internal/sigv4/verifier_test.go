package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	userKey     = "AKIAUSER"
	userSecret  = "user-secret"
	sessionKey  = "ASIASESSION"
	sessionSec  = "session-secret"
	sessionTok  = "session-token"
	rootKey     = "test"
	inactiveKey = "AKIAINACTIVE"
)

var signingTime = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func testStore() staticStore {
	return staticStore{
		userKey:     {SecretAccessKey: userSecret},
		sessionKey:  {SecretAccessKey: sessionSec, SessionToken: sessionTok, Expires: signingTime.Add(time.Hour)},
		rootKey:     {SecretAccessKey: "test", IgnoreToken: true},
		inactiveKey: {SecretAccessKey: "x", Inactive: true},
	}
}

func verifierAt(t time.Time) *Verifier {
	return NewVerifier(testStore(), func() time.Time { return t })
}

// signRequest signs r with header auth the way an SDK would: every
// x-amz-* header plus host, with payloadHash as x-amz-content-sha256 when
// it is not empty.
func signRequest(r *http.Request, key, secret, token string, now time.Time, payloadHash string, s3 bool) {
	amzDate := now.UTC().Format(amzDateFormat)
	r.Header.Set("X-Amz-Date", amzDate)
	if token != "" {
		r.Header.Set("X-Amz-Security-Token", token)
	}
	if payloadHash != "" {
		r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	signed := []string{"host"}
	for name := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") || lower == "content-type" {
			signed = append(signed, lower)
		}
	}
	sort.Strings(signed)
	service := "sqs"
	if s3 {
		service = "s3"
	}
	auth := &Auth{AccessKeyID: key, Date: amzDate[:8], Region: "us-east-1", Service: service, SignedHeaders: signed, AmzDate: amzDate}
	hash := payloadHash
	if hash == "" {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	style := PathDoubleEncoded
	if s3 {
		style = PathReencoded
	}
	sts := StringToSign(auth, CanonicalRequest(r, auth, style, hash))
	sig := Signature(SigningKey(secret, auth.Date, auth.Region, auth.Service), sts)
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", Algorithm, key, auth.Scope(), strings.Join(signed, ";"), sig))
}

// newRequest builds a server-side request: RequestURI set as net/http does.
func newRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	return r
}

func sqsRequest(body string) *http.Request {
	r := newRequest(http.MethodPost, "http://localhost:4566/", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func wantKind(t *testing.T, f *Failure, err error, kind Kind) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected I/O error: %v", err)
	}
	if f == nil {
		t.Fatalf("want %s, the request verified", kind)
	}
	if f.Kind != kind {
		t.Fatalf("want %s, got %s: %s", kind, f.Kind, f.Message)
	}
}

func TestVerifiesASignedQueryRequest(t *testing.T) {
	r := sqsRequest("Action=ListQueues")
	signRequest(r, userKey, userSecret, "", signingTime, "", false)
	res, f, err := verifierAt(signingTime).Verify(r, Rules{})
	if err != nil || f != nil {
		t.Fatalf("Verify: %v %v", f, err)
	}
	if res.Auth.AccessKeyID != userKey || res.Auth.Region != "us-east-1" {
		t.Fatalf("unexpected auth %+v", res.Auth)
	}
}

func TestSignatureMismatchExplainsWhatWasExpected(t *testing.T) {
	r := sqsRequest("Action=ListQueues")
	signRequest(r, userKey, "wrong-secret", "", signingTime, "", false)
	_, f, err := verifierAt(signingTime).Verify(r, Rules{})
	wantKind(t, f, err, KindSignatureMismatch)
	if f.AccessKeyID != userKey || f.SignatureProvided == "" ||
		!strings.HasPrefix(f.CanonicalRequest, "POST\n/\n\ncontent-type:application/x-www-form-urlencoded\nhost:localhost:4566\n") ||
		!strings.HasPrefix(f.StringToSign, Algorithm+"\n20261114T120000Z\n20261114/us-east-1/sqs/aws4_request\n") {
		t.Fatalf("the failure should carry what Stratus expected: %+v", f)
	}
}

func TestBodyChangedAfterSigningFails(t *testing.T) {
	r := sqsRequest("Action=ListQueues")
	signRequest(r, userKey, userSecret, "", signingTime, "", false)
	r.Body = io.NopCloser(strings.NewReader("Action=DeleteQueue"))
	_, f, err := verifierAt(signingTime).Verify(r, Rules{})
	wantKind(t, f, err, KindSignatureMismatch)
}

func TestHeaderChangedAfterSigningFails(t *testing.T) {
	r := sqsRequest("Action=ListQueues")
	signRequest(r, userKey, userSecret, "", signingTime, "", false)
	r.Header.Set("Content-Type", "application/json")
	_, f, err := verifierAt(signingTime).Verify(r, Rules{})
	wantKind(t, f, err, KindSignatureMismatch)
}

func TestCredentialChecks(t *testing.T) {
	cases := []struct {
		name       string
		key, sec   string
		token      string
		now        time.Time
		want       Kind
		wantPasses bool
	}{
		{name: "unknown key", key: "AKIANOBODY", sec: "x", now: signingTime, want: KindUnknownAccessKey},
		{name: "inactive key", key: inactiveKey, sec: "x", now: signingTime, want: KindUnknownAccessKey},
		{name: "session without token", key: sessionKey, sec: sessionSec, now: signingTime, want: KindInvalidToken},
		{name: "session with wrong token", key: sessionKey, sec: sessionSec, token: "other", now: signingTime, want: KindInvalidToken},
		{name: "session with its token", key: sessionKey, sec: sessionSec, token: sessionTok, now: signingTime, wantPasses: true},
		{name: "expired session", key: sessionKey, sec: sessionSec, token: sessionTok, now: signingTime.Add(2 * time.Hour), want: KindExpiredToken},
		{name: "long-term key with a token", key: userKey, sec: userSecret, token: "stray", now: signingTime, want: KindInvalidToken},
		{name: "root key ignores a stray token", key: rootKey, sec: "test", token: "stray", now: signingTime, wantPasses: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sqsRequest("Action=ListQueues")
			signRequest(r, tc.key, tc.sec, tc.token, tc.now, "", false)
			_, f, err := verifierAt(tc.now).Verify(r, Rules{})
			if tc.wantPasses {
				if f != nil || err != nil {
					t.Fatalf("want success, got %v %v", f, err)
				}
				return
			}
			wantKind(t, f, err, tc.want)
		})
	}
}

func TestClockSkew(t *testing.T) {
	for _, offset := range []time.Duration{-16 * time.Minute, 16 * time.Minute} {
		r := sqsRequest("Action=ListQueues")
		signRequest(r, userKey, userSecret, "", signingTime.Add(offset), "", false)
		_, f, err := verifierAt(signingTime).Verify(r, Rules{})
		wantKind(t, f, err, KindRequestTimeSkewed)
	}
	r := sqsRequest("Action=ListQueues")
	signRequest(r, userKey, userSecret, "", signingTime.Add(-14*time.Minute), "", false)
	if _, f, err := verifierAt(signingTime).Verify(r, Rules{}); f != nil || err != nil {
		t.Fatalf("14 minutes of skew is within AWS's window: %v %v", f, err)
	}
}

func TestMalformedAuthorization(t *testing.T) {
	cases := map[string]string{
		"no signature":       "AWS4-HMAC-SHA256 Credential=AKIAUSER/20261114/us-east-1/sqs/aws4_request, SignedHeaders=host;x-amz-date",
		"bad terminator":     "AWS4-HMAC-SHA256 Credential=AKIAUSER/20261114/us-east-1/sqs/aws5_request, SignedHeaders=host;x-amz-date, Signature=abc",
		"short scope":        "AWS4-HMAC-SHA256 Credential=AKIAUSER/20261114/us-east-1, SignedHeaders=host;x-amz-date, Signature=abc",
		"host not signed":    "AWS4-HMAC-SHA256 Credential=AKIAUSER/20261114/us-east-1/sqs/aws4_request, SignedHeaders=x-amz-date, Signature=abc",
		"scope date differs": "AWS4-HMAC-SHA256 Credential=AKIAUSER/20261113/us-east-1/sqs/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			r := sqsRequest("Action=ListQueues")
			r.Header.Set("X-Amz-Date", "20261114T120000Z")
			r.Header.Set("Authorization", header)
			_, f, err := verifierAt(signingTime).Verify(r, Rules{})
			wantKind(t, f, err, KindIncompleteSignature)
		})
	}
	t.Run("no date", func(t *testing.T) {
		r := sqsRequest("Action=ListQueues")
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAUSER/20261114/us-east-1/sqs/aws4_request, SignedHeaders=host, Signature=abc")
		_, f, err := verifierAt(signingTime).Verify(r, Rules{})
		wantKind(t, f, err, KindIncompleteSignature)
	})
}

func TestUnsignedAndOtherSchemes(t *testing.T) {
	_, f, err := verifierAt(signingTime).Verify(sqsRequest("Action=ListQueues"), Rules{})
	wantKind(t, f, err, KindMissingAuthentication)

	r := newRequest(http.MethodGet, "http://localhost/bucket/key", "")
	r.Header.Set("Authorization", "AWS AKIAUSER:c2lnbmF0dXJl")
	_, f, err = verifierAt(signingTime).Verify(r, Rules{S3: true})
	wantKind(t, f, err, KindUnsupportedAlgorithm)

	r = newRequest(http.MethodGet, "http://localhost/bucket/key", "")
	r.Header.Set("Authorization", "AWS4-ECDSA-P256-SHA256 Credential=AKIAUSER/20261114/s3/aws4_request, SignedHeaders=host, Signature=abc")
	_, f, err = verifierAt(signingTime).Verify(r, Rules{S3: true})
	wantKind(t, f, err, KindUnsupportedAlgorithm)
}

func s3Put(body string) *http.Request {
	return newRequest(http.MethodPut, "http://localhost:4566/bucket/my%20key.txt", body)
}

func TestS3PayloadRules(t *testing.T) {
	body := "hello"
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])

	t.Run("hashed payload verifies and replays", func(t *testing.T) {
		r := s3Put(body)
		signRequest(r, userKey, userSecret, "", signingTime, hash, true)
		if _, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true}); f != nil || err != nil {
			t.Fatalf("Verify: %v %v", f, err)
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Fatalf("handler would see %q", got)
		}
	})
	t.Run("body differs from the signed hash", func(t *testing.T) {
		r := s3Put("tampered")
		signRequest(r, userKey, userSecret, "", signingTime, hash, true)
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindContentHashMismatch)
		if got, _ := io.ReadAll(r.Body); string(got) != "tampered" {
			t.Fatalf("audit mode needs the body replayed, got %q", got)
		}
	})
	t.Run("unsigned payload accepts any body", func(t *testing.T) {
		r := s3Put("anything")
		signRequest(r, userKey, userSecret, "", signingTime, UnsignedPayload, true)
		if _, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true}); f != nil || err != nil {
			t.Fatalf("Verify: %v %v", f, err)
		}
	})
	t.Run("missing x-amz-content-sha256", func(t *testing.T) {
		r := s3Put(body)
		signRequest(r, userKey, userSecret, "", signingTime, "", true)
		r.Header.Del("X-Amz-Content-Sha256")
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindMissingContentHash)
	})
	t.Run("invalid x-amz-content-sha256", func(t *testing.T) {
		r := s3Put(body)
		signRequest(r, userKey, userSecret, "", signingTime, "not-a-hash", true)
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindInvalidContentHash)
	})
	t.Run("unsigned x-amz header", func(t *testing.T) {
		r := s3Put(body)
		signRequest(r, userKey, userSecret, "", signingTime, hash, true)
		r.Header.Set("X-Amz-Meta-Owner", "added-after-signing")
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindUnsignedHeaders)
		if !strings.Contains(f.Message, "x-amz-meta-owner") {
			t.Fatalf("message should name the header: %s", f.Message)
		}
	})
	t.Run("S3 encodes the path once, not twice", func(t *testing.T) {
		r := s3Put(body)
		signRequest(r, userKey, userSecret, "", signingTime, hash, false) // signed as a non-S3 service would
		r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "/sqs/", "/s3/", 1))
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindSignatureMismatch)
	})
}

// presign builds a presigned S3 GET URL the way SDKs do.
func presign(key, secret, token string, signedAt time.Time, expires int, target string) *http.Request {
	amzDate := signedAt.UTC().Format(amzDateFormat)
	auth := &Auth{AccessKeyID: key, Date: amzDate[:8], Region: "us-east-1", Service: "s3", SignedHeaders: []string{"host"}, AmzDate: amzDate, Presigned: true}
	query := "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=" + URIEncode(key+"/"+auth.Scope()) +
		"&X-Amz-Date=" + amzDate + fmt.Sprintf("&X-Amz-Expires=%d", expires) + "&X-Amz-SignedHeaders=host"
	if token != "" {
		query += "&X-Amz-Security-Token=" + URIEncode(token)
	}
	r := newRequest(http.MethodGet, target+"?"+query, "")
	sts := StringToSign(auth, CanonicalRequest(r, auth, PathReencoded, UnsignedPayload))
	sig := Signature(SigningKey(secret, auth.Date, auth.Region, auth.Service), sts)
	return newRequest(http.MethodGet, target+"?"+query+"&X-Amz-Signature="+sig, "")
}

// resignWithURI re-signs r over a canonical request whose URI line is uri,
// written out by hand rather than computed by the code under test.
func resignWithURI(t *testing.T, r *http.Request, secret, uri string) {
	t.Helper()
	auth, f := ParseHeader(r)
	if f != nil {
		t.Fatalf("ParseHeader: %v", f)
	}
	lines := strings.Split(CanonicalRequest(r, auth, PathReencoded, r.Header.Get("X-Amz-Content-Sha256")), "\n")
	lines[1] = uri
	sig := Signature(SigningKey(secret, auth.Date, auth.Region, auth.Service), StringToSign(auth, strings.Join(lines, "\n")))
	r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "Signature="+auth.Signature, "Signature="+sig, 1))
}

// S3 decodes the path and URI-encodes each segment once, whatever escaping
// the client put on the wire. Go's net/http sends ":", "+", "=", "(", ")",
// "@" and "," raw where an SDK escapes them, and AWS signs both forms as
// escaped. A client that signs the raw path is refused by AWS.
func TestS3PathIsDecodedThenEncodedOnce(t *testing.T) {
	const (
		raw     = "/bucket/ledger/10:00+01=(a)@b,c$.json"
		escaped = "/bucket/ledger/10%3A00%2B01%3D%28a%29%40b%2Cc%24.json"
	)
	for name, sent := range map[string]string{"sent raw": raw, "sent escaped": escaped} {
		t.Run(name, func(t *testing.T) {
			r := newRequest(http.MethodPut, "http://localhost:4566"+sent, "")
			signRequest(r, userKey, userSecret, "", signingTime, UnsignedPayload, true)
			resignWithURI(t, r, userSecret, escaped)
			if _, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true}); f != nil || err != nil {
				t.Fatalf("Verify: %v %v", f, err)
			}
		})
	}
	t.Run("signed over the raw path", func(t *testing.T) {
		r := newRequest(http.MethodPut, "http://localhost:4566"+raw, "")
		signRequest(r, userKey, userSecret, "", signingTime, UnsignedPayload, true)
		resignWithURI(t, r, userSecret, raw)
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindSignatureMismatch)
	})
}

func TestPresignedURLs(t *testing.T) {
	target := "http://localhost:4566/bucket/report.csv"
	t.Run("valid", func(t *testing.T) {
		r := presign(userKey, userSecret, "", signingTime, 300, target)
		res, f, err := verifierAt(signingTime.Add(4*time.Minute)).Verify(r, Rules{S3: true})
		if f != nil || err != nil || !res.Auth.Presigned {
			t.Fatalf("Verify: %v %v", f, err)
		}
	})
	t.Run("session credentials carry their token", func(t *testing.T) {
		r := presign(sessionKey, sessionSec, sessionTok, signingTime, 300, target)
		if _, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true}); f != nil || err != nil {
			t.Fatalf("Verify: %v %v", f, err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		r := presign(userKey, userSecret, "", signingTime, 300, target)
		_, f, err := verifierAt(signingTime.Add(6*time.Minute)).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindPresignExpired)
		if !f.Presigned {
			t.Fatal("failure should be marked presigned")
		}
	})
	t.Run("a long expiry outlives the header skew window", func(t *testing.T) {
		r := presign(userKey, userSecret, "", signingTime, 3600, target)
		if _, f, err := verifierAt(signingTime.Add(50*time.Minute)).Verify(r, Rules{S3: true}); f != nil || err != nil {
			t.Fatalf("Verify: %v %v", f, err)
		}
	})
	t.Run("not yet valid", func(t *testing.T) {
		r := presign(userKey, userSecret, "", signingTime.Add(time.Hour), 300, target)
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindRequestTimeSkewed)
	})
	t.Run("expiry over a week", func(t *testing.T) {
		r := presign(userKey, userSecret, "", signingTime, 604801, target)
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindIncompleteSignature)
	})
	t.Run("query changed after signing", func(t *testing.T) {
		r := presign(userKey, userSecret, "", signingTime, 300, target)
		r = newRequest(http.MethodGet, strings.Replace(r.RequestURI, "/report.csv", "/other.csv", 1), "")
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindSignatureMismatch)
	})
	t.Run("S3 requires X-Amz-Expires", func(t *testing.T) {
		r := presign(userKey, userSecret, "", signingTime, 300, target)
		r = newRequest(http.MethodGet, strings.Replace(r.RequestURI, "&X-Amz-Expires=300", "", 1), "")
		_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
		wantKind(t, f, err, KindIncompleteSignature)
	})
}

// chunkedBody signs chunks the way an SDK streaming upload does, starting
// from seed, optionally with a signed trailer.
func chunkedBody(key []byte, amzDate, scope, seed string, chunks []string, trailer string) string {
	var b strings.Builder
	prev := seed
	for _, c := range append(chunks, "") {
		sts := strings.Join([]string{chunkAlgorithm, amzDate, scope, prev, EmptyPayloadHash, hashHex([]byte(c))}, "\n")
		prev = Signature(key, sts)
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(c), prev)
		if c != "" {
			b.WriteString(c + "\r\n")
		}
	}
	if trailer == "" {
		b.WriteString("\r\n")
		return b.String()
	}
	b.WriteString(trailer + "\r\n")
	sts := strings.Join([]string{trailerAlgorithm, amzDate, scope, prev, hashHex([]byte(trailer + "\n"))}, "\n")
	b.WriteString("x-amz-trailer-signature:" + Signature(key, sts) + "\r\n\r\n")
	return b.String()
}

func TestStreamingSignedTrailer(t *testing.T) {
	amzDate := signingTime.Format(amzDateFormat)
	scope := amzDate[:8] + "/us-east-1/s3/aws4_request"
	key := SigningKey(userSecret, amzDate[:8], "us-east-1", "s3")

	build := func(trailer string) *http.Request {
		r := s3Put("")
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		r.Header.Set("X-Amz-Decoded-Content-Length", "11")
		signRequest(r, userKey, userSecret, "", signingTime, StreamingSignedPayloadTrailer, true)
		seed := r.Header.Get("Authorization")[strings.LastIndex(r.Header.Get("Authorization"), "=")+1:]
		body := chunkedBody(key, amzDate, scope, seed, []string{"hello", " world"}, "x-amz-checksum-crc32:DUoRhQ==")
		if trailer != "" {
			body = strings.Replace(body, "x-amz-checksum-crc32:DUoRhQ==", trailer, 1)
		}
		r.Body = io.NopCloser(strings.NewReader(body))
		return r
	}

	if _, f, err := verifierAt(signingTime).Verify(build(""), Rules{S3: true}); f != nil || err != nil {
		t.Fatalf("Verify: %v %v", f, err)
	}
	_, f, err := verifierAt(signingTime).Verify(build("x-amz-checksum-crc32:AAAAAA=="), Rules{S3: true})
	wantKind(t, f, err, KindSignatureMismatch)
	if !strings.Contains(f.Message, "trailer") {
		t.Fatalf("message should name the trailer: %s", f.Message)
	}
}

func TestStreamingRejectsMalformedBodies(t *testing.T) {
	amzDate := signingTime.Format(amzDateFormat)
	scope := amzDate[:8] + "/us-east-1/s3/aws4_request"
	key := SigningKey(userSecret, amzDate[:8], "us-east-1", "s3")
	cases := map[string]func(body string) string{
		"truncated":         func(b string) string { return b[:20] },
		"no signature":      func(b string) string { return strings.Replace(b, ";chunk-signature=", ";sig=", 1) },
		"bad size":          func(b string) string { return "zz" + b[1:] },
		"wrong decoded len": func(b string) string { return b },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := s3Put("")
			r.Header.Set("Content-Encoding", "aws-chunked")
			decoded := "5"
			if name == "wrong decoded len" {
				decoded = "6"
			}
			r.Header.Set("X-Amz-Decoded-Content-Length", decoded)
			signRequest(r, userKey, userSecret, "", signingTime, StreamingSignedPayload, true)
			seed := r.Header.Get("Authorization")[strings.LastIndex(r.Header.Get("Authorization"), "=")+1:]
			r.Body = io.NopCloser(strings.NewReader(mutate(chunkedBody(key, amzDate, scope, seed, []string{"hello"}, ""))))
			_, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true})
			wantKind(t, f, err, KindMalformedChunk)
		})
	}
}

func TestLargeBodiesSpoolToDisk(t *testing.T) {
	body := strings.Repeat("x", memorySpoolLimit+1024)
	sum := sha256.Sum256([]byte(body))
	r := s3Put(body)
	signRequest(r, userKey, userSecret, "", signingTime, hex.EncodeToString(sum[:]), true)
	if _, f, err := verifierAt(signingTime).Verify(r, Rules{S3: true}); f != nil || err != nil {
		t.Fatalf("Verify: %v %v", f, err)
	}
	got, _ := io.ReadAll(r.Body)
	if err := r.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(got) != len(body) || string(got) != body {
		t.Fatalf("replayed %d bytes, want %d", len(got), len(body))
	}
}
