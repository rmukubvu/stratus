package kafka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// fakeRunner records broker lifecycle calls.
type fakeRunner struct {
	mu       sync.Mutex
	started  []backing.Spec
	stopped  []string
	running  map[string]bool
	startErr error
	exported map[string]backing.Data
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{running: map[string]bool{}, exported: map[string]backing.Data{}}
}

func (f *fakeRunner) Start(_ context.Context, spec backing.Spec) (backing.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return backing.Endpoint{}, f.startErr
	}
	f.started = append(f.started, spec)
	f.running[spec.Name] = true
	return backing.Endpoint{Address: "127.0.0.1", Port: spec.HostPort}, nil
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
	return f.running[name], nil
}

func (f *fakeRunner) StopAll(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = map[string]bool{}
	return nil
}

func (f *fakeRunner) Export(_ context.Context, _, name string, data backing.Data, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[name] {
		return backing.ErrNotRunning
	}
	f.exported[name] = data
	return os.WriteFile(dst, []byte(name), 0o644)
}

// sequentialPorts hands out 41001, 41002, ...
func sequentialPorts() func() (int, error) {
	next := 41000
	return func() (int, error) { next++; return next, nil }
}

func newTestService(t *testing.T, runner backing.Runner) *Service {
	t.Helper()
	metadata, err := bbolt.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { metadata.Close() })
	return NewService(Options{
		Metadata: metadata,
		Engines:  runner,
		Now:      func() time.Time { return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC) },
		FreePort: sequentialPorts(),
	})
}

func do(t *testing.T, svc *Service, method, path, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	return rec, svc.Handle(rec, req)
}

func mustDo(t *testing.T, svc *Service, method, path, body string) map[string]any {
	t.Helper()
	rec, err := do(t, svc, method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if rec.Code == http.StatusNoContent {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s %s: %v\n%s", method, path, err, rec.Body.String())
	}
	return out
}

func requireCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != status || apiErr.Code != code {
		t.Fatalf("expected %d %s, got %v", status, code, err)
	}
}

const provisionedBody = `{
  "clusterName": "orders",
  "kafkaVersion": "3.6.0",
  "numberOfBrokerNodes": 3,
  "brokerNodeGroupInfo": {"instanceType": "kafka.m5.large", "clientSubnets": ["subnet-1", "subnet-2", "subnet-3"]},
  "configurationInfo": {"arn": "arn:aws:kafka:us-east-1:000000000000:configuration/base/1", "revision": 2},
  "tags": {"team": "payments"}
}`

func clusterPath(arn string) string { return "/v1/clusters/" + url.PathEscape(arn) }

func createOrders(t *testing.T, svc *Service) string {
	t.Helper()
	out := mustDo(t, svc, http.MethodPost, "/v1/clusters", provisionedBody)
	return out["clusterArn"].(string)
}

func TestCreateClusterStartsBrokerAdvertisingItsEndpoint(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	out := mustDo(t, svc, http.MethodPost, "/v1/clusters", provisionedBody)

	arn, _ := out["clusterArn"].(string)
	if !strings.HasPrefix(arn, "arn:aws:kafka:us-east-1:000000000000:cluster/orders/") || out["state"] != "ACTIVE" {
		t.Fatalf("unexpected create response: %v", out)
	}
	if len(runner.started) != 1 {
		t.Fatalf("one broker should start, got %d", len(runner.started))
	}
	spec := runner.started[0]
	if spec.Kind != "kafka" || spec.Name != "orders" || spec.Image != defaultImage || spec.HostPort != 41001 || spec.Port != brokerPort {
		t.Fatalf("unexpected broker spec: %+v", spec)
	}
	if !strings.Contains(strings.Join(spec.Cmd, " "), "--advertise-kafka-addr PLAINTEXT://127.0.0.1:41001") {
		t.Fatalf("broker must advertise the endpoint clients reach: %v", spec.Cmd)
	}
	if spec.Probe == nil || spec.Data == nil || spec.Data.Path != dataDir {
		t.Fatalf("broker should probe for a controller and keep data in %s: %+v", dataDir, spec)
	}

	brokers := mustDo(t, svc, http.MethodGet, clusterPath(arn)+"/bootstrap-brokers", "")
	if brokers["bootstrapBrokerString"] != "127.0.0.1:41001" {
		t.Fatalf("bootstrap brokers = %v", brokers)
	}
}

