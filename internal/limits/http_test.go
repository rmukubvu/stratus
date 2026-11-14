package limits_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratus/internal/control"
	"github.com/stratus/internal/limits"
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
	svc := control.NewService(control.Options{
		Catalog: control.DefaultCatalog(), State: meta, Blobs: fsblob.New(filepath.Join(dir, "blobs")), Snapshots: repo,
	})
	g := limits.NewGovernor(control.DefaultCatalog().Names(), nil)
	h := control.NewHandler(svc)
	limits.RegisterRoutes(h, g)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, g
}

func errCode(body map[string]any) any {
	if e, ok := body["error"].(map[string]any); ok {
		return e["code"]
	}
	return nil
}

func call(t *testing.T, srv *httptest.Server, method, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+control.PathPrefix+"/limits", reader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLimitsEndpoints(t *testing.T) {
	srv, g := newServer(t)

	if code, body := call(t, srv, http.MethodGet, ""); code != http.StatusNotFound || errCode(body) != "NoLimitsProfile" {
		t.Fatalf("GET with no profile: %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodPut, `{"rates":[{"service":"nope","per_second":1}]}`); code != http.StatusBadRequest || errCode(body) != "InvalidRateRule" {
		t.Fatalf("PUT invalid: %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodPut, `{"scael":2}`); code != http.StatusBadRequest {
		t.Fatalf("PUT with a typo: %d %v", code, body)
	}

	code, body := call(t, srv, http.MethodPut, "")
	if code != http.StatusOK || body["profile"] != "aws-default" || !g.Enabled() {
		t.Fatalf("PUT empty: %d %v", code, body)
	}
	code, body = call(t, srv, http.MethodPut, `{"profile":"strict","scale":0.5}`)
	if code != http.StatusOK || body["profile"] != "strict" || body["scale"] != 0.005 {
		t.Fatalf("PUT strict: %d %v", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, ""); code != http.StatusOK || body["profile"] != "strict" {
		t.Fatalf("GET: %d %v", code, body)
	}
	if code, _ := call(t, srv, http.MethodDelete, ""); code != http.StatusOK || g.Enabled() {
		t.Fatalf("DELETE: %d", code)
	}
}
