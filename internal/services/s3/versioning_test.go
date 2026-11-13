package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const lockBucket = "audit"

// testClock is a settable clock for retention tests.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newLockService(t *testing.T) (*Service, *testClock) {
	t.Helper()
	svc := newTestService(t)
	clock := &testClock{now: lockNow}
	svc.now = clock.Now
	req := httptest.NewRequest(http.MethodPut, "/"+lockBucket, nil)
	req.Header.Set(headerBucketLock, "true")
	if _, apiErr := do(t, svc, req, lockBucket, ""); apiErr != nil {
		t.Fatalf("create lock bucket: %v", apiErr)
	}
	return svc, clock
}

type request struct {
	method, target, body string
	headers              map[string]string
}

// send issues a request against bucket/key; target may carry a query string.
func send(t *testing.T, svc *Service, bucket, key string, req request) (*httptest.ResponseRecorder, string) {
	t.Helper()
	path := "/" + bucket
	if key != "" {
		path += "/" + key
	}
	r := httptest.NewRequest(req.method, path+req.target, strings.NewReader(req.body))
	for k, v := range req.headers {
		r.Header.Set(k, v)
	}
	rec, apiErr := do(t, svc, r, bucket, key)
	if apiErr != nil {
		return rec, apiErr.Code
	}
	return rec, ""
}

func mustSend(t *testing.T, svc *Service, bucket, key string, req request) *httptest.ResponseRecorder {
	t.Helper()
	rec, code := send(t, svc, bucket, key, req)
	if code != "" {
		t.Fatalf("%s %s/%s%s failed: %s", req.method, bucket, key, req.target, code)
	}
	return rec
}

func expectRefusal(t *testing.T, svc *Service, bucket, key string, req request, want string) {
	t.Helper()
	if _, code := send(t, svc, bucket, key, req); code != want {
		t.Fatalf("%s %s/%s%s: got %q, want %q", req.method, bucket, key, req.target, code, want)
	}
}

func putLocked(t *testing.T, svc *Service, key, body string, headers map[string]string) string {
	t.Helper()
	rec := mustSend(t, svc, lockBucket, key, request{method: http.MethodPut, body: body, headers: headers})
	version := rec.Header().Get("X-Amz-Version-Id")
	if version == "" {
		t.Fatal("PutObject on a lock bucket returned no version ID")
	}
	return version
}

func retainHeaders(mode string, until time.Time) map[string]string {
	return map[string]string{headerLockMode: mode, headerLockRetainUntil: until.Format(time.RFC3339)}
}

func TestCreateBucketWithObjectLockEnablesVersioning(t *testing.T) {
	svc, _ := newLockService(t)
	rec := mustSend(t, svc, lockBucket, "", request{method: http.MethodGet, target: "?object-lock"})
	if !strings.Contains(rec.Body.String(), "<ObjectLockEnabled>Enabled</ObjectLockEnabled>") {
		t.Fatalf("object lock configuration: %s", rec.Body.String())
	}
	rec = mustSend(t, svc, lockBucket, "", request{method: http.MethodGet, target: "?versioning"})
	if !strings.Contains(rec.Body.String(), "<Status>Enabled</Status>") {
		t.Fatalf("versioning: %s", rec.Body.String())
	}
	expectRefusal(t, svc, lockBucket, "", request{method: http.MethodPut, target: "?versioning", body: `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`}, "InvalidBucketState")
}

