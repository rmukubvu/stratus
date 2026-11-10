package rds

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/store/bbolt"
)

// fakeRunner records engine lifecycle calls and hands out ports.
type fakeRunner struct {
	mu         sync.Mutex
	started    []backing.Spec
	stopped    []string
	stopAll    []string
	running    map[string]bool
	nextPort   int
	startErr   error
	runningErr error
	stopAllErr error
	exported   map[string]backing.Data
	exportErr  error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{running: map[string]bool{}, nextPort: 40000}
}

func (f *fakeRunner) Start(_ context.Context, spec backing.Spec) (backing.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return backing.Endpoint{}, f.startErr
	}
	f.started = append(f.started, spec)
	port := spec.HostPort
	if port == 0 {
		f.nextPort++
		port = f.nextPort
	}
	f.running[spec.Name] = true
	return backing.Endpoint{Address: "127.0.0.1", Port: port}, nil
}

func (f *fakeRunner) Stop(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	delete(f.running, name)
	return nil
}

func (f *fakeRunner) Running(_ context.Context, _, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runningErr != nil {
		return false, f.runningErr
	}
	return f.running[name], nil
}

func (f *fakeRunner) Export(_ context.Context, _, name string, data backing.Data, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exportErr != nil {
		return f.exportErr
	}
	if !f.running[name] {
		return backing.ErrNotRunning
	}
	if f.exported == nil {
		f.exported = map[string]backing.Data{}
	}
	f.exported[name] = data
	return os.WriteFile(dst, []byte(name), 0o644)
}

func (f *fakeRunner) StopAll(_ context.Context, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopAllErr != nil {
		return f.stopAllErr
	}
	f.stopAll = append(f.stopAll, kind)
	f.running = map[string]bool{}
	return nil
}

func newTestService(t *testing.T, runner backing.Runner) *Service {
	t.Helper()
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { metadata.Close() })
	return NewService(Options{
		Metadata: metadata,
		Engines:  runner,
		Now:      func() time.Time { return time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC) },
	})
}

func call(t *testing.T, svc *Service, operation string, params url.Values) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(params.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	err := svc.Handle(rec, req, operation, "req-1")
	return rec, err
}

func mustCall(t *testing.T, svc *Service, operation string, params url.Values) string {
	t.Helper()
	rec, err := call(t, svc, operation, params)
	if err != nil {
		t.Fatalf("%s failed: %v", operation, err)
	}
	return rec.Body.String()
}

func createParams(identifier, engine string) url.Values {
	return url.Values{
		"DBInstanceIdentifier": {identifier},
		"Engine":               {engine},
		"MasterUsername":       {"app"},
		"MasterUserPassword":   {"secret"},
	}
}

type describedInstance struct {
	Identifier string `xml:"DBInstanceIdentifier"`
	Status     string `xml:"DBInstanceStatus"`
	Engine     string `xml:"Engine"`
	DBName     string `xml:"DBName"`
	Address    string `xml:"Endpoint>Address"`
	Port       int    `xml:"Endpoint>Port"`
}

func describe(t *testing.T, svc *Service, identifier string) []describedInstance {
	t.Helper()
	params := url.Values{}
	if identifier != "" {
		params.Set("DBInstanceIdentifier", identifier)
	}
	body := mustCall(t, svc, "DescribeDBInstances", params)
	var out struct {
		Instances []describedInstance `xml:"DescribeDBInstancesResult>DBInstances>DBInstance"`
	}
	if err := xml.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode describe: %v\n%s", err, body)
	}
	return out.Instances
}

