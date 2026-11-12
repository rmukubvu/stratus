package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const taggingDoc = `<Tagging><TagSet><Tag><Key>env</Key><Value>dev</Value></Tag></TagSet></Tagging>`

func TestPutBucketTaggingDoesNotCreateTheBucketAgain(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	put := httptest.NewRequest(http.MethodPut, "/"+existingBucket+"?tagging", strings.NewReader(taggingDoc))
	rec, apiErr := do(t, svc, put, existingBucket, "")
	if apiErr != nil || rec.Code != http.StatusNoContent {
		t.Fatalf("PutBucketTagging: %d %v", rec.Code, apiErr)
	}

	rec, apiErr = do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?tagging", nil), existingBucket, "")
	if apiErr != nil || rec.Body.String() != taggingDoc {
		t.Fatalf("GetBucketTagging: %q %v", rec.Body.String(), apiErr)
	}

	rec, apiErr = do(t, svc, httptest.NewRequest(http.MethodDelete, "/"+existingBucket+"?tagging", nil), existingBucket, "")
	if apiErr != nil || rec.Code != http.StatusNoContent {
		t.Fatalf("DeleteBucketTagging: %d %v", rec.Code, apiErr)
	}
	_, apiErr = do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?tagging", nil), existingBucket, "")
	assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchTagSet", "/"+existingBucket)
}

func TestBucketConfigurationsAndDefaults(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	rec, apiErr := do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?versioning", nil), existingBucket, "")
	if apiErr != nil || !strings.Contains(rec.Body.String(), "<VersioningConfiguration") {
		t.Fatalf("default versioning: %q %v", rec.Body.String(), apiErr)
	}
	_, apiErr = do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?cors", nil), existingBucket, "")
	assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchCORSConfiguration", "/"+existingBucket)

	doc := `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
	if _, apiErr := do(t, svc, httptest.NewRequest(http.MethodPut, "/"+existingBucket+"?versioning", strings.NewReader(doc)), existingBucket, ""); apiErr != nil {
		t.Fatal(apiErr)
	}
	rec, _ = do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?versioning", nil), existingBucket, "")
	if rec.Body.String() != doc {
		t.Fatalf("stored versioning: %q", rec.Body.String())
	}
}

func TestBucketSubresourceErrors(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	_, apiErr := do(t, svc, httptest.NewRequest(http.MethodPut, "/"+missingBucket+"?tagging", strings.NewReader(taggingDoc)), missingBucket, "")
	assertAPIError(t, apiErr, http.StatusNotFound, "NoSuchBucket", "/"+missingBucket)

	_, apiErr = do(t, svc, httptest.NewRequest(http.MethodPut, "/"+existingBucket+"?cors", strings.NewReader("<Wrong/>")), existingBucket, "")
	assertAPIError(t, apiErr, http.StatusBadRequest, "MalformedXML", "/"+existingBucket)

	dup := `<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>a</Key><Value>2</Value></Tag></TagSet></Tagging>`
	_, apiErr = do(t, svc, httptest.NewRequest(http.MethodPut, "/"+existingBucket+"?tagging", strings.NewReader(dup)), existingBucket, "")
	assertAPIError(t, apiErr, http.StatusBadRequest, "InvalidTag", "/"+existingBucket)

	_, apiErr = do(t, svc, httptest.NewRequest(http.MethodPut, "/"+existingBucket+"?acl", nil), existingBucket, "")
	assertAPIError(t, apiErr, http.StatusNotImplemented, "NotImplemented", "/"+existingBucket)
}

func TestGetBucketLocation(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	rec, apiErr := do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?location", nil), existingBucket, "")
	if apiErr != nil || !strings.Contains(rec.Body.String(), "<LocationConstraint") {
		t.Fatalf("GetBucketLocation: %q %v", rec.Body.String(), apiErr)
	}
}

func TestObjectTagging(t *testing.T) {
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)

	req := httptest.NewRequest(http.MethodPut, "/"+existingBucket+"/k", strings.NewReader("body"))
	req.Header.Set("X-Amz-Tagging", "env=dev&team=core")
	if _, apiErr := do(t, svc, req, existingBucket, "k"); apiErr != nil {
		t.Fatal(apiErr)
	}
	rec, _ := do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"/k", nil), existingBucket, "k")
	if rec.Header().Get("X-Amz-Tagging-Count") != "2" || rec.Body.String() != "body" {
		t.Fatalf("GetObject: count %q body %q", rec.Header().Get("X-Amz-Tagging-Count"), rec.Body.String())
	}

	put := httptest.NewRequest(http.MethodPut, "/"+existingBucket+"/k?tagging", strings.NewReader(taggingDoc))
	if _, apiErr := do(t, svc, put, existingBucket, "k"); apiErr != nil {
		t.Fatal(apiErr)
	}
	rec, _ = do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"/k?tagging", nil), existingBucket, "k")
	if !strings.Contains(rec.Body.String(), "<TagSet><Tag><Key>env</Key><Value>dev</Value></Tag></TagSet>") {
		t.Fatalf("GetObjectTagging: %q", rec.Body.String())
	}
	rec, _ = do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"/k", nil), existingBucket, "k")
	if rec.Body.String() != "body" {
		t.Fatalf("PutObjectTagging must not overwrite the object: %q", rec.Body.String())
	}

	_, apiErr := do(t, svc, httptest.NewRequest(http.MethodGet, "/"+existingBucket+"/missing?tagging", nil), existingBucket, "missing")
	if apiErr == nil || apiErr.Code != "NoSuchKey" {
		t.Fatalf("tagging a missing key: %v", apiErr)
	}
}
