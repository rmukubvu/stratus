package contract

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRoute53ZonesAndRecords drives Route 53 with the AWS CLI: a hosted zone
// gets its NS and SOA records, a change batch is atomic, and the control API
// resolves names through CNAMEs the way a client would.
func TestRoute53ZonesAndRecords(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	out := runAWS(t, h.baseURL, "route53", "create-hosted-zone", "--name", "example.test", "--caller-reference", "ref-1", "--output", "json")
	var created struct {
		HostedZone struct {
			ID string `json:"Id"`
		} `json:"HostedZone"`
		ChangeInfo struct {
			Status string `json:"Status"`
		} `json:"ChangeInfo"`
	}
	if err := json.Unmarshal(out, &created); err != nil || !strings.HasPrefix(created.HostedZone.ID, "/hostedzone/") {
		t.Fatalf("create-hosted-zone (%v): %s", err, out)
	}
	zoneID := strings.TrimPrefix(created.HostedZone.ID, "/hostedzone/")

	batch := `{"Changes":[
		{"Action":"CREATE","ResourceRecordSet":{"Name":"db.example.test","Type":"A","TTL":60,"ResourceRecords":[{"Value":"10.0.0.7"}]}},
		{"Action":"CREATE","ResourceRecordSet":{"Name":"api.example.test","Type":"CNAME","TTL":60,"ResourceRecords":[{"Value":"db.example.test"}]}}
	]}`
	out = runAWS(t, h.baseURL, "route53", "change-resource-record-sets", "--hosted-zone-id", zoneID, "--change-batch", batch, "--output", "json")
	if !strings.Contains(string(out), `"INSYNC"`) {
		t.Fatalf("changes should be in sync at once: %s", out)
	}

	out = runAWS(t, h.baseURL, "route53", "list-resource-record-sets", "--hosted-zone-id", zoneID, "--output", "json")
	for _, want := range []string{`"Type": "NS"`, `"Type": "SOA"`, `"api.example.test."`, `"db.example.test."`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("record sets should include %s: %s", want, out)
		}
	}

	// A failing change leaves the zone as it was: the first change in this
	// batch is fine, the second deletes a record that does not exist.
	bad := `{"Changes":[
		{"Action":"CREATE","ResourceRecordSet":{"Name":"cache.example.test","Type":"A","TTL":60,"ResourceRecords":[{"Value":"10.0.0.9"}]}},
		{"Action":"DELETE","ResourceRecordSet":{"Name":"gone.example.test","Type":"A","TTL":60,"ResourceRecords":[{"Value":"10.0.0.1"}]}}
	]}`
	if msg := runAWSExpectingFailure(t, h.baseURL, "route53", "change-resource-record-sets", "--hosted-zone-id", zoneID, "--change-batch", bad); !strings.Contains(msg, "InvalidChangeBatch") {
		t.Fatalf("a bad batch should fail with InvalidChangeBatch: %s", msg)
	}
	if res := resolveDNS(t, h.baseURL, "cache.example.test", "A"); res.Found {
		t.Fatalf("a failed batch must not apply any change: %+v", res)
	}

	res := resolveDNS(t, h.baseURL, "api.example.test", "A")
	if !res.Found || len(res.Answers) != 2 || res.Answers[0].Type != "CNAME" || res.Answers[1].Value != "10.0.0.7" {
		t.Fatalf("api.example.test should resolve through its CNAME: %+v", res)
	}

	if msg := runAWSExpectingFailure(t, h.baseURL, "route53", "delete-hosted-zone", "--id", zoneID); !strings.Contains(msg, "HostedZoneNotEmpty") {
		t.Fatalf("deleting a zone with records should fail: %s", msg)
	}
}