func TestCreateDBInstanceStartsEngineAndReturnsItsEndpoint(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	params := createParams("app-db", "postgres")
	params.Set("DBName", "orders")
	body := mustCall(t, svc, "CreateDBInstance", params)

	if len(runner.started) != 1 {
		t.Fatalf("expected one engine start, got %d", len(runner.started))
	}
	spec := runner.started[0]
	if spec.Kind != "rds" || spec.Name != "app-db" || spec.Image != defaultPostgresImage || spec.Port != 5432 || spec.HostPort != 0 {
		t.Fatalf("unexpected engine spec: %+v", spec)
	}
	wantEnv := []string{"POSTGRES_USER=app", "POSTGRES_PASSWORD=secret", "POSTGRES_DB=orders", "PGDATA=/var/lib/postgresql/data"}
	if strings.Join(spec.Env, ",") != strings.Join(wantEnv, ",") {
		t.Fatalf("unexpected env %v", spec.Env)
	}
	for _, want := range []string{"<DBInstanceStatus>available</DBInstanceStatus>", "<Address>127.0.0.1</Address>", "<Port>40001</Port>", "<DBName>orders</DBName>", "<MasterUsername>app</MasterUsername>", "arn:aws:rds:us-east-1:000000000000:db:app-db"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret") {
		t.Fatal("response must not leak the master password")
	}

	instances := describe(t, svc, "app-db")
	if len(instances) != 1 || instances[0].Port != 40001 || instances[0].Status != "available" {
		t.Fatalf("describe should show the live endpoint: %+v", instances)
	}
}

func TestCreateDBInstanceDefaultsToPostgresDatabase(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	mustCall(t, svc, "CreateDBInstance", createParams("plain", "postgres"))

	if got := describe(t, svc, "plain")[0].DBName; got != "postgres" {
		t.Fatalf("postgres instances should default to the postgres database, got %q", got)
	}
}

func TestCreateDBInstanceConfiguresMySQLFamilyEngines(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	params := createParams("shop", "mysql")
	params.Set("DBName", "shop")
	mustCall(t, svc, "CreateDBInstance", params)
	root := createParams("admin-db", "mariadb")
	root.Set("MasterUsername", "root")
	mustCall(t, svc, "CreateDBInstance", root)

	mysql, mariadb := runner.started[0], runner.started[1]
	if mysql.Image != defaultMySQLImage || mysql.Port != 3306 {
		t.Fatalf("unexpected mysql spec %+v", mysql)
	}
	if strings.Join(mysql.Env, ",") != "MYSQL_ROOT_PASSWORD=secret,MYSQL_USER=app,MYSQL_PASSWORD=secret,MYSQL_DATABASE=shop" {
		t.Fatalf("unexpected mysql env %v", mysql.Env)
	}
	if strings.Join(mariadb.Env, ",") != "MARIADB_ROOT_PASSWORD=secret" {
		t.Fatalf("root as master user must not create a second user: %v", mariadb.Env)
	}
}

func TestCreateDBInstanceRejectsBadInput(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	cases := []struct {
		name   string
		params url.Values
		code   string
	}{
		{"missing identifier", url.Values{"Engine": {"postgres"}}, "ValidationError"},
		{"unknown engine", createParams("x", "oracle-ee"), "InvalidParameterValue"},
		{"missing username", url.Values{"DBInstanceIdentifier": {"x"}, "MasterUserPassword": {"p"}}, "InvalidParameterValue"},
		{"missing password", url.Values{"DBInstanceIdentifier": {"x"}, "MasterUsername": {"u"}}, "InvalidParameterValue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := call(t, svc, "CreateDBInstance", tc.params)
			var apiErr *apierror.Error
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
	if len(runner.started) != 0 {
		t.Fatal("invalid requests must not start engines")
	}
}

func TestCreateDBInstanceRejectsDuplicateIdentifier(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("dup", "postgres"))

	_, err := call(t, svc, "CreateDBInstance", createParams("dup", "postgres"))
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "DBInstanceAlreadyExists" {
		t.Fatalf("want DBInstanceAlreadyExists, got %v", err)
	}
	if len(runner.started) != 1 {
		t.Fatal("a duplicate must not replace the running engine")
	}
}

func TestCreateDBInstanceExplainsWhenDockerIsUnavailable(t *testing.T) {
	svc := newTestService(t, nil)

	_, err := call(t, svc, "CreateDBInstance", createParams("app-db", "postgres"))
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", err)
	}
	if !strings.Contains(apiErr.Message, "Docker") {
		t.Fatalf("error should point at Docker: %q", apiErr.Message)
	}
	if len(describe(t, svc, "")) != 0 {
		t.Fatal("a failed create must not leave a record behind")
	}
}

func TestCreateDBInstanceSurfacesMissingImage(t *testing.T) {
	runner := newFakeRunner()
	runner.startErr = &backing.ImageMissingError{Image: defaultPostgresImage}
	svc := newTestService(t, runner)

	_, err := call(t, svc, "CreateDBInstance", createParams("app-db", "postgres"))
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "docker pull "+defaultPostgresImage) {
		t.Fatalf("error should tell the caller which image to pull: %v", err)
	}
}

