package advise

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/control"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newServerWith(t, fakeFamily{}, nil)
}

func newServerWith(t *testing.T, f Family, runs RunSource) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	meta, err := bbolt.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	snaps, _ := control.NewFSSnapshotRepository(filepath.Join(dir, "snaps"))
	h := control.NewHandler(control.NewService(control.Options{
		Catalog: control.DefaultCatalog(), State: meta, Blobs: fsblob.New(filepath.Join(dir, "blobs")), Snapshots: snaps,
	}))
	reg, err := NewRegistry(f)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	svc := NewService(NewStoreRepository(meta), reg, func() time.Time { return t0 }, func() string { n++; return "adr-" + string(rune('0'+n)) })
	svc.SetProofs(everything, runs)
	RegisterRoutes(h, svc)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+control.PathPrefix+"/advise"+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestADecisionGoesFromQuestionsToAnADROverTheControlAPI(t *testing.T) {
	srv := newServer(t)
	status, body := do(t, srv, "GET", "/families", "")
	if status != 200 || !strings.Contains(body, `"id": "fake"`) || !strings.Contains(body, OpsCapacityID) {
		t.Fatalf("families: %d %s", status, body)
	}
	status, body = do(t, srv, "POST", "/decisions", `{"family":"fake","title":"Order events","answers":{"rate":20}}`)
	if status != 201 || !strings.Contains(body, `"id": "adr-1"`) || strings.Contains(body, `"id": "rate"`) {
		t.Fatalf("start: %d %s", status, body)
	}
	if status, body = do(t, srv, "POST", "/decisions/adr-1/decide", `{"option":"simple"}`); status != 409 || !strings.Contains(body, "DecisionNotAssessed") {
		t.Fatalf("decide before assess: %d %s", status, body)
	}
	if status, body = do(t, srv, "POST", "/decisions/adr-1/answers", `{"answers":{"strict":"maybe"}}`); status != 400 || !strings.Contains(body, `"hint": "answer strict with true or false"`) {
		t.Fatalf("bad answer: %d %s", status, body)
	}
	if status, body = do(t, srv, "POST", "/decisions/adr-1/assess", ""); status != 200 || !strings.Contains(body, `"recommended": "simple"`) {
		t.Fatalf("assess: %d %s", status, body)
	}
	if status, body = do(t, srv, "POST", "/decisions/adr-1/decide", `{"option":"simple","rationale":"it is simple"}`); status != 200 || !strings.Contains(body, `"status": "decided"`) {
		t.Fatalf("decide: %d %s", status, body)
	}
	status, body = do(t, srv, "GET", "/decisions/adr-1/adr", "")
	if status != 200 || !strings.Contains(body, "Chosen option: **Simple**, because it is simple.") {
		t.Fatalf("adr: %d %s", status, body)
	}
	if status, body = do(t, srv, "GET", "/decisions/adr-1/adr?format=json", ""); status != 200 || !strings.Contains(body, `"markdown": "# Order events`) {
		t.Fatalf("adr as json: %d %s", status, body)
	}
	status, body = do(t, srv, "GET", "/decisions", "")
	var list struct {
		Decisions []View `json:"decisions"`
	}
	if status != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list.Decisions) != 1 || list.Decisions[0].Chosen != "simple" {
		t.Fatalf("list: %d %s", status, body)
	}
	if status, body = do(t, srv, "GET", "/decisions/adr-9", ""); status != 404 || !strings.Contains(body, "DecisionNotFound") {
		t.Fatalf("missing: %d %s", status, body)
	}
	if status, body = do(t, srv, "POST", "/decisions", `{"family":"databases"}`); status != 400 || !strings.Contains(body, "valid families are: fake") {
		t.Fatalf("unknown family: %d %s", status, body)
	}
}

func TestProofsArePlannedAndRecordedOverTheControlAPI(t *testing.T) {
	runs := fakeRuns{"run-1": {ID: "run-1", Finished: true, DurationSeconds: 10, Requests: 1000, OK: 1000, PerSecond: 100}}
	srv := newServerWith(t, provingFamily{}, runs)
	do(t, srv, "POST", "/decisions", `{"family":"fake","answers":{"rate":100}}`)
	if status, body := do(t, srv, "POST", "/decisions/adr-1/proofs", ""); status != 409 || !strings.Contains(body, "DecisionNotAssessed") {
		t.Fatalf("plan before assess: %d %s", status, body)
	}
	do(t, srv, "POST", "/decisions/adr-1/assess", "")
	status, body := do(t, srv, "POST", "/decisions/adr-1/proofs", `{"options":["simple"],"duration_seconds":10}`)
	if status != 200 || !strings.Contains(body, `"provable": true`) || !strings.Contains(body, `"rate": 100`) || !strings.Contains(body, `"profile": "aws-default"`) {
		t.Fatalf("plan: %d %s", status, body)
	}
	if status, body = do(t, srv, "POST", "/decisions/adr-1/proofs", `{"scale":5}`); status != 400 || !strings.Contains(body, "InvalidProofScale") {
		t.Fatalf("bad scale: %d %s", status, body)
	}
	status, body = do(t, srv, "POST", "/decisions/adr-1/evidence", `{"option":"simple","run_id":"run-1","target_rate":100}`)
	if status != 200 || !strings.Contains(body, `"verdict": "held"`) {
		t.Fatalf("evidence: %d %s", status, body)
	}
	if status, body = do(t, srv, "GET", "/decisions/adr-1/adr", ""); !strings.Contains(body, "| held at 100/s |") {
		t.Fatalf("adr: %d %s", status, body)
	}
}