// TestCloudMapPublishesDNSRecords registers instances with the AWS CLI and
// checks that DNS and DiscoverInstances see them, and that an instance
// reported unhealthy drops out of DNS.
func TestCloudMapPublishesDNSRecords(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	out := runAWS(t, h.baseURL, "servicediscovery", "create-private-dns-namespace", "--name", "corp.local", "--vpc", "vpc-1", "--output", "json")
	var op struct {
		OperationID string `json:"OperationId"`
	}
	if err := json.Unmarshal(out, &op); err != nil || op.OperationID == "" {
		t.Fatalf("create namespace (%v): %s", err, out)
	}
	out = runAWS(t, h.baseURL, "servicediscovery", "get-operation", "--operation-id", op.OperationID, "--output", "json")
	var operation struct {
		Operation struct {
			Status  string            `json:"Status"`
			Targets map[string]string `json:"Targets"`
		} `json:"Operation"`
	}
	if err := json.Unmarshal(out, &operation); err != nil || operation.Operation.Status != "SUCCESS" {
		t.Fatalf("namespace operation should succeed (%v): %s", err, out)
	}
	nsID := operation.Operation.Targets["NAMESPACE"]

	out = runAWS(t, h.baseURL, "servicediscovery", "create-service", "--name", "api", "--namespace-id", nsID,
		"--dns-config", `{"RoutingPolicy":"MULTIVALUE","DnsRecords":[{"Type":"A","TTL":60},{"Type":"SRV","TTL":60}]}`,
		"--health-check-custom-config", `{"FailureThreshold":1}`, "--output", "json")
	var svc struct {
		Service struct {
			ID string `json:"Id"`
		} `json:"Service"`
	}
	if err := json.Unmarshal(out, &svc); err != nil || svc.Service.ID == "" {
		t.Fatalf("create service (%v): %s", err, out)
	}

	for id, ip := range map[string]string{"api-1": "10.1.0.1", "api-2": "10.1.0.2"} {
		runAWS(t, h.baseURL, "servicediscovery", "register-instance", "--service-id", svc.Service.ID, "--instance-id", id,
			"--attributes", "AWS_INSTANCE_IPV4="+ip+",AWS_INSTANCE_PORT=8080,stage=prod", "--output", "json")
	}

	if res := resolveDNS(t, h.baseURL, "api.corp.local", "A"); len(res.Answers) != 2 {
		t.Fatalf("both instances should resolve: %+v", res)
	}
	if res := resolveDNS(t, h.baseURL, "api.corp.local", "SRV"); len(res.Answers) != 2 || !strings.HasPrefix(res.Answers[0].Value, "1 1 8080 ") {
		t.Fatalf("SRV records should carry the port: %+v", res)
	}

	out = runAWS(t, h.baseURL, "servicediscovery", "discover-instances", "--namespace-name", "corp.local", "--service-name", "api",
		"--query-parameters", "stage=prod", "--output", "json")
	if strings.Count(string(out), `"InstanceId"`) != 2 {
		t.Fatalf("discover-instances should find both instances: %s", out)
	}

	runAWS(t, h.baseURL, "servicediscovery", "update-instance-custom-health-status", "--service-id", svc.Service.ID, "--instance-id", "api-1", "--status", "UNHEALTHY")
	res := resolveDNS(t, h.baseURL, "api.corp.local", "A")
	if len(res.Answers) != 1 || res.Answers[0].Value != "10.1.0.2" {
		t.Fatalf("the unhealthy instance should drop out of DNS: %+v", res)
	}

	// The namespace's hosted zone is visible through Route 53 too.
	out = runAWS(t, h.baseURL, "route53", "list-hosted-zones-by-name", "--dns-name", "corp.local", "--output", "json")
	if !strings.Contains(string(out), `"corp.local."`) || !strings.Contains(string(out), `"PrivateZone": true`) {
		t.Fatalf("the namespace should own a private hosted zone: %s", out)
	}
}

type dnsResolution struct {
	Found   bool `json:"found"`
	Answers []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"answers"`
}

func resolveDNS(t *testing.T, base, name, rtype string) dnsResolution {
	t.Helper()
	q := url.Values{"name": {name}, "type": {rtype}}
	body := mustStatus(t, http.MethodGet, base+"/_stratus/control/v1/dns/resolve?"+q.Encode(), "", http.StatusOK)
	var res dnsResolution
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode resolution: %v: %s", err, body)
	}
	return res
}