func TestDeleteDBInstanceStopsEngineAndForgetsInstance(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("app-db", "postgres"))

	body := mustCall(t, svc, "DeleteDBInstance", url.Values{"DBInstanceIdentifier": {"app-db"}})

	if !strings.Contains(body, "<DBInstanceStatus>deleting</DBInstanceStatus>") {
		t.Fatalf("delete should report deleting:\n%s", body)
	}
	if len(runner.stopped) != 1 || runner.stopped[0] != "app-db" {
		t.Fatalf("engine should be stopped: %v", runner.stopped)
	}
	_, err := call(t, svc, "DescribeDBInstances", url.Values{"DBInstanceIdentifier": {"app-db"}})
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "DBInstanceNotFound" {
		t.Fatalf("want DBInstanceNotFound, got %v", err)
	}
}

func TestDeleteDBInstanceSucceedsWithoutDocker(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("app-db", "postgres"))

	svc.engines = backing.Unavailable{}
	mustCall(t, svc, "DeleteDBInstance", url.Values{"DBInstanceIdentifier": {"app-db"}})

	if len(describe(t, svc, "")) != 0 {
		t.Fatal("instance should be forgotten even when Docker is gone")
	}
}

func TestResetStopsEveryEngine(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("a", "postgres"))
	mustCall(t, svc, "CreateDBInstance", createParams("b", "mysql"))

	if err := svc.Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(runner.stopAll) != 1 || runner.stopAll[0] != "rds" {
		t.Fatalf("reset should stop all rds engines: %v", runner.stopAll)
	}
	if svc.Service() != "rds" {
		t.Fatalf("backend name should match the catalog service, got %q", svc.Service())
	}
}

func TestResetSucceedsWithoutDockerButReportsOtherFailures(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	runner.stopAllErr = fmt.Errorf("%w: daemon down", backing.ErrUnavailable)
	if err := svc.Reset(context.Background()); err != nil {
		t.Fatalf("reset must not fail when Docker is unreachable: %v", err)
	}
	runner.stopAllErr = errors.New("permission denied")
	if err := svc.Reset(context.Background()); err == nil {
		t.Fatal("reset should report failures other than an unreachable Docker")
	}
}

func TestReconcileWithoutDockerMarksInstancesFailedAndReports(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("a", "postgres"))
	runner.runningErr = backing.ErrUnavailable

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{}); err == nil {
		t.Fatal("reconcile should report that the engine could not be inspected or started")
	}
	if got := describe(t, svc, "a")[0].Status; got != "failed" {
		t.Fatalf("instance should be marked failed, got %q", got)
	}
}

func TestReconcileWithoutRecordsNeverTouchesDocker(t *testing.T) {
	runner := newFakeRunner()
	runner.runningErr = backing.ErrUnavailable
	svc := newTestService(t, runner)

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{}); err != nil {
		t.Fatalf("nothing to reconcile must not need Docker: %v", err)
	}
}

func TestReconcileRestartsMissingEnginesOnTheirRecordedPort(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("a", "postgres"))
	mustCall(t, svc, "CreateDBInstance", createParams("b", "postgres"))
	before := describe(t, svc, "")

	// Simulate a restore or restart: records survive, containers do not.
	_ = runner.StopAll(context.Background(), "rds")
	delete(runner.running, "a")
	runner.running["b"] = true

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(runner.started) != 3 {
		t.Fatalf("only the missing engine should be restarted, got %d starts", len(runner.started))
	}
	restarted := runner.started[2]
	if restarted.Name != "a" || restarted.HostPort != before[0].Port {
		t.Fatalf("engine should come back on its recorded port %d: %+v", before[0].Port, restarted)
	}
	if !strings.Contains(strings.Join(restarted.Env, ","), "POSTGRES_PASSWORD=secret") {
		t.Fatal("engine should come back with the same credentials")
	}
	after := describe(t, svc, "")
	if after[0].Port != before[0].Port || after[0].Status != "available" {
		t.Fatalf("endpoint should be stable across reconcile: before %+v after %+v", before[0], after[0])
	}
}

