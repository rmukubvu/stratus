package control_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratus/internal/control"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

// env wires the control API to real stores, exercising the adapters together.
type env struct {
	t     *testing.T
	meta  *bbolt.Store
	blobs *fsblob.Store
	srv   *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	meta, err := bbolt.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("open meta: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	blobs := fsblob.New(filepath.Join(dir, "blobs"))
	repo, err := control.NewFSSnapshotRepository(filepath.Join(dir, "snapshots"))
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	svc := control.NewService(control.Options{
		Catalog:   control.DefaultCatalog(),
		State:     meta,
		Blobs:     blobs,
		Snapshots: repo,
	})
	srv := httptest.NewServer(control.NewHandler(svc))
	t.Cleanup(srv.Close)
	return &env{t: t, meta: meta, blobs: blobs, srv: srv}
}

func (e *env) do(method, path, body string) (int, map[string]any) {
	e.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+control.PathPrefix+path, reader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return resp.StatusCode, out
}

func (e *env) put(bucket, key, val string) {
	e.t.Helper()
	if err := e.meta.Put(bucket, key, []byte(val)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) get(bucket, key string) string {
	e.t.Helper()
	v, err := e.meta.Get(bucket, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(v)
}

func (e *env) putBlob(ns, key, body string) {
	e.t.Helper()
	if _, err := e.blobs.Put(ns, key, strings.NewReader(body)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) blob(ns, key string) string {
	f, err := e.blobs.Open(ns, key)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return string(b)
}

func errorCode(body map[string]any) string {
	errObj, _ := body["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func TestHTTPHealthAndReady(t *testing.T) {
	e := newEnv(t)
	status, body := e.do(http.MethodGet, "/health", "")
	if status != http.StatusOK || body["status"] != "ok" || body["ready"] != true {
		t.Fatalf("health: %d %v", status, body)
	}
	status, body = e.do(http.MethodGet, "/ready", "")
	if status != http.StatusOK || body["ready"] != true {
		t.Fatalf("ready: %d %v", status, body)
	}
}

func TestHTTPSnapshotRestoreRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.put("sqs-queues", "orders", "v1")
	e.putBlob("s3/assets", "logo.png", "v1")

	status, body := e.do(http.MethodPost, "/snapshots", `{"name":"baseline"}`)
	if status != http.StatusCreated || body["id"] != "baseline" {
		t.Fatalf("create: %d %v", status, body)
	}

	// A test run dirties the environment.
	e.put("sqs-queues", "orders", "v2")
	e.put("dynamodb-tables", "users", "x")
	e.putBlob("s3/assets", "logo.png", "v2")

	status, body = e.do(http.MethodPost, "/snapshots/baseline/restore", "")
	if status != http.StatusOK {
		t.Fatalf("restore: %d %v", status, body)
	}
	if got := e.get("sqs-queues", "orders"); got != "v1" {
		t.Fatalf("metadata not restored: %q", got)
	}
	if got := e.get("dynamodb-tables", "users"); got != "" {
		t.Fatalf("post-snapshot state survived restore: %q", got)
	}
	if got := e.blob("s3/assets", "logo.png"); got != "v1" {
		t.Fatalf("blob not restored: %q", got)
	}

	status, body = e.do(http.MethodGet, "/snapshots", "")
	items, _ := body["items"].([]any)
	if status != http.StatusOK || len(items) != 1 {
		t.Fatalf("list: %d %v", status, body)
	}

	status, _ = e.do(http.MethodDelete, "/snapshots/baseline", "")
	if status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	status, body = e.do(http.MethodGet, "/snapshots/baseline", "")
	if status != http.StatusNotFound || errorCode(body) != "SnapshotNotFound" {
		t.Fatalf("get deleted: %d %v", status, body)
	}
}

func TestHTTPResetSingleServiceAndAll(t *testing.T) {
	e := newEnv(t)
	e.put("sqs-queues", "orders", "v1")
	e.put("s3-buckets", "assets", "v1")
	e.putBlob("s3/assets", "logo.png", "v1")

	status, body := e.do(http.MethodPost, "/reset", `{"services":["s3"]}`)
	if status != http.StatusOK {
		t.Fatalf("reset s3: %d %v", status, body)
	}
	if e.get("s3-buckets", "assets") != "" || e.blob("s3/assets", "logo.png") != "" {
		t.Fatal("s3 state survived reset")
	}
	if e.get("sqs-queues", "orders") != "v1" {
		t.Fatal("sqs state should survive an s3 reset")
	}

	status, _ = e.do(http.MethodPost, "/reset", "")
	if status != http.StatusOK || e.get("sqs-queues", "orders") != "" {
		t.Fatalf("reset all: %d", status)
	}
}

func TestHTTPErrorsAreStructured(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{http.MethodPost, "/reset", `{"services":["nope"]}`, http.StatusBadRequest, "UnknownService"},
		{http.MethodPost, "/reset", `{"servicez":[]}`, http.StatusBadRequest, "InvalidRequestBody"},
		{http.MethodPost, "/snapshots", `{"name":"Bad Name"}`, http.StatusBadRequest, "InvalidSnapshotName"},
		{http.MethodPost, "/snapshots/missing/restore", "", http.StatusNotFound, "SnapshotNotFound"},
		{http.MethodGet, "/does-not-exist", "", http.StatusNotFound, "UnknownControlEndpoint"},
		{http.MethodPut, "/reset", "", http.StatusNotFound, "UnknownControlEndpoint"},
	}
	for _, tc := range cases {
		status, body := e.do(tc.method, tc.path, tc.body)
		if status != tc.status || errorCode(body) != tc.code {
			t.Errorf("%s %s: got %d %v, want %d %s", tc.method, tc.path, status, body, tc.status, tc.code)
		}
		if errObj, _ := body["error"].(map[string]any); errObj["hint"] == "" || errObj["hint"] == nil {
			t.Errorf("%s %s: missing hint", tc.method, tc.path)
		}
	}
}

func TestMatches(t *testing.T) {
	for path, want := range map[string]bool{
		"/_stratus/control/v1":         true,
		"/_stratus/control/v1/health":  true,
		"/_stratus/control/v10/health": false,
		"/_stratus/operator/bootstrap": false,
		"/_stratus/health":             false,
	} {
		if got := control.Matches(path); got != want {
			t.Errorf("Matches(%q) = %v, want %v", path, got, want)
		}
	}
}
