package sigv4

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Test vectors from the AWS SigV4 test suite and the S3 SigV4 examples in
// the AWS documentation: if these verify, canonicalization matches AWS.

const (
	suiteKey    = "AKIDEXAMPLE"
	suiteSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	s3Key       = "AKIAIOSFODNN7EXAMPLE"
	s3Secret    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

type staticStore map[string]Credential

func (s staticStore) Lookup(id string) (Credential, bool) {
	c, ok := s[id]
	return c, ok
}

var vectorStore = staticStore{
	suiteKey: {SecretAccessKey: suiteSecret},
	s3Key:    {SecretAccessKey: s3Secret},
}

func at(value string) func() time.Time {
	t, err := time.Parse(amzDateFormat, value)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return t }
}

// parseRaw reads a request the way net/http's server does, so RequestURI,
// Host and headers are exactly what a handler would see.
func parseRaw(t *testing.T, raw string) *http.Request {
	t.Helper()
	raw = strings.ReplaceAll(raw, "\n", "\r\n")
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return r
}

func TestAWSSuiteVectors(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"get-vanilla", "GET / HTTP/1.1\nHost: example.amazonaws.com\nX-Amz-Date: 20150830T123600Z\n" +
			"Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31\n\n"},
		{"get-vanilla-query-order-key-case", "GET /?Param2=value2&Param1=value1 HTTP/1.1\nHost: example.amazonaws.com\nX-Amz-Date: 20150830T123600Z\n" +
			"Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500\n\n"},
		{"post-x-www-form-urlencoded", "POST / HTTP/1.1\nContent-Type: application/x-www-form-urlencoded\nHost: example.amazonaws.com\nX-Amz-Date: 20150830T123600Z\nContent-Length: 13\n" +
			"Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a\n\nParam1=value1"},
	}
	v := NewVerifier(vectorStore, at("20150830T123600Z"))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := parseRaw(t, tc.raw)
			if _, f, err := v.Verify(r, Rules{}); err != nil || f != nil {
				t.Fatalf("Verify: failure=%v err=%v\ncanonical:\n%s", f, err, canonicalOf(f))
			}
		})
	}
}

func TestPostBodyIsReplayedAfterHashing(t *testing.T) {
	r := parseRaw(t, "POST / HTTP/1.1\nContent-Type: application/x-www-form-urlencoded\nHost: example.amazonaws.com\nX-Amz-Date: 20150830T123600Z\nContent-Length: 13\n"+
		"Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a\n\nParam1=value1")
	if _, f, err := NewVerifier(vectorStore, at("20150830T123600Z")).Verify(r, Rules{}); err != nil || f != nil {
		t.Fatalf("Verify: %v %v", f, err)
	}
	body, _ := io.ReadAll(r.Body)
	if string(body) != "Param1=value1" {
		t.Fatalf("handler would see body %q", body)
	}
}

func TestS3DocumentationVectors(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"GetObject", "GET /test.txt HTTP/1.1\nHost: examplebucket.s3.amazonaws.com\nRange: bytes=0-9\n" +
			"x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\nx-amz-date: 20130524T000000Z\n" +
			"Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41\n\n"},
		{"PutObject", "PUT /test$file.text HTTP/1.1\nHost: examplebucket.s3.amazonaws.com\nDate: Fri, 24 May 2013 00:00:00 GMT\n" +
			"x-amz-date: 20130524T000000Z\nx-amz-storage-class: REDUCED_REDUNDANCY\nContent-Length: 21\n" +
			"x-amz-content-sha256: 44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072\n" +
			"Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class,Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd\n\nWelcome to Amazon S3."},
		{"GetBucketLifecycle", "GET /?lifecycle HTTP/1.1\nHost: examplebucket.s3.amazonaws.com\n" +
			"x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\nx-amz-date: 20130524T000000Z\n" +
			"Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date,Signature=fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543\n\n"},
		{"ListObjects", "GET /?max-keys=2&prefix=J HTTP/1.1\nHost: examplebucket.s3.amazonaws.com\n" +
			"x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\nx-amz-date: 20130524T000000Z\n" +
			"Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date,Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7\n\n"},
		{"PresignedGetObject", "GET /test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404 HTTP/1.1\n" +
			"Host: examplebucket.s3.amazonaws.com\n\n"},
	}
	v := NewVerifier(vectorStore, at("20130524T000500Z"))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := parseRaw(t, tc.raw)
			if _, f, err := v.Verify(r, Rules{S3: true}); err != nil || f != nil {
				t.Fatalf("Verify: failure=%v err=%v\ncanonical:\n%s", f, err, canonicalOf(f))
			}
		})
	}
}