func TestDescribeClusterEchoesConfigurationAndRequestedBrokerCount(t *testing.T) {
	svc := newTestService(t, newFakeRunner())
	arn := createOrders(t, svc)

	info := mustDo(t, svc, http.MethodGet, clusterPath(arn), "")["clusterInfo"].(map[string]any)

	if info["clusterName"] != "orders" || info["numberOfBrokerNodes"] != float64(3) || info["creationTime"] != "2026-11-10T09:00:00.000Z" {
		t.Fatalf("unexpected cluster info: %v", info)
	}
	group := info["brokerNodeGroupInfo"].(map[string]any)
	if group["instanceType"] != "kafka.m5.large" {
		t.Fatalf("broker node group should be echoed: %v", group)
	}
	software := info["currentBrokerSoftwareInfo"].(map[string]any)
	if software["kafkaVersion"] != "3.6.0" || software["configurationRevision"] != float64(2) {
		t.Fatalf("broker software should report version and configuration: %v", software)
	}
	if info["tags"].(map[string]any)["team"] != "payments" {
		t.Fatalf("tags should be reported: %v", info["tags"])
	}
}

func TestCreateClusterV2SupportsServerlessAndProvisioned(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)

	serverless := mustDo(t, svc, http.MethodPost, "/api/v2/clusters",
		`{"clusterName":"events","serverless":{"vpcConfigs":[{"subnetIds":["subnet-1"]}],"clientAuthentication":{"sasl":{"iam":{"enabled":true}}}}}`)
	provisioned := mustDo(t, svc, http.MethodPost, "/api/v2/clusters",
		`{"clusterName":"audit","provisioned":{"kafkaVersion":"3.5.1","numberOfBrokerNodes":2,"brokerNodeGroupInfo":{"instanceType":"kafka.t3.small"}}}`)

	if serverless["clusterType"] != "SERVERLESS" || provisioned["clusterType"] != "PROVISIONED" {
		t.Fatalf("cluster types: %v / %v", serverless, provisioned)
	}
	info := mustDo(t, svc, http.MethodGet, "/api/v2/clusters/"+url.PathEscape(serverless["clusterArn"].(string)), "")["clusterInfo"].(map[string]any)
	if info["serverless"] == nil || info["provisioned"] != nil {
		t.Fatalf("serverless cluster should echo its serverless settings only: %v", info)
	}

	v2 := mustDo(t, svc, http.MethodGet, "/api/v2/clusters?clusterTypeFilter=SERVERLESS", "")["clusterInfoList"].([]any)
	if len(v2) != 1 || v2[0].(map[string]any)["clusterName"] != "events" {
		t.Fatalf("type filter should keep serverless clusters only: %v", v2)
	}
	v1 := mustDo(t, svc, http.MethodGet, "/v1/clusters", "")["clusterInfoList"].([]any)
	if len(v1) != 1 || v1[0].(map[string]any)["clusterName"] != "audit" {
		t.Fatalf("ListClusters lists provisioned clusters only: %v", v1)
	}
	if len(runner.started) != 2 {
		t.Fatalf("every cluster type runs a real broker, got %d", len(runner.started))
	}
}

