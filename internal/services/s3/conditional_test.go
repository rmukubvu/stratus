package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestIfNoneMatchStarOnlyCreates(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	create := request{method: http.MethodPut, body: "first", headers: map[string]string{"If-None-Match": "*"}}

	mustSend(t, svc, existingBucket, "checkpoint", create)
	expectRefusal(t, svc, existingBucket, "checkpoint", request{method: http.MethodPut, body: "second", headers: map[string]string{"If-None-Match": "*"}}, "PreconditionFailed")
	if got := mustSend(t, svc, existingBucket, "checkpoint", request{method: http.MethodGet}).Body.String(); got != "first" {
		t.Fatalf("object was overwritten: %q", got)
	}
	expectRefusal(t, svc, existingBucket, "checkpoint", request{method: http.MethodPut, body: "x", headers: map[string]string{"If-None-Match": `"abc"`}}, "NotImplemented")
}

func TestIfNoneMatchAfterDeleteMarkerCreatesAgain(t *testing.T) {
	svc, _ := newLockService(t)
	create := request{method: http.MethodPut, body: "v", headers: map[string]string{"If-None-Match": "*"}}
	mustSend(t, svc, lockBucket, "checkpoint", create)
	mustSend(t, svc, lockBucket, "checkpoint", request{method: http.MethodDelete})
	mustSend(t, svc, lockBucket, "checkpoint", create)
}

func TestIfMatchOverwritesOnlyTheExpectedObject(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	expectRefusal(t, svc, existingBucket, "doc", request{method: http.MethodPut, body: "x", headers: map[string]string{"If-Match": `"abc"`}}, "NoSuchKey")

	etag := mustSend(t, svc, existingBucket, "doc", request{method: http.MethodPut, body: "one"}).Header().Get("ETag")
	expectRefusal(t, svc, existingBucket, "doc", request{method: http.MethodPut, body: "two", headers: map[string]string{"If-Match": `"0000"`}}, "PreconditionFailed")
	mustSend(t, svc, existingBucket, "doc", request{method: http.MethodPut, body: "two", headers: map[string]string{"If-Match": etag}})
	if got := mustSend(t, svc, existingBucket, "doc", request{method: http.MethodGet}).Body.String(); got != "two" {
		t.Fatalf("body = %q", got)
	}
}

func TestConcurrentIfNoneMatchWritesHaveOneWinner(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	const writers = 16
	var wg sync.WaitGroup
	results := make(chan bool, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPut, "/"+existingBucket+"/lease", strings.NewReader("claim"))
			req.Header.Set("If-None-Match", "*")
			results <- svc.Handle(httptest.NewRecorder(), req, existingBucket, "lease") == nil
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d writers succeeded, want exactly 1", wins)
	}
}

func TestConditionalCompleteMultipartUpload(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	mustPutObject(t, svc, existingBucket, "big", "exists")

	rec := mustSend(t, svc, existingBucket, "big", request{method: http.MethodPost, target: "?uploads"})
	uploadID := between(rec.Body.String(), "<UploadId>", "</UploadId>")
	part := mustSend(t, svc, existingBucket, "big", request{method: http.MethodPut, target: "?partNumber=1&uploadId=" + uploadID, body: "data"})
	complete := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + part.Header().Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	expectRefusal(t, svc, existingBucket, "big", request{method: http.MethodPost, target: "?uploadId=" + uploadID, body: complete, headers: map[string]string{"If-None-Match": "*"}}, "PreconditionFailed")
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	j := strings.Index(s, end)
	if i < 0 || j < i {
		return ""
	}
	return s[i+len(start) : j]
}