func TestComplianceRetentionBlocksVersionDeletionUntilItExpires(t *testing.T) {
	svc, clock := newLockService(t)
	version := putLocked(t, svc, "record.json", "evidence", retainHeaders(lockModeCompliance, lockNow.Add(24*time.Hour)))

	head := mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodHead})
	if head.Header().Get(headerLockMode) != lockModeCompliance || head.Header().Get(headerLockRetainUntil) == "" {
		t.Fatalf("HEAD lock headers: %v", head.Header())
	}

	deleteVersion := request{method: http.MethodDelete, target: "?versionId=" + version, headers: map[string]string{headerBypassGovernance: "true"}}
	expectRefusal(t, svc, lockBucket, "record.json", deleteVersion, "AccessDenied")

	// A plain delete only adds a delete marker; the locked version survives.
	marker := mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodDelete})
	if marker.Header().Get("X-Amz-Delete-Marker") != "true" {
		t.Fatalf("plain delete should add a delete marker: %v", marker.Header())
	}
	rec, code := send(t, svc, lockBucket, "record.json", request{method: http.MethodGet})
	if code != "NoSuchKey" || rec.Header().Get("X-Amz-Delete-Marker") != "true" {
		t.Fatalf("GET after delete marker: %s %v", code, rec.Header())
	}
	body := mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodGet, target: "?versionId=" + version})
	if body.Body.String() != "evidence" {
		t.Fatalf("locked version body = %q", body.Body.String())
	}

	clock.Advance(25 * time.Hour)
	mustSend(t, svc, lockBucket, "record.json", deleteVersion)
	expectRefusal(t, svc, lockBucket, "record.json", request{method: http.MethodGet, target: "?versionId=" + version}, "NoSuchVersion")
}

func TestGovernanceRetentionYieldsToBypassHeader(t *testing.T) {
	svc, _ := newLockService(t)
	version := putLocked(t, svc, "record.json", "evidence", retainHeaders(lockModeGovernance, lockNow.Add(time.Hour)))

	expectRefusal(t, svc, lockBucket, "record.json", request{method: http.MethodDelete, target: "?versionId=" + version}, "AccessDenied")
	mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodDelete, target: "?versionId=" + version, headers: map[string]string{headerBypassGovernance: "true"}})
	expectRefusal(t, svc, lockBucket, "record.json", request{method: http.MethodGet}, "NoSuchKey")
}

func TestLegalHoldBlocksDeletionUntilReleased(t *testing.T) {
	svc, _ := newLockService(t)
	version := putLocked(t, svc, "record.json", "evidence", nil)

	rec := mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodGet, target: "?legal-hold"})
	if !strings.Contains(rec.Body.String(), "<Status>OFF</Status>") {
		t.Fatalf("default legal hold: %s", rec.Body.String())
	}
	mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodPut, target: "?legal-hold", body: `<LegalHold><Status>ON</Status></LegalHold>`})
	deleteVersion := request{method: http.MethodDelete, target: "?versionId=" + version, headers: map[string]string{headerBypassGovernance: "true"}}
	expectRefusal(t, svc, lockBucket, "record.json", deleteVersion, "AccessDenied")

	mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodPut, target: "?legal-hold", body: `<LegalHold><Status>OFF</Status></LegalHold>`})
	mustSend(t, svc, lockBucket, "record.json", deleteVersion)
}

func TestPutObjectRetentionRules(t *testing.T) {
	svc, _ := newLockService(t)
	until := lockNow.Add(24 * time.Hour)
	version := putLocked(t, svc, "record.json", "evidence", retainHeaders(lockModeGovernance, until))
	retain := func(mode string, at time.Time, bypass bool) request {
		req := request{method: http.MethodPut, target: "?retention&versionId=" + version,
			body: `<Retention><Mode>` + mode + `</Mode><RetainUntilDate>` + at.Format(time.RFC3339) + `</RetainUntilDate></Retention>`}
		if bypass {
			req.headers = map[string]string{headerBypassGovernance: "true"}
		}
		return req
	}

	expectRefusal(t, svc, lockBucket, "record.json", retain(lockModeGovernance, until.Add(-time.Hour), false), "AccessDenied")
	mustSend(t, svc, lockBucket, "record.json", retain(lockModeGovernance, until.Add(-time.Hour), true))
	mustSend(t, svc, lockBucket, "record.json", retain(lockModeCompliance, until, false))
	expectRefusal(t, svc, lockBucket, "record.json", retain(lockModeCompliance, until.Add(-time.Minute), true), "AccessDenied")
	expectRefusal(t, svc, lockBucket, "record.json", retain(lockModeCompliance, lockNow.Add(-time.Minute), true), "InvalidArgument")
	mustSend(t, svc, lockBucket, "record.json", retain(lockModeCompliance, until.Add(time.Hour), false))

	rec := mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodGet, target: "?retention"})
	var doc retentionXML
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.Mode != lockModeCompliance || doc.RetainUntilDate != until.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("GetObjectRetention = %s (%v)", rec.Body.String(), err)
	}
}

