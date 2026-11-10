package servicediscovery_test

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/services/route53"
	"github.com/stratus/internal/services/servicediscovery"
	"github.com/stratus/internal/store/bbolt"
)

type fixture struct {
	dir   *servicediscovery.Directory
	zones *route53.Service
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	metadata, err := bbolt.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { metadata.Close() })
	now := func() time.Time { return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC) }
	zones := route53.NewService(route53.Options{Metadata: metadata, Now: now})
	return fixture{dir: servicediscovery.NewDirectory(servicediscovery.Options{Metadata: metadata, Zones: zones, Now: now}), zones: zones}
}

// call drives the JSON API the way the AWS SDKs do.
func (f fixture) call(t *testing.T, op string, in any) (map[string]any, error) {
	t.Helper()
	body, _ := json.Marshal(in)
	rec := httptest.NewRecorder()
	if err := f.dir.Handle(rec, httptest.NewRequest("POST", "/", strings.NewReader(string(body))), op); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", op, err)
	}
	return out, nil
}

func (f fixture) mustCall(t *testing.T, op string, in any) map[string]any {
	t.Helper()
	out, err := f.call(t, op, in)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return out
}

func code(err error) string {
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

func (f fixture) privateNamespace(t *testing.T, name string) string {
	t.Helper()
	op := f.mustCall(t, "CreatePrivateDnsNamespace", map[string]any{"Name": name, "Vpc": "vpc-1"})
	got := f.mustCall(t, "GetOperation", map[string]any{"OperationId": op["OperationId"]})
	return got["Operation"].(map[string]any)["Targets"].(map[string]any)["NAMESPACE"].(string)
}

func (f fixture) dnsService(t *testing.T, nsID string, custom bool) string {
	t.Helper()
	in := map[string]any{
		"Name": "api", "NamespaceId": nsID,
		"DnsConfig": map[string]any{"RoutingPolicy": "MULTIVALUE", "DnsRecords": []any{map[string]any{"Type": "A", "TTL": 60}}},
	}
	if custom {
		in["HealthCheckCustomConfig"] = map[string]any{"FailureThreshold": 1}
	}
	out := f.mustCall(t, "CreateService", in)
	return out["Service"].(map[string]any)["Id"].(string)
}

func (f fixture) resolve(t *testing.T, name string) []string {
	t.Helper()
	res, err := f.zones.Resolve(name, "A")
	if err != nil {
		t.Fatal(err)
	}
	values := []string{}
	for _, a := range res.Answers {
		values = append(values, a.Value)
	}
	return values
}

func TestRegisteredInstancesResolveThroughRoute53(t *testing.T) {
	f := newFixture(t)
	nsID := f.privateNamespace(t, "corp.local")
	svcID := f.dnsService(t, nsID, false)

	for id, ip := range map[string]string{"i-1": "10.0.0.1", "i-2": "10.0.0.2"} {
		f.mustCall(t, "RegisterInstance", map[string]any{"ServiceId": svcID, "InstanceId": id, "Attributes": map[string]string{"AWS_INSTANCE_IPV4": ip}})
	}
	if got := f.resolve(t, "api.corp.local"); strings.Join(got, ",") != "10.0.0.1,10.0.0.2" {
		t.Fatalf("api.corp.local = %v", got)
	}

	f.mustCall(t, "DeregisterInstance", map[string]any{"ServiceId": svcID, "InstanceId": "i-1"})
	if got := f.resolve(t, "api.corp.local"); strings.Join(got, ",") != "10.0.0.2" {
		t.Fatalf("after deregister api.corp.local = %v", got)
	}

	ns := f.mustCall(t, "GetNamespace", map[string]any{"Id": nsID})["Namespace"].(map[string]any)
	zoneID := ns["Properties"].(map[string]any)["DnsProperties"].(map[string]any)["HostedZoneId"].(string)
	if _, err := f.zones.Zone(zoneID); err != nil {
		t.Fatalf("namespace zone %s: %v", zoneID, err)
	}
	if ns["ServiceCount"].(float64) != 1 {
		t.Fatalf("ServiceCount = %v", ns["ServiceCount"])
	}
}

func TestCustomHealthTakesInstancesOutOfDNSAndDiscovery(t *testing.T) {
	f := newFixture(t)
	nsID := f.privateNamespace(t, "corp.local")
	svcID := f.dnsService(t, nsID, true)
	f.mustCall(t, "RegisterInstance", map[string]any{"ServiceId": svcID, "InstanceId": "i-1", "Attributes": map[string]string{"AWS_INSTANCE_IPV4": "10.0.0.1"}})

	f.mustCall(t, "UpdateInstanceCustomHealthStatus", map[string]any{"ServiceId": svcID, "InstanceId": "i-1", "Status": "UNHEALTHY"})
	if got := f.resolve(t, "api.corp.local"); len(got) != 0 {
		t.Fatalf("unhealthy instance still resolves: %v", got)
	}
	healthyOnly := f.mustCall(t, "DiscoverInstances", map[string]any{"NamespaceName": "corp.local", "ServiceName": "api", "HealthStatus": "HEALTHY"})
	if n := len(healthyOnly["Instances"].([]any)); n != 0 {
		t.Fatalf("HEALTHY discovery returned %d instances", n)
	}
	all := f.mustCall(t, "DiscoverInstances", map[string]any{"NamespaceName": "corp.local", "ServiceName": "api"})
	if n := len(all["Instances"].([]any)); n != 1 {
		t.Fatalf("default discovery returned %d instances, want 1", n)
	}
	status := f.mustCall(t, "GetInstancesHealthStatus", map[string]any{"ServiceId": svcID})
	if status["Status"].(map[string]any)["i-1"] != "UNHEALTHY" {
		t.Fatalf("health status = %v", status)
	}

	f.mustCall(t, "UpdateInstanceCustomHealthStatus", map[string]any{"ServiceId": svcID, "InstanceId": "i-1", "Status": "HEALTHY"})
	if got := f.resolve(t, "api.corp.local"); len(got) != 1 {
		t.Fatalf("healthy again but resolves to %v", got)
	}
}

func TestCustomHealthNeedsCustomConfig(t *testing.T) {
	f := newFixture(t)
	svcID := f.dnsService(t, f.privateNamespace(t, "corp.local"), false)
	f.mustCall(t, "RegisterInstance", map[string]any{"ServiceId": svcID, "InstanceId": "i-1", "Attributes": map[string]string{"AWS_INSTANCE_IPV4": "10.0.0.1"}})
	_, err := f.call(t, "UpdateInstanceCustomHealthStatus", map[string]any{"ServiceId": svcID, "InstanceId": "i-1", "Status": "UNHEALTHY"})
	if code(err) != "CustomHealthNotFound" {
		t.Fatalf("err = %v, want CustomHealthNotFound", err)
	}
}

func TestHTTPNamespaceDiscoversByAttributes(t *testing.T) {
	f := newFixture(t)
	op := f.mustCall(t, "CreateHttpNamespace", map[string]any{"Name": "jobs"})
	nsID := f.mustCall(t, "GetOperation", map[string]any{"OperationId": op["OperationId"]})["Operation"].(map[string]any)["Targets"].(map[string]any)["NAMESPACE"]
	svc := f.mustCall(t, "CreateService", map[string]any{"Name": "worker", "NamespaceId": nsID})["Service"].(map[string]any)
	if svc["Type"] != "HTTP" {
		t.Fatalf("service type = %v", svc["Type"])
	}
	for id, stage := range map[string]string{"w-1": "prod", "w-2": "dev"} {
		f.mustCall(t, "RegisterInstance", map[string]any{"ServiceId": svc["Id"], "InstanceId": id, "Attributes": map[string]string{"stage": stage}})
	}
	out := f.mustCall(t, "DiscoverInstances", map[string]any{"NamespaceName": "jobs", "ServiceName": "worker", "QueryParameters": map[string]string{"stage": "prod"}})
	instances := out["Instances"].([]any)
	if len(instances) != 1 || instances[0].(map[string]any)["InstanceId"] != "w-1" {
		t.Fatalf("discovered %v", instances)
	}
}

func TestDeletesRefuseWhileInUse(t *testing.T) {
	f := newFixture(t)
	nsID := f.privateNamespace(t, "corp.local")
	svcID := f.dnsService(t, nsID, false)
	f.mustCall(t, "RegisterInstance", map[string]any{"ServiceId": svcID, "InstanceId": "i-1", "Attributes": map[string]string{"AWS_INSTANCE_IPV4": "10.0.0.1"}})

	if _, err := f.call(t, "DeleteService", map[string]any{"Id": svcID}); code(err) != "ResourceInUse" {
		t.Fatalf("delete service with instances: %v", err)
	}
	if _, err := f.call(t, "DeleteNamespace", map[string]any{"Id": nsID}); code(err) != "ResourceInUse" {
		t.Fatalf("delete namespace with services: %v", err)
	}
	f.mustCall(t, "DeregisterInstance", map[string]any{"ServiceId": svcID, "InstanceId": "i-1"})
	f.mustCall(t, "DeleteService", map[string]any{"Id": svcID})
	f.mustCall(t, "DeleteNamespace", map[string]any{"Id": nsID})
	if zones, _ := f.zones.Zones(""); len(zones) != 0 {
		t.Fatalf("namespace zone left behind: %v", zones)
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t)
	if _, err := f.call(t, "CreatePrivateDnsNamespace", map[string]any{"Name": "corp.local"}); code(err) != "InvalidInput" {
		t.Fatalf("private namespace without VPC: %v", err)
	}
	f.privateNamespace(t, "corp.local")
	if _, err := f.call(t, "CreatePrivateDnsNamespace", map[string]any{"Name": "corp.local", "Vpc": "vpc-1"}); code(err) != "NamespaceAlreadyExists" {
		t.Fatalf("duplicate namespace: %v", err)
	}
	if _, err := f.call(t, "GetService", map[string]any{"Id": "srv-missing"}); code(err) != "ServiceNotFound" {
		t.Fatalf("missing service: %v", err)
	}
	if _, err := f.call(t, "Nope", map[string]any{}); code(err) != "UnknownOperationException" {
		t.Fatalf("unknown op: %v", err)
	}
}

func TestTags(t *testing.T) {
	f := newFixture(t)
	nsID := f.privateNamespace(t, "corp.local")
	arn := f.mustCall(t, "GetNamespace", map[string]any{"Id": nsID})["Namespace"].(map[string]any)["Arn"]
	f.mustCall(t, "TagResource", map[string]any{"ResourceARN": arn, "Tags": []any{map[string]string{"Key": "team", "Value": "core"}, map[string]string{"Key": "env", "Value": "ci"}}})
	f.mustCall(t, "UntagResource", map[string]any{"ResourceARN": arn, "TagKeys": []string{"env"}})
	tags := f.mustCall(t, "ListTagsForResource", map[string]any{"ResourceARN": arn})["Tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["Key"] != "team" {
		t.Fatalf("tags = %v", tags)
	}
}
