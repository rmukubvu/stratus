package servicediscovery

import (
	"reflect"
	"testing"
)

func TestServiceRecordsFollowDNSConfig(t *testing.T) {
	ns := Namespace{ID: "ns-1", Name: "corp.local", Type: typeDNSPrivate, HostedZoneID: "Z1"}
	svc := Service{Name: "api", DNSConfig: &DNSConfig{RoutingPolicy: routingMultivalue, DNSRecords: []DNSRecordTemplate{{Type: "A", TTL: 60}, {Type: "SRV", TTL: 30}}}}
	inst := Instance{ID: "i-1", Attributes: map[string]string{attrIPv4: "10.0.0.5", attrPort: "8080"}}

	got, err := svc.records(ns, inst)
	if err != nil {
		t.Fatal(err)
	}
	want := []DNSRecord{
		{Name: "api.corp.local.", Type: "A", TTL: 60, SetIdentifier: "i-1", MultiValue: true, Value: "10.0.0.5"},
		{Name: "i-1.api.corp.local.", Type: "A", TTL: 30, SetIdentifier: "i-1", MultiValue: true, Value: "10.0.0.5"},
		{Name: "api.corp.local.", Type: "SRV", TTL: 30, SetIdentifier: "i-1", MultiValue: true, Value: "1 1 8080 i-1.api.corp.local."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %+v\nwant %+v", got, want)
	}
}

func TestServiceRecordsWeightedRouting(t *testing.T) {
	ns := Namespace{Name: "corp.local", Type: typeDNSPublic}
	svc := Service{Name: "web", DNSConfig: &DNSConfig{RoutingPolicy: routingWeighted, DNSRecords: []DNSRecordTemplate{{Type: "CNAME", TTL: 60}}}}
	got, err := svc.records(ns, Instance{ID: "i-2", Attributes: map[string]string{attrCNAME: "lb.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MultiValue || got[0].Value != "lb.example.com" {
		t.Fatalf("records = %+v", got)
	}
}

func TestServiceRecordsNeedTheMatchingAttribute(t *testing.T) {
	ns := Namespace{Name: "corp.local", Type: typeDNSPrivate}
	cases := map[string][]DNSRecordTemplate{
		"A without IPv4":    {{Type: "A"}},
		"AAAA without IPv6": {{Type: "AAAA"}},
		"SRV without port":  {{Type: "SRV"}},
		"unsupported type":  {{Type: "TXT"}},
	}
	for name, templates := range cases {
		t.Run(name, func(t *testing.T) {
			svc := Service{Name: "api", DNSConfig: &DNSConfig{DNSRecords: templates}}
			if _, err := svc.records(ns, Instance{ID: "i-1", Attributes: map[string]string{}}); err == nil {
				t.Fatal("records succeeded, want an error")
			}
		})
	}
}

func TestHTTPNamespacesPublishNoRecords(t *testing.T) {
	svc := Service{Name: "api", DNSConfig: &DNSConfig{DNSRecords: []DNSRecordTemplate{{Type: "A"}}}}
	got, err := svc.records(Namespace{Name: "corp", Type: typeHTTP}, Instance{ID: "i-1"})
	if err != nil || got != nil {
		t.Fatalf("records = %v, %v; want nothing", got, err)
	}
}

func TestFilterInstances(t *testing.T) {
	instances := []Instance{
		{ID: "a", Health: healthy, Attributes: map[string]string{"stage": "prod", "az": "a"}},
		{ID: "b", Health: unhealthy, Attributes: map[string]string{"stage": "prod", "az": "b"}},
		{ID: "c", Health: healthy, Attributes: map[string]string{"stage": "dev", "az": "a"}},
	}
	ids := func(in []Instance) []string {
		out := []string{}
		for _, i := range in {
			out = append(out, i.ID)
		}
		return out
	}
	cases := []struct {
		name string
		q    DiscoverQuery
		want []string
	}{
		{"everything by default", DiscoverQuery{}, []string{"a", "b", "c"}},
		{"query parameters must match", DiscoverQuery{QueryParameters: map[string]string{"stage": "prod"}}, []string{"a", "b"}},
		{"optional parameters prefer", DiscoverQuery{QueryParameters: map[string]string{"stage": "prod"}, OptionalParameters: map[string]string{"az": "b"}}, []string{"b"}},
		{"optional parameters fall back", DiscoverQuery{OptionalParameters: map[string]string{"az": "z"}}, []string{"a", "b", "c"}},
		{"healthy only", DiscoverQuery{HealthStatus: healthy}, []string{"a", "c"}},
		{"unhealthy only", DiscoverQuery{HealthStatus: unhealthy}, []string{"b"}},
		{"healthy or else all", DiscoverQuery{QueryParameters: map[string]string{"az": "b"}, HealthStatus: "HEALTHY_OR_ELSE_ALL"}, []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(filterInstances(instances, tc.q)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