func TestCreateClusterRejectsInvalidAndDuplicateRequests(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	createOrders(t, svc)

	cases := map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"duplicate name":  {"/v1/clusters", provisionedBody, http.StatusConflict, "ConflictException"},
		"bad name":        {"/v1/clusters", `{"clusterName":"-x","kafkaVersion":"3.6.0","numberOfBrokerNodes":1}`, http.StatusBadRequest, "BadRequestException"},
		"no version":      {"/v1/clusters", `{"clusterName":"x","numberOfBrokerNodes":1}`, http.StatusBadRequest, "BadRequestException"},
		"no brokers":      {"/v1/clusters", `{"clusterName":"x","kafkaVersion":"3.6.0"}`, http.StatusBadRequest, "BadRequestException"},
		"v2 without type": {"/api/v2/clusters", `{"clusterName":"x"}`, http.StatusBadRequest, "BadRequestException"},
		"invalid json":    {"/v1/clusters", `{`, http.StatusBadRequest, "BadRequestException"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := do(t, svc, http.MethodPost, tc.path, tc.body)
			requireCode(t, err, tc.status, tc.code)
		})
	}
	if len(runner.started) != 1 {
		t.Fatalf("rejected requests must not start brokers, got %d starts", len(runner.started))
	}
}

func TestCreateClusterReportsMissingDockerWithoutKeepingTheCluster(t *testing.T) {
	runner := newFakeRunner()
	runner.startErr = backing.ErrUnavailable
	svc := newTestService(t, runner)

	_, err := do(t, svc, http.MethodPost, "/v1/clusters", provisionedBody)
	requireCode(t, err, http.StatusServiceUnavailable, "ServiceUnavailable")
	if clusters, _ := svc.List("", ""); len(clusters) != 0 {
		t.Fatalf("a cluster whose broker never started must not be recorded: %v", clusters)
	}
}

func TestDeleteClusterStopsBrokerAndForgetsCluster(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	arn := createOrders(t, svc)

	out := mustDo(t, svc, http.MethodDelete, clusterPath(arn), "")

	if out["state"] != "DELETING" || len(runner.stopped) != 1 || runner.stopped[0] != "orders" {
		t.Fatalf("delete should stop the broker: %v %v", out, runner.stopped)
	}
	_, err := do(t, svc, http.MethodGet, clusterPath(arn), "")
	requireCode(t, err, http.StatusNotFound, "NotFoundException")
}

func TestUnknownOrForeignARNsAreNotFound(t *testing.T) {
	svc := newTestService(t, newFakeRunner())
	arn := createOrders(t, svc)
	// Same name, different cluster: a deleted-and-recreated cluster's old ARN.
	stale := arn[:strings.LastIndex(arn, "/")] + "/another-incarnation"

	_, err := do(t, svc, http.MethodGet, clusterPath(stale), "")
	requireCode(t, err, http.StatusNotFound, "NotFoundException")
	_, err = do(t, svc, http.MethodGet, clusterPath("not-an-arn"), "")
	requireCode(t, err, http.StatusBadRequest, "BadRequestException")
}

