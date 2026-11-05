package chaos_test

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
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

func newServer(t *testing.T) (*httptest.Server, *chaos.Engine) {
	t.Helper()
	dir := t.TempDir()
	meta, err := bbolt.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	repo, _ := control.NewFSSnapshotRepository(filepath.Join(dir, "snaps"))
	svc := control.NewService(control.Options{
		Catalog: control.DefaultCatalog(), State: meta, Blobs: fsblob.New(filepath.Join(dir, "blobs")), Snapshots: repo,
	})
	engine := chaos.NewEngine(control.DefaultCatalog().Names(), nil)
	h := control.NewHandler(svc)
	chaos.RegisterRoutes(h, engine)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, engine
}

func call(t *testing.T, srv *httptest.Server, method, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+control.PathPrefix+"/chaos", reader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestChaosLifecycleOverHTTP(t *testing.T) {
	srv, engine := newServer(t)

	status, body := call(t, srv, http.MethodPut, `{"seed": 9, "rules": [{"id": "t", "service": "sqs", "action": "throttle"}]}`)
	if status != http.StatusCreated || body["status"] != "running" || body["seed"] != float64(9) {
		t.Fatalf("start: %d %v", status, body)
	}

	engine.Decide(chaos.Request{RequestID: "abc", Service: "sqs", Operation: "SendMessage"})

	status, body = call(t, srv, http.MethodGet, "")
	if status != http.StatusOK || body["total_injected"] != float64(1) {
		t.Fatalf("report: %d %v", status, body)
	}
	hits := body["recent_hits"].([]any)
	if hits[0].(map[string]any)["request_id"] != "abc" {
		t.Fatalf("hits: %v", hits)
	}

	status, body = call(t, srv, http.MethodDelete, "")
	if status != http.StatusOK || body["status"] != "stopped" {
		t.Fatalf("stop: %d %v", status, body)
	}

	status, body = call(t, srv, http.MethodGet, "")
	if errObj, _ := body["error"].(map[string]any); status != http.StatusNotFound || errObj["code"] != "NoChaosExperiment" {
		t.Fatalf("after stop: %d %v", status, body)
	}
}

func TestChaosRejectsInvalidExperiment(t *testing.T) {
	srv, _ := newServer(t)
	for _, body := range []string{
		`{"rules": [{"service": "s4", "action": "throttle"}]}`,
		`{"rules": [], "seed": 1}`,
		`{"rulez": []}`,
	} {
		status, out := call(t, srv, http.MethodPut, body)
		errObj, _ := out["error"].(map[string]any)
		if status != http.StatusBadRequest || errObj["hint"] == nil {
			t.Errorf("%s: got %d %v", body, status, out)
		}
	}
}
