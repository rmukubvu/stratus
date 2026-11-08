package s3

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

const (
	existingBucket = "present"
	missingBucket  = "absent"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	metadata, err := bbolt.Open(dir + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	return NewService(Options{Metadata: metadata, Blobs: fsblob.New(dir + "/blobs")})
}

// do sends a request through Handle and returns the API error it produced, if any.
func do(t *testing.T, svc *Service, req *http.Request, bucket, key string) (*httptest.ResponseRecorder, *apierror.Error) {
	t.Helper()
	rec := httptest.NewRecorder()
	err := svc.Handle(rec, req, bucket, key)
	if err == nil {
		return rec, nil
	}
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.Error, got %T: %v", err, err)
	}
	return rec, apiErr
}

func mustCreateBucket(t *testing.T, svc *Service, bucket string) {
	t.Helper()
	if _, apiErr := do(t, svc, httptest.NewRequest(http.MethodPut, "/"+bucket, nil), bucket, ""); apiErr != nil {
		t.Fatalf("create bucket %q: %v", bucket, apiErr)
	}
}

func mustPutObject(t *testing.T, svc *Service, bucket, key, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"/"+key, strings.NewReader(body))
	if _, apiErr := do(t, svc, req, bucket, key); apiErr != nil {
		t.Fatalf("put object %s/%s: %v", bucket, key, apiErr)
	}
}

func assertAPIError(t *testing.T, got *apierror.Error, status int, code, resource string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected %s, got success", code)
	}
	if got.StatusCode != status || got.Code != code || got.Resource != resource {
		t.Fatalf("expected %d %s on %s, got %d %s on %s", status, code, resource, got.StatusCode, got.Code, got.Resource)
	}
}

func TestObjectOperationsOnMissingBucketReturnNoSuchBucket(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	mustPutObject(t, svc, existingBucket, "src.txt", "hello")

	copyReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPut, "/"+missingBucket+"/k", nil)
		req.Header.Set("X-Amz-Copy-Source", "/"+existingBucket+"/src.txt")
		return req
	}

	cases := []struct {
		name string
		req  func() *http.Request
		key  string
	}{
		{"GetObject", func() *http.Request { return httptest.NewRequest(http.MethodGet, "/"+missingBucket+"/k", nil) }, "k"},
		{"HeadObject", func() *http.Request { return httptest.NewRequest(http.MethodHead, "/"+missingBucket+"/k", nil) }, "k"},
		{"DeleteObject", func() *http.Request { return httptest.NewRequest(http.MethodDelete, "/"+missingBucket+"/k", nil) }, "k"},
		{"PutObject", func() *http.Request {
			return httptest.NewRequest(http.MethodPut, "/"+missingBucket+"/k", strings.NewReader("x"))
		}, "k"},
		{"CopyObject destination", copyReq, "k"},
		{"CreateMultipartUpload", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/"+missingBucket+"/k?uploads", nil)
		}, "k"},
		{"UploadPart", func() *http.Request {
			return httptest.NewRequest(http.MethodPut, "/"+missingBucket+"/k?uploadId=u1&partNumber=1", strings.NewReader("x"))
		}, "k"},
		{"CompleteMultipartUpload", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/"+missingBucket+"/k?uploadId=u1", nil)
		}, "k"},
		{"AbortMultipartUpload", func() *http.Request {
			return httptest.NewRequest(http.MethodDelete, "/"+missingBucket+"/k?uploadId=u1", nil)
		}, "k"},
		{"ListObjectsV2", func() *http.Request { return httptest.NewRequest(http.MethodGet, "/"+missingBucket, nil) }, ""},
		{"HeadBucket", func() *http.Request { return httptest.NewRequest(http.MethodHead, "/"+missingBucket, nil) }, ""},
		{"DeleteBucket", func() *http.Request { return httptest.NewRequest(http.MethodDelete, "/"+missingBucket, nil) }, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, apiErr := do(t, svc, tc.req(), missingBucket, tc.key)
			assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchBucket", "/"+missingBucket)
		})
	}
}

func TestCopyObjectFromMissingSourceBucketReturnsNoSuchBucket(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	req := httptest.NewRequest(http.MethodPut, "/"+existingBucket+"/dst.txt", nil)
	req.Header.Set("X-Amz-Copy-Source", "/"+missingBucket+"/src.txt")

	_, apiErr := do(t, svc, req, existingBucket, "dst.txt")
	assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchBucket", "/"+missingBucket)
}

func TestReadObjectBytesFromMissingBucketReturnsNoSuchBucket(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.ReadObjectBytes(missingBucket, "k")

	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apierror.Error, got %T: %v", err, err)
	}
	assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchBucket", "/"+missingBucket)
}

func TestMissingKeyInExistingBucketStillReturnsNoSuchKey(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			_, apiErr := do(t, svc, httptest.NewRequest(method, "/"+existingBucket+"/nope", nil), existingBucket, "nope")
			assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchKey", "/"+existingBucket+"/nope")
		})
	}
}

func TestMissingUploadInExistingBucketStillReturnsNoSuchUpload(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	req := httptest.NewRequest(http.MethodPut, "/"+existingBucket+"/k?uploadId=u1&partNumber=1", strings.NewReader("x"))
	_, apiErr := do(t, svc, req, existingBucket, "k")
	assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchUpload", "/"+existingBucket)
}

func TestDeleteMissingKeyInExistingBucketSucceeds(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	rec, apiErr := do(t, svc, httptest.NewRequest(http.MethodDelete, "/"+existingBucket+"/nope", nil), existingBucket, "nope")
	if apiErr != nil {
		t.Fatalf("expected success, got %v", apiErr)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
}

func TestGetExistingObjectReturnsBody(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	mustPutObject(t, svc, existingBucket, "hello.txt", "world")

	rec, apiErr := do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"/hello.txt", nil), existingBucket, "hello.txt")
	if apiErr != nil {
		t.Fatalf("get object: %v", apiErr)
	}
	if got := rec.Body.String(); got != "world" {
		t.Fatalf("expected body %q, got %q", "world", got)
	}
}
