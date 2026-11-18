package s3

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func listV2(t *testing.T, svc *Service, bucket string, params url.Values) listObjectsV2Result {
	t.Helper()
	params.Set("list-type", "2")
	rec, apiErr := do(t, svc, httptest.NewRequest(http.MethodGet, "/"+bucket+"?"+params.Encode(), nil), bucket, "")
	if apiErr != nil {
		t.Fatalf("ListObjectsV2 %s: %v", params.Encode(), apiErr)
	}
	var out listObjectsV2Result
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	return out
}

func listV1(t *testing.T, svc *Service, bucket string, params url.Values) listObjectsResult {
	t.Helper()
	rec, apiErr := do(t, svc, httptest.NewRequest(http.MethodGet, "/"+bucket+"?"+params.Encode(), nil), bucket, "")
	if apiErr != nil {
		t.Fatalf("ListObjects %s: %v", params.Encode(), apiErr)
	}
	var out listObjectsResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<Marker>") {
		t.Fatalf("a ListObjects answer always carries Marker:\n%s", rec.Body.String())
	}
	return out
}

func keysOf(entries []objectEntry) []string {
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	return keys
}

func prefixesOf(entries []commonPrefix) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Prefix
	}
	return out
}

func bucketWith(t *testing.T, keys ...string) *Service {
	t.Helper()
	svc := newTestService(t)
	mustCreateBucket(t, svc, existingBucket)
	for _, k := range keys {
		mustPutObject(t, svc, existingBucket, k, "x")
	}
	return svc
}

// start-after leaves out the key it names, so "the first key after this
// one" is start-after with max-keys 1, and the last key has none after it.
func TestListObjectsV2StartAfterIsExclusive(t *testing.T) {
	svc := bucketWith(t,
		"ledger/main/checkpoints/00000000000000000001.json",
		"ledger/main/checkpoints/00000000000000000002.json",
		"ledger/main/entries/00000000000000000001.jsonl",
	)

	got := listV2(t, svc, existingBucket, url.Values{"prefix": {"ledger/"}, "max-keys": {"1"},
		"start-after": {"ledger/main/checkpoints/00000000000000000001.json"}})
	if keys := keysOf(got.Contents); !reflect.DeepEqual(keys, []string{"ledger/main/checkpoints/00000000000000000002.json"}) {
		t.Fatalf("first key after checkpoint 1: %v", keys)
	}
	if !got.IsTruncated || got.KeyCount != 1 || got.MaxKeys != 1 || got.NextContinuationToken == "" {
		t.Fatalf("one key of two left: truncated=%v keycount=%d maxkeys=%d token=%q", got.IsTruncated, got.KeyCount, got.MaxKeys, got.NextContinuationToken)
	}
	if got.StartAfter != "ledger/main/checkpoints/00000000000000000001.json" {
		t.Fatalf("StartAfter is echoed: %q", got.StartAfter)
	}

	last := listV2(t, svc, existingBucket, url.Values{"prefix": {"ledger/"}, "max-keys": {"1"},
		"start-after": {"ledger/main/entries/00000000000000000001.jsonl"}})
	if len(last.Contents) != 0 || last.IsTruncated || last.KeyCount != 0 || last.NextContinuationToken != "" {
		t.Fatalf("nothing after the last key: %+v", last)
	}
}

// Following NextContinuationToken visits every key once, in order, and the
// last page says it is the last.
func TestListObjectsV2PagesWithContinuationTokens(t *testing.T) {
	want := []string{"a", "b/1", "b/2", "c", "d", "e"}
	svc := bucketWith(t, "e", "c", "a", "b/2", "d", "b/1")

	var seen []string
	params := url.Values{"max-keys": {"2"}}
	for pages := 0; ; pages++ {
		if pages > len(want) {
			t.Fatalf("still paging after %d pages: %v", pages, seen)
		}
		page := listV2(t, svc, existingBucket, params)
		if page.KeyCount != len(page.Contents) || page.KeyCount > 2 {
			t.Fatalf("KeyCount %d for %d keys", page.KeyCount, len(page.Contents))
		}
		seen = append(seen, keysOf(page.Contents)...)
		if !page.IsTruncated {
			if page.NextContinuationToken != "" {
				t.Fatalf("the last page has a next token")
			}
			break
		}
		params = url.Values{"max-keys": {"2"}, "continuation-token": {page.NextContinuationToken},
			// Ignored once a continuation token is given.
			"start-after": {"z"}}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("paged keys %v, want %v", seen, want)
	}
}