func TestReconcileMarksInstancesWhoseEngineCannotStart(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("a", "postgres"))
	_ = runner.StopAll(context.Background(), "rds")
	runner.startErr = backing.ErrUnavailable

	err := svc.Reconcile(context.Background(), backing.ReconcileOptions{})
	if err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("reconcile should report the instance it could not start, got %v", err)
	}
	if got := describe(t, svc, "a")[0].Status; got != "failed" {
		t.Fatalf("instance should be marked failed, got %q", got)
	}
}

func TestDefaultEnginesHonourImageOverrides(t *testing.T) {
	env := map[string]string{EnvPostgresImage: "mirror/postgres:16", EnvMySQLImage: " "}
	engines := DefaultEngines(func(key string) string { return env[key] })

	pg, _ := engines.Lookup("POSTGRES")
	if pg.Image != "mirror/postgres:16" {
		t.Fatalf("override ignored: %q", pg.Image)
	}
	aurora, _ := engines.Lookup("aurora-postgresql")
	if aurora.Image != "mirror/postgres:16" {
		t.Fatalf("aurora-postgresql should share the postgres image: %q", aurora.Image)
	}
	mysql, _ := engines.Lookup("mysql")
	if mysql.Image != defaultMySQLImage {
		t.Fatalf("blank override should keep the default: %q", mysql.Image)
	}
	if _, ok := engines.Lookup("oracle-ee"); ok {
		t.Fatal("unknown engines must not resolve")
	}
}

func TestCaptureExportsEveryRunningInstanceDataDirectory(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("orders", "postgres"))
	mustCall(t, svc, "CreateDBInstance", createParams("users", "mysql"))
	mustCall(t, svc, "CreateDBInstance", createParams("stopped", "mariadb"))
	delete(runner.running, "stopped")
	dir := t.TempDir()

	captured, err := svc.Capture(context.Background(), dir)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	if strings.Join(captured, ",") != "orders,users" {
		t.Fatalf("only running instances hold data to capture, got %v", captured)
	}
	if got := runner.exported["orders"]; got.Path != postgresDataDir || len(got.Exclude) != 1 {
		t.Fatalf("postgres should export PGDATA without its lock file: %+v", got)
	}
	if got := runner.exported["users"].Path; got != mysqlDataDir {
		t.Fatalf("mysql should export its data dir, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "orders.tar.gz")); err != nil {
		t.Fatalf("archive should be written per instance: %v", err)
	}
}

func TestCaptureFailsWhenAnExportFails(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("orders", "postgres"))
	runner.exportErr = errors.New("disk full")

	if _, err := svc.Capture(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failed export should fail the capture, got %v", err)
	}
}

func TestReconcileStartsEnginesFromCapturedData(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("orders", "postgres"))
	mustCall(t, svc, "CreateDBInstance", createParams("fresh", "postgres"))
	dir := t.TempDir()
	if _, err := svc.Capture(context.Background(), dir); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "fresh.tar.gz")); err != nil {
		t.Fatal(err)
	}
	_ = runner.StopAll(context.Background(), "rds")
	runner.started = nil

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{DataDir: dir}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	seeds := map[string]string{}
	for _, spec := range runner.started {
		seeds[spec.Name] = spec.Seed
	}
	if seeds["orders"] != filepath.Join(dir, "orders.tar.gz") {
		t.Fatalf("orders should start from its archive, got %q", seeds["orders"])
	}
	if seeds["fresh"] != "" {
		t.Fatalf("an instance without an archive should start empty, got %q", seeds["fresh"])
	}
}

func TestReconcileWithFreshEndpointsPublishesNewPorts(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateDBInstance", createParams("orders", "postgres"))
	before := describe(t, svc, "orders")[0]
	_ = runner.StopAll(context.Background(), "rds")

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{FreshEndpoints: true}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	restarted := runner.started[len(runner.started)-1]
	if restarted.HostPort != 0 {
		t.Fatalf("a seeded fork must not reuse the snapshot's port, asked for %d", restarted.HostPort)
	}
	after := describe(t, svc, "orders")[0]
	if after.Port == before.Port || after.Status != "available" {
		t.Fatalf("instance should report its new endpoint: before %+v after %+v", before, after)
	}
}
