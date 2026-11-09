package elasticache

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{running: map[string]bool{}, nextPort: 50000}
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

type describedCluster struct {
	ID      string `xml:"CacheClusterId"`
	Status  string `xml:"CacheClusterStatus"`
	Engine  string `xml:"Engine"`
	Nodes   int    `xml:"NumCacheNodes"`
	Address string `xml:"CacheNodes>CacheNode>Endpoint>Address"`
	Port    int    `xml:"CacheNodes>CacheNode>Endpoint>Port"`
	Config  *struct {
		Port int `xml:"Port"`
	} `xml:"ConfigurationEndpoint"`
}

func describe(t *testing.T, svc *Service, id string) []describedCluster {
	t.Helper()
	params := url.Values{}
	if id != "" {
		params.Set("CacheClusterId", id)
	}
	body := mustCall(t, svc, "DescribeCacheClusters", params)
	var out struct {
		Clusters []describedCluster `xml:"DescribeCacheClustersResult>CacheClusters>CacheCluster"`
	}
	if err := xml.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode describe: %v\n%s", err, body)
	}
	return out.Clusters
}

func TestCreateCacheClusterStartsEngineAndReturnsNodeEndpoint(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	body := mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"cache-main"}, "Engine": {"redis"}, "NumCacheNodes": {"1"}})

	if len(runner.started) != 1 {
		t.Fatalf("expected one engine start, got %d", len(runner.started))
	}
	spec := runner.started[0]
	if spec.Kind != "elasticache" || spec.Name != "cache-main" || spec.Image != defaultRedisImage || spec.Port != 6379 || spec.Probe == nil {
		t.Fatalf("unexpected engine spec: %+v", spec)
	}
	for _, want := range []string{"<CacheClusterStatus>available</CacheClusterStatus>", "<CacheNodeId>0001</CacheNodeId>", "<Address>127.0.0.1</Address>", "<Port>50001</Port>", "arn:aws:elasticache:us-east-1:000000000000:cluster:cache-main"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "ConfigurationEndpoint") {
		t.Fatal("redis clusters have no configuration endpoint")
	}

	clusters := describe(t, svc, "cache-main")
	if len(clusters) != 1 || clusters[0].Port != 50001 || clusters[0].Status != "available" || clusters[0].Nodes != 1 {
		t.Fatalf("describe should show the live node endpoint: %+v", clusters)
	}
}

func TestCreateCacheClusterSupportsValkeyAndMemcached(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"kv"}, "Engine": {"VALKEY"}})
	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"mc"}, "Engine": {"memcached"}, "NumCacheNodes": {"3"}})

	if runner.started[0].Image != defaultRedisImage {
		t.Fatalf("valkey should use the redis-compatible image: %+v", runner.started[0])
	}
	if runner.started[1].Image != defaultMemcachedImage || runner.started[1].Port != 11211 {
		t.Fatalf("unexpected memcached spec: %+v", runner.started[1])
	}
	mc := describe(t, svc, "mc")[0]
	if mc.Config == nil || mc.Config.Port != mc.Port {
		t.Fatalf("memcached should expose a configuration endpoint on the node port: %+v", mc)
	}
	if mc.Nodes != 1 {
		t.Fatalf("stratus runs one node per cluster, got %d", mc.Nodes)
	}
}

func TestCreateCacheClusterRejectsBadInput(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	cases := []struct {
		name   string
		params url.Values
	}{
		{"missing id", url.Values{"Engine": {"redis"}}},
		{"zero nodes", url.Values{"CacheClusterId": {"x"}, "NumCacheNodes": {"0"}}},
		{"unknown engine", url.Values{"CacheClusterId": {"x"}, "Engine": {"hazelcast"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := call(t, svc, "CreateCacheCluster", tc.params)
			var apiErr *apierror.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "InvalidParameterValue" {
				t.Fatalf("want InvalidParameterValue, got %v", err)
			}
		})
	}
	if len(runner.started) != 0 {
		t.Fatal("invalid requests must not start engines")
	}
}

func TestCreateCacheClusterRejectsDuplicateID(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"dup"}})

	_, err := call(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"dup"}})
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "CacheClusterAlreadyExists" {
		t.Fatalf("want CacheClusterAlreadyExists, got %v", err)
	}
	if len(runner.started) != 1 {
		t.Fatal("a duplicate must not replace the running engine")
	}
}

