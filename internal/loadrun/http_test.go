package loadrun_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/limits"
	"github.com/stratus/internal/loadrun"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

func newServer(t *testing.T) (*httptest.Server, *limits.Governor) {
	t.Helper()
	dir := t.TempDir()
	meta, err := bbolt.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	repo, _ := control.NewFSSnapshotRepository(filepath.Join(dir, "snaps"))
	h := control.NewHandler(control.NewService(control.Options{
		Catalog: control.DefaultCatalog(), State: meta, Blobs: fsblob.New(filepath.Join(dir, "blobs")), Snapshots: repo,
	}))
	g := limits.NewGovernor(control.DefaultCatalog().Names(), nil)
	loadrun.RegisterRoutes(h, loadrun.NewRecorder(g, chaos.NewEngine(control.DefaultCatalog().Names(), nil), nil))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, g
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+control.PathPrefix+path, reader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestRunEndpoints(t *testing.T) {
	srv, g := newServer(t)
	if code, _ := call(t, srv, http.MethodGet, "/runs/current", ""); code != http.StatusNotFound {
		t.Fatalf("GET current with no run: %d", code)
	}
	code, body := call(t, srv, http.MethodPost, "/runs", `{"name":"smoke","limits":{"profile":"strict"},"chaos":{"seed":7,"rules":[{"service":"sqs","action":"throttle","probability":0.1}]}}`)
	if code != http.StatusCreated || body["id"] != "run-1" || !g.Enabled() {
		t.Fatalf("POST: %d %v", code, body)
	}
	if code, _ := call(t, srv, http.MethodPost, "/runs", ""); code != http.StatusConflict {
		t.Fatalf("second POST: %d", code)
	}
	if code, body := call(t, srv, http.MethodGet, "/runs/current", ""); code != http.StatusOK || body["status"] != "running" {
		t.Fatalf("GET current: %d %v", code, body)
	}
	code, body = call(t, srv, http.MethodPost, "/runs/run-1/stop", "")
	if code != http.StatusOK || body["status"] != "finished" || g.Enabled() {
		t.Fatalf("stop: %d %v (limits enabled %v)", code, body, g.Enabled())
	}
	if code, body := call(t, srv, http.MethodGet, "/runs", ""); code != http.StatusOK || len(body["runs"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/runs/latest", ""); code != http.StatusOK || body["id"] != "run-1" {
		t.Fatalf("latest: %d %v", code, body)
	}
	if code, _ := call(t, srv, http.MethodPost, "/runs", `{"limits":{"profile":"fast"}}`); code != http.StatusBadRequest {
		t.Fatalf("bad limits: %d", code)
	}
}