func TestTagsCanBeListedAddedAndRemoved(t *testing.T) {
	svc := newTestService(t, newFakeRunner())
	arn := createOrders(t, svc)
	tagsPath := "/v1/tags/" + url.PathEscape(arn)

	mustDo(t, svc, http.MethodPost, tagsPath, `{"tags":{"env":"test"}}`)
	mustDo(t, svc, http.MethodDelete, tagsPath+"?tagKeys=team", "")

	tags := mustDo(t, svc, http.MethodGet, tagsPath, "")["tags"].(map[string]any)
	if len(tags) != 1 || tags["env"] != "test" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestListNodesDescribesTheSingleBroker(t *testing.T) {
	svc := newTestService(t, newFakeRunner())
	arn := createOrders(t, svc)

	nodes := mustDo(t, svc, http.MethodGet, clusterPath(arn)+"/nodes", "")["nodeInfoList"].([]any)

	if len(nodes) != 1 {
		t.Fatalf("one broker runs per cluster, got %v", nodes)
	}
	node := nodes[0].(map[string]any)
	if node["instanceType"] != "kafka.m5.large" || node["nodeType"] != "BROKER" {
		t.Fatalf("unexpected node: %v", node)
	}
}

func TestReconcileRestartsBrokersOnRecordedPortFromCapturedData(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	createOrders(t, svc)
	dir := t.TempDir()
	captured, err := svc.Capture(context.Background(), dir)
	if err != nil || len(captured) != 1 || captured[0] != "orders" {
		t.Fatalf("capture = %v, %v", captured, err)
	}
	if runner.exported["orders"].Path != dataDir {
		t.Fatalf("capture should export the broker's data dir: %+v", runner.exported["orders"])
	}
	_ = runner.StopAll(context.Background(), kind)
	runner.started = nil

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{DataDir: dir}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	spec := runner.started[0]
	if spec.HostPort != 41001 || spec.Seed != backing.ArchivePath(dir, "orders") {
		t.Fatalf("broker should restart on its port from its archive: %+v", spec)
	}
}

func TestReconcileWithFreshEndpointsReadvertisesANewPort(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	arn := createOrders(t, svc)
	_ = runner.StopAll(context.Background(), kind)

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{FreshEndpoints: true}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	spec := runner.started[len(runner.started)-1]
	if spec.HostPort != 41002 || !strings.Contains(strings.Join(spec.Cmd, " "), "127.0.0.1:41002") {
		t.Fatalf("a forked broker must advertise its new port: %+v", spec)
	}
	brokers := mustDo(t, svc, http.MethodGet, clusterPath(arn)+"/bootstrap-brokers", "")
	if brokers["bootstrapBrokerString"] != "127.0.0.1:41002" {
		t.Fatalf("bootstrap brokers should follow the new port: %v", brokers)
	}
}

func TestReconcileMarksClustersWhoseBrokerCannotStart(t *testing.T) {
	runner := newFakeRunner()
	svc := newTestService(t, runner)
	arn := createOrders(t, svc)
	_ = runner.StopAll(context.Background(), kind)
	runner.startErr = backing.ErrUnavailable

	if err := svc.Reconcile(context.Background(), backing.ReconcileOptions{}); err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("reconcile should name the cluster it could not start, got %v", err)
	}
	if c, _ := svc.Get(arn); c.State != "FAILED" {
		t.Fatalf("cluster should be marked failed, got %q", c.State)
	}
}

func TestClustersViewListsBootstrapAddresses(t *testing.T) {
	svc := newTestService(t, newFakeRunner())
	createOrders(t, svc)

	views, err := svc.Clusters()
	if err != nil || len(views) != 1 {
		t.Fatalf("clusters = %v, %v", views, err)
	}
	raw, _ := json.Marshal(views[0])
	if !bytes.Contains(raw, []byte(`"bootstrap_brokers":"127.0.0.1:41001"`)) || views[0].KafkaVersion != "3.6.0" {
		t.Fatalf("unexpected view: %s", raw)
	}
}

func TestOperationRoutesEscapedARNs(t *testing.T) {
	arn := url.PathEscape("arn:aws:kafka:us-east-1:000000000000:cluster/orders/abc-1")
	cases := map[string]string{
		"POST /v1/clusters":                              "CreateCluster",
		"GET /v1/clusters/" + arn:                        "DescribeCluster",
		"DELETE /v1/clusters/" + arn:                     "DeleteCluster",
		"GET /v1/clusters/" + arn + "/bootstrap-brokers": "GetBootstrapBrokers",
		"GET /api/v2/clusters/" + arn:                    "DescribeClusterV2",
		"DELETE /v1/tags/" + arn:                         "UntagResource",
		"GET /v1/clusters/" + arn + "/operations":        "",
		"PUT /v1/clusters":                               "",
	}
	for in, want := range cases {
		method, path, _ := strings.Cut(in, " ")
		if got := Operation(method, path); got != want {
			t.Errorf("Operation(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultEngineHonoursImageOverride(t *testing.T) {
	if got := DefaultEngine(func(string) string { return "mirror/redpanda:v24" }).Image; got != "mirror/redpanda:v24" {
		t.Fatalf("override ignored: %q", got)
	}
	if got := DefaultEngine(func(string) string { return "  " }).Image; got != defaultImage {
		t.Fatalf("blank override should keep the default: %q", got)
	}
}