// chunkedExample is the STREAMING-AWS4-HMAC-SHA256-PAYLOAD upload from the
// S3 documentation: 65536 + 1024 bytes of 'a' in two signed chunks.
func chunkedExample(chunk1Sig string) string {
	body := "10000;chunk-signature=" + chunk1Sig + "\r\n" + strings.Repeat("a", 65536) + "\r\n" +
		"400;chunk-signature=0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497\r\n" + strings.Repeat("a", 1024) + "\r\n" +
		"0;chunk-signature=b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9\r\n\r\n"
	head := "PUT /examplebucket/chunkObject.txt HTTP/1.1\r\nHost: s3.amazonaws.com\r\nx-amz-date: 20130524T000000Z\r\n" +
		"x-amz-storage-class: REDUCED_REDUNDANCY\r\nx-amz-content-sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD\r\n" +
		"Content-Encoding: aws-chunked\r\nx-amz-decoded-content-length: 66560\r\nContent-Length: 66824\r\n" +
		"Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,Signature=4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9\r\n\r\n"
	return head + body
}

func readExact(t *testing.T, raw string) *http.Request {
	t.Helper()
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return r
}

func TestS3ChunkedUploadVector(t *testing.T) {
	raw := chunkedExample("ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648")
	r := readExact(t, raw)
	if _, f, err := NewVerifier(vectorStore, at("20130524T000000Z")).Verify(r, Rules{S3: true}); err != nil || f != nil {
		t.Fatalf("Verify: failure=%+v err=%v", f, err)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(string(body), "10000;chunk-signature=") || len(body) != 66824 {
		t.Fatalf("the handler should get the aws-chunked body unchanged, got %d bytes", len(body))
	}
}

func TestS3ChunkedUploadRejectsATamperedChunk(t *testing.T) {
	raw := chunkedExample("ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648")
	raw = strings.Replace(raw, strings.Repeat("a", 1024)+"\r\n0;", strings.Repeat("a", 1023)+"b\r\n0;", 1)
	r := readExact(t, raw)
	_, f, err := NewVerifier(vectorStore, at("20130524T000000Z")).Verify(r, Rules{S3: true})
	if err != nil || f == nil || f.Kind != KindSignatureMismatch || !strings.Contains(f.Message, "chunk 2") {
		t.Fatalf("want a chunk 2 signature mismatch, got %+v %v", f, err)
	}
	body, _ := io.ReadAll(r.Body)
	if len(body) != 66824 {
		t.Fatalf("audit mode needs the full body replayed, got %d bytes", len(body))
	}
}

func TestS3ChunkedUploadRejectsAWrongChunkSignature(t *testing.T) {
	raw := chunkedExample(strings.Repeat("0", 64))
	_, f, _ := NewVerifier(vectorStore, at("20130524T000000Z")).Verify(readExact(t, raw), Rules{S3: true})
	if f == nil || f.Kind != KindSignatureMismatch || !strings.Contains(f.Message, "chunk 1") {
		t.Fatalf("want a chunk 1 signature mismatch, got %+v", f)
	}
}

func canonicalOf(f *Failure) string {
	if f == nil {
		return ""
	}
	return f.CanonicalRequest + "\n---\n" + f.StringToSign
}