func TestCreateCacheClusterExplainsWhenDockerIsUnavailable(t *testing.T) {
	svc := newTestService(t, nil)

	_, err := call(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"cache-main"}})
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable || !strings.Contains(apiErr.Message, "Docker") {
		t.Fatalf("want a 503 that points at Docker, got %v", err)
	}
	if len(describe(t, svc, "")) != 0 {
		t.Fatal("a failed create must not leave a record behind")
	}
}

func TestDeleteCacheClusterStopsEngineAndForgetsCluster(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"cache-main"}})

	body := mustCall(t, svc, "DeleteCacheCluster", url.Values{"CacheClusterId": {"cache-main"}})

	if !strings.Contains(body, "<CacheClusterStatus>deleting</CacheClusterStatus>") {
		t.Fatalf("delete should report deleting:\n%s", body)
	}
	if len(runner.stopped) != 1 || runner.stopped[0] != "cache-main" {
		t.Fatalf("engine should be stopped: %v", runner.stopped)
	}
	_, err := call(t, svc, "DescribeCacheClusters", url.Values{"CacheClusterId": {"cache-main"}})
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "CacheClusterNotFound" {
		t.Fatalf("want CacheClusterNotFound, got %v", err)
	}
}

func TestResetStopsEveryEngine(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"a"}})

	if err := svc.Reset(context.Background()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(runner.stopAll) != 1 || runner.stopAll[0] != "elasticache" || svc.Service() != "elasticache" {
		t.Fatalf("reset should stop all elasticache engines: %v", runner.stopAll)
	}
}

func TestResetSucceedsWithoutDocker(t *testing.T) {
	runner := newFakeRunner()
	runner.stopAllErr = backing.ErrUnavailable
	svc := newTestService(t, runner)

	if err := svc.Reset(context.Background()); err != nil {
		t.Fatalf("reset must not fail when Docker is unreachable: %v", err)
	}
}

func TestReconcileWithoutDockerMarksClustersFailedAndReports(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"a"}})
	runner.runningErr = backing.ErrUnavailable

	if err := svc.Reconcile(context.Background()); err == nil {
		t.Fatal("reconcile should report that the engine could not be inspected or started")
	}
	if got := describe(t, svc, "a")[0].Status; got != "failed" {
		t.Fatalf("cluster should be marked failed, got %q", got)
	}
}

func TestReconcileRestartsMissingEnginesOnTheirRecordedPort(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"a"}})
	before := describe(t, svc, "a")[0]
	_ = runner.StopAll(context.Background(), "elasticache")

	if err := svc.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(runner.started) != 2 || runner.started[1].HostPort != before.Port {
		t.Fatalf("engine should come back on port %d: %+v", before.Port, runner.started)
	}
	after := describe(t, svc, "a")[0]
	if after.Port != before.Port || after.Status != "available" {
		t.Fatalf("endpoint should be stable across reconcile: before %+v after %+v", before, after)
	}
}

func TestReconcileMarksClustersWhoseEngineCannotStart(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	mustCall(t, svc, "CreateCacheCluster", url.Values{"CacheClusterId": {"a"}})
	_ = runner.StopAll(context.Background(), "elasticache")
	runner.startErr = &backing.ImageMissingError{Image: defaultRedisImage}

	err := svc.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), defaultRedisImage) {
		t.Fatalf("reconcile should report why the engine could not start, got %v", err)
	}
	if got := describe(t, svc, "a")[0].Status; got != "failed" {
		t.Fatalf("cluster should be marked failed, got %q", got)
	}
}

func TestDefaultEnginesHonourImageOverrides(t *testing.T) {
	env := map[string]string{EnvRedisImage: "mirror/valkey:8"}
	engines := DefaultEngines(func(key string) string { return env[key] })

	redis, _ := engines.Lookup("redis")
	valkey, _ := engines.Lookup("valkey")
	if redis.Image != "mirror/valkey:8" || valkey.Image != "mirror/valkey:8" {
		t.Fatalf("redis and valkey should share the overridden image: %q %q", redis.Image, valkey.Image)
	}
	memcached, _ := engines.Lookup("memcached")
	if memcached.Image != defaultMemcachedImage {
		t.Fatalf("memcached should keep its default: %q", memcached.Image)
	}
}