func TestDefaultRetentionAppliesToNewVersions(t *testing.T) {
	svc, _ := newLockService(t)
	mustSend(t, svc, lockBucket, "", request{method: http.MethodPut, target: "?object-lock",
		body: `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`})
	version := putLocked(t, svc, "record.json", "evidence", nil)

	rec := mustSend(t, svc, lockBucket, "record.json", request{method: http.MethodGet, target: "?retention"})
	if !strings.Contains(rec.Body.String(), "<Mode>GOVERNANCE</Mode>") || !strings.Contains(rec.Body.String(), lockNow.AddDate(0, 0, 1).Format(time.RFC3339)) {
		t.Fatalf("default retention: %s", rec.Body.String())
	}
	expectRefusal(t, svc, lockBucket, "record.json", request{method: http.MethodDelete, target: "?versionId=" + version}, "AccessDenied")
}

func TestObjectLockNeedsALockBucket(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	mustPutObject(t, svc, existingBucket, "k", "v")

	expectRefusal(t, svc, existingBucket, "k", request{method: http.MethodPut, headers: map[string]string{headerLockLegalHold: "ON"}}, "InvalidRequest")
	expectRefusal(t, svc, existingBucket, "k", request{method: http.MethodGet, target: "?retention"}, "InvalidRequest")
	expectRefusal(t, svc, existingBucket, "", request{method: http.MethodPut, target: "?object-lock", body: objectLockEnabledDoc}, "InvalidBucketState")
}

func TestVersioningKeepsEveryVersion(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	mustPutObject(t, svc, existingBucket, "doc.txt", "original") // written before versioning: the null version
	mustSend(t, svc, existingBucket, "", request{method: http.MethodPut, target: "?versioning", body: `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`})

	v2 := mustSend(t, svc, existingBucket, "doc.txt", request{method: http.MethodPut, body: "second"}).Header().Get("X-Amz-Version-Id")
	v3 := mustSend(t, svc, existingBucket, "doc.txt", request{method: http.MethodPut, body: "third"}).Header().Get("X-Amz-Version-Id")
	if v2 == "" || v3 == "" || v2 == v3 {
		t.Fatalf("version IDs %q, %q", v2, v3)
	}
	read := func(target string) string {
		rec := mustSend(t, svc, existingBucket, "doc.txt", request{method: http.MethodGet, target: target})
		body, _ := io.ReadAll(rec.Body)
		return string(body)
	}
	if read("") != "third" || read("?versionId="+v2) != "second" || read("?versionId=null") != "original" {
		t.Fatal("versions do not read back their own bodies")
	}

	var listed struct {
		Versions []struct {
			Key       string `xml:"Key"`
			VersionID string `xml:"VersionId"`
			IsLatest  bool   `xml:"IsLatest"`
		} `xml:"Version"`
	}
	rec := mustSend(t, svc, existingBucket, "", request{method: http.MethodGet, target: "?versions"})
	if err := xml.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Versions) != 3 || listed.Versions[0].VersionID != v3 || !listed.Versions[0].IsLatest || listed.Versions[1].IsLatest || listed.Versions[2].VersionID != "null" {
		t.Fatalf("ListObjectVersions = %+v", listed.Versions)
	}

	// Deleting the current version promotes the previous one.
	mustSend(t, svc, existingBucket, "doc.txt", request{method: http.MethodDelete, target: "?versionId=" + v3})
	if read("") != "second" {
		t.Fatal("previous version was not promoted")
	}
	expectRefusal(t, svc, existingBucket, "", request{method: http.MethodDelete}, "BucketNotEmpty")
}

func TestCopyObjectReadsASourceVersion(t *testing.T) {
	svc, _ := newLockService(t)
	v1 := putLocked(t, svc, "src", "one", nil)
	putLocked(t, svc, "src", "two", nil)
	mustSend(t, svc, lockBucket, "dst", request{method: http.MethodPut, headers: map[string]string{"X-Amz-Copy-Source": "/" + lockBucket + "/src?versionId=" + v1}})
	if got := mustSend(t, svc, lockBucket, "dst", request{method: http.MethodGet}).Body.String(); got != "one" {
		t.Fatalf("copied body = %q", got)
	}
}