// A delimiter rolls keys up into common prefixes, each counted once toward
// max-keys and listed once across pages.
func TestListObjectsV2DelimiterAcrossPages(t *testing.T) {
	svc := bucketWith(t, "photos/2025/a.jpg", "photos/2025/b.jpg", "photos/2026/c.jpg", "photos/readme.txt", "photos/zz/d.jpg", "other")

	all := listV2(t, svc, existingBucket, url.Values{"prefix": {"photos/"}, "delimiter": {"/"}})
	if p := prefixesOf(all.CommonPrefixes); !reflect.DeepEqual(p, []string{"photos/2025/", "photos/2026/", "photos/zz/"}) {
		t.Fatalf("common prefixes %v", p)
	}
	if k := keysOf(all.Contents); !reflect.DeepEqual(k, []string{"photos/readme.txt"}) {
		t.Fatalf("keys %v", k)
	}
	if all.KeyCount != 4 || all.IsTruncated || all.Delimiter != "/" {
		t.Fatalf("keycount=%d truncated=%v delimiter=%q", all.KeyCount, all.IsTruncated, all.Delimiter)
	}

	var entries []string
	params := url.Values{"prefix": {"photos/"}, "delimiter": {"/"}, "max-keys": {"1"}}
	for pages := 0; ; pages++ {
		if pages > 6 {
			t.Fatalf("still paging: %v", entries)
		}
		page := listV2(t, svc, existingBucket, params)
		entries = append(entries, prefixesOf(page.CommonPrefixes)...)
		entries = append(entries, keysOf(page.Contents)...)
		if !page.IsTruncated {
			break
		}
		params.Set("continuation-token", page.NextContinuationToken)
	}
	if want := []string{"photos/2025/", "photos/2026/", "photos/readme.txt", "photos/zz/"}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("paged entries %v, want %v", entries, want)
	}
}

// The original ListObjects starts after its marker, and gives NextMarker
// only with a delimiter, as S3 does.
func TestListObjectsMarker(t *testing.T) {
	svc := bucketWith(t, "a", "b", "c", "d/1", "d/2", "e")

	page := listV1(t, svc, existingBucket, url.Values{"marker": {"a"}, "max-keys": {"2"}})
	if k := keysOf(page.Contents); !reflect.DeepEqual(k, []string{"b", "c"}) {
		t.Fatalf("after marker a: %v", k)
	}
	if !page.IsTruncated || page.Marker != "a" || page.NextMarker != "" {
		t.Fatalf("truncated=%v marker=%q nextmarker=%q", page.IsTruncated, page.Marker, page.NextMarker)
	}

	page = listV1(t, svc, existingBucket, url.Values{"marker": {"c"}, "delimiter": {"/"}, "max-keys": {"1"}})
	if p := prefixesOf(page.CommonPrefixes); !reflect.DeepEqual(p, []string{"d/"}) || page.NextMarker != "d/" || !page.IsTruncated {
		t.Fatalf("after c with a delimiter: prefixes %v nextmarker %q truncated %v", p, page.NextMarker, page.IsTruncated)
	}
	page = listV1(t, svc, existingBucket, url.Values{"marker": {page.NextMarker}, "delimiter": {"/"}, "max-keys": {"1"}})
	if k := keysOf(page.Contents); !reflect.DeepEqual(k, []string{"e"}) || page.IsTruncated {
		t.Fatalf("after d/: %v truncated %v", k, page.IsTruncated)
	}
}

func TestListObjectsMaxKeys(t *testing.T) {
	svc := bucketWith(t, "a", "b")

	if page := listV2(t, svc, existingBucket, url.Values{"max-keys": {"5000"}}); page.MaxKeys != 1000 || len(page.Contents) != 2 || page.IsTruncated {
		t.Fatalf("max-keys above 1000: %+v", page)
	}
	if page := listV2(t, svc, existingBucket, url.Values{"max-keys": {"0"}}); len(page.Contents) != 0 || page.IsTruncated || page.NextContinuationToken != "" {
		t.Fatalf("max-keys 0: %+v", page)
	}
	for _, bad := range []string{"-1", "ten", "99999999999"} {
		req := httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?list-type=2&max-keys="+url.QueryEscape(bad), nil)
		_, apiErr := do(t, svc, req, existingBucket, "")
		assertAPIError(t, apiErr, http.StatusBadRequest, "InvalidArgument", "/"+existingBucket)
	}
}

func TestListObjectsV2RefusesAForeignContinuationToken(t *testing.T) {
	svc := bucketWith(t, "a")
	for _, token := range []string{"not base64!", "aGVsbG8=", ""} {
		req := httptest.NewRequest(http.MethodGet, "/"+existingBucket+"?list-type=2&continuation-token="+url.QueryEscape(token), nil)
		_, apiErr := do(t, svc, req, existingBucket, "")
		assertAPIError(t, apiErr, http.StatusBadRequest, "InvalidArgument", "/"+existingBucket)
	}
}
