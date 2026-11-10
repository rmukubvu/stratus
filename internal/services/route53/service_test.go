package route53

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/store/bbolt"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	metadata, err := bbolt.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { metadata.Close() })
	return NewService(Options{Metadata: metadata, Now: func() time.Time { return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC) }})
}

func serve(t *testing.T, svc *Service, method, path, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	err := svc.Handle(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec, err
}

func mustServe(t *testing.T, svc *Service, method, path, body string, into any) *httptest.ResponseRecorder {
	t.Helper()
	rec, err := serve(t, svc, method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if into != nil {
		if err := xml.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("decode %s %s: %v\n%s", method, path, err, rec.Body.String())
		}
	}
	return rec
}

const createZoneBody = `<CreateHostedZoneRequest xmlns="https://route53.amazonaws.com/doc/2013-04-01/">
  <Name>example.com</Name><CallerReference>ref-1</CallerReference>
  <HostedZoneConfig><Comment>tests</Comment></HostedZoneConfig>
</CreateHostedZoneRequest>`

func createZone(t *testing.T, svc *Service) string {
	t.Helper()
	var out struct {
		HostedZone struct {
			ID string `xml:"Id"`
		} `xml:"HostedZone"`
	}
	rec := mustServe(t, svc, http.MethodPost, "/2013-04-01/hostedzone", createZoneBody, &out)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(out.HostedZone.ID, "/hostedzone/Z") {
		t.Fatalf("create should answer 201 with a zone id: %d %s", rec.Code, rec.Body.String())
	}
	return strings.TrimPrefix(out.HostedZone.ID, "/hostedzone/")
}

func changeBatch(action, name, rtype, value string) string {
	return `<ChangeResourceRecordSetsRequest xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><ChangeBatch><Changes><Change>
  <Action>` + action + `</Action>
  <ResourceRecordSet><Name>` + name + `</Name><Type>` + rtype + `</Type><TTL>300</TTL>
    <ResourceRecords><ResourceRecord><Value>` + value + `</Value></ResourceRecord></ResourceRecords>
  </ResourceRecordSet></Change></Changes></ChangeBatch></ChangeResourceRecordSetsRequest>`
}

func TestHostedZoneLifecycleOverXML(t *testing.T) {
	svc := newTestService(t)
	id := createZone(t, svc)

	var change struct {
		ChangeInfo struct {
			ID     string `xml:"Id"`
			Status string `xml:"Status"`
		} `xml:"ChangeInfo"`
	}
	mustServe(t, svc, http.MethodPost, "/2013-04-01/hostedzone/"+id+"/rrset/", changeBatch("CREATE", "api.example.com", "A", "10.0.0.7"), &change)
	if change.ChangeInfo.Status != "INSYNC" {
		t.Fatalf("changes apply at once: %+v", change)
	}
	var got struct {
		ChangeInfo struct {
			Status string `xml:"Status"`
		} `xml:"ChangeInfo"`
	}
	mustServe(t, svc, http.MethodGet, "/2013-04-01"+change.ChangeInfo.ID, "", &got)
	if got.ChangeInfo.Status != "INSYNC" {
		t.Fatalf("GetChange should report INSYNC: %+v", got)
	}

	var list struct {
		Sets []struct {
			Name   string   `xml:"Name"`
			Type   string   `xml:"Type"`
			Values []string `xml:"ResourceRecords>ResourceRecord>Value"`
		} `xml:"ResourceRecordSets>ResourceRecordSet"`
	}
	mustServe(t, svc, http.MethodGet, "/2013-04-01/hostedzone/"+id+"/rrset", "", &list)
	if len(list.Sets) != 3 || list.Sets[2].Name != "api.example.com." || list.Sets[2].Values[0] != "10.0.0.7" {
		t.Fatalf("expected NS, SOA and the new A record: %+v", list.Sets)
	}

	_, err := serve(t, svc, http.MethodDelete, "/2013-04-01/hostedzone/"+id, "")
	requireAPICode(t, err, "HostedZoneNotEmpty")
	mustServe(t, svc, http.MethodPost, "/2013-04-01/hostedzone/"+id+"/rrset", changeBatch("DELETE", "api.example.com", "A", "10.0.0.7"), nil)
	mustServe(t, svc, http.MethodDelete, "/2013-04-01/hostedzone/"+id, "", nil)
	_, err = serve(t, svc, http.MethodGet, "/2013-04-01/hostedzone/"+id, "")
	requireAPICode(t, err, "NoSuchHostedZone")
}

func TestGetHostedZoneReportsDelegationOrVPCs(t *testing.T) {
	svc := newTestService(t)
	public := createZone(t, svc)
	private := `<CreateHostedZoneRequest><Name>internal.test</Name><CallerReference>ref-2</CallerReference>
	  <VPC><VPCRegion>us-east-1</VPCRegion><VPCId>vpc-1</VPCId></VPC><HostedZoneConfig><PrivateZone>true</PrivateZone></HostedZoneConfig></CreateHostedZoneRequest>`
	var created struct {
		ID string `xml:"HostedZone>Id"`
	}
	mustServe(t, svc, http.MethodPost, "/2013-04-01/hostedzone", private, &created)

	type details struct {
		Private     bool     `xml:"HostedZone>Config>PrivateZone"`
		NameServers []string `xml:"DelegationSet>NameServers>NameServer"`
		VPCs        []string `xml:"VPCs>VPC>VPCId"`
	}
	var pub, priv details
	mustServe(t, svc, http.MethodGet, "/2013-04-01/hostedzone/"+public, "", &pub)
	mustServe(t, svc, http.MethodGet, "/2013-04-01"+created.ID, "", &priv)
	if pub.Private || len(pub.NameServers) != 4 || len(pub.VPCs) != 0 {
		t.Fatalf("public zone should report its name servers: %+v", pub)
	}
	if !priv.Private || len(priv.NameServers) != 0 || len(priv.VPCs) != 1 {
		t.Fatalf("private zone should report its VPC: %+v", priv)
	}
}

func TestCreateHostedZoneRejectsReusedCallerReference(t *testing.T) {
	svc := newTestService(t)
	createZone(t, svc)

	_, err := serve(t, svc, http.MethodPost, "/2013-04-01/hostedzone", createZoneBody)
	requireAPICode(t, err, "HostedZoneAlreadyExists")
}

func TestListHostedZonesPagesAndFiltersByName(t *testing.T) {
	svc := newTestService(t)
	for i, name := range []string{"b.test", "a.test", "c.test"} {
		body := strings.Replace(strings.Replace(createZoneBody, "example.com", name, 1), "ref-1", "ref-"+string(rune('a'+i)), 1)
		mustServe(t, svc, http.MethodPost, "/2013-04-01/hostedzone", body, nil)
	}

	var page struct {
		Names       []string `xml:"HostedZones>HostedZone>Name"`
		IsTruncated bool     `xml:"IsTruncated"`
		NextMarker  string   `xml:"NextMarker"`
	}
	mustServe(t, svc, http.MethodGet, "/2013-04-01/hostedzone?maxitems=2", "", &page)
	if strings.Join(page.Names, ",") != "a.test.,b.test." || !page.IsTruncated || page.NextMarker == "" {
		t.Fatalf("first page: %+v", page)
	}
	var rest struct {
		Names []string `xml:"HostedZones>HostedZone>Name"`
	}
	mustServe(t, svc, http.MethodGet, "/2013-04-01/hostedzone?marker="+page.NextMarker, "", &rest)
	if strings.Join(rest.Names, ",") != "c.test." {
		t.Fatalf("second page: %+v", rest)
	}

	var byName struct {
		Names []string `xml:"HostedZones>HostedZone>Name"`
	}
	mustServe(t, svc, http.MethodGet, "/2013-04-01/hostedzonesbyname?dnsname=b.test", "", &byName)
	if strings.Join(byName.Names, ",") != "b.test.,c.test." {
		t.Fatalf("by name should start at b.test: %+v", byName)
	}
}

func TestTagsAreAddedAndRemoved(t *testing.T) {
	svc := newTestService(t)
	id := createZone(t, svc)
	tagsPath := "/2013-04-01/tags/hostedzone/" + id

	mustServe(t, svc, http.MethodPost, tagsPath, `<ChangeTagsForResourceRequest><AddTags><Tag><Key>env</Key><Value>test</Value></Tag><Tag><Key>team</Key><Value>x</Value></Tag></AddTags></ChangeTagsForResourceRequest>`, nil)
	mustServe(t, svc, http.MethodPost, tagsPath, `<ChangeTagsForResourceRequest><RemoveTagKeys><Key>team</Key></RemoveTagKeys></ChangeTagsForResourceRequest>`, nil)

	var out struct {
		Keys []string `xml:"ResourceTagSet>Tags>Tag>Key"`
	}
	mustServe(t, svc, http.MethodGet, tagsPath, "", &out)
	if strings.Join(out.Keys, ",") != "env" {
		t.Fatalf("tags = %v", out.Keys)
	}
}

func TestManagedZonesAndRecordsForCloudMap(t *testing.T) {
	svc := newTestService(t)
	id, err := svc.CreateManagedZone("orders.local", true, "vpc-1", "Cloud Map namespace")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetRecord(id, "api.orders.local", "A", 60, "i-1", true, []string{"10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetRecord(id, "api.orders.local", "A", 60, "i-2", true, []string{"10.0.0.2"}); err != nil {
		t.Fatal(err)
	}

	res, _ := svc.Resolve("api.orders.local", "A")
	if len(values(res, "A")) != 2 {
		t.Fatalf("both instances should resolve: %+v", res)
	}
	if err := svc.DeleteRecord(id, "api.orders.local", "A", "i-1"); err != nil {
		t.Fatal(err)
	}
	if res, _ = svc.Resolve("api.orders.local", "A"); len(values(res, "A")) != 1 {
		t.Fatalf("deregistered instance should not resolve: %+v", res)
	}
	zone, _ := svc.Zone(id)
	if zone.ManagedBy == "" || !zone.Private {
		t.Fatalf("zone should be private and marked as Cloud Map's: %+v", zone)
	}
	if err := svc.DeleteManagedZone(id); err != nil {
		t.Fatalf("managed zones delete even with records: %v", err)
	}
	if err := svc.DeleteManagedZone(id); err != nil {
		t.Fatalf("deleting a deleted zone is not an error: %v", err)
	}
}

func TestOperationRouting(t *testing.T) {
	cases := map[string]string{
		"POST /2013-04-01/hostedzone":           "CreateHostedZone",
		"GET /2013-04-01/hostedzone/Z1":         "GetHostedZone",
		"POST /2013-04-01/hostedzone/Z1/rrset/": "ChangeResourceRecordSets",
		"GET /2013-04-01/hostedzone/Z1/rrset":   "ListResourceRecordSets",
		"GET /2013-04-01/change/C1":             "GetChange",
		"GET /2013-04-01/hostedzonesbyname":     "ListHostedZonesByName",
		"POST /2013-04-01/tags/hostedzone/Z1":   "ChangeTagsForResource",
		"GET /2013-04-01/healthcheck":           "",
		"GET /2015-03-31/functions":             "",
	}
	for in, want := range cases {
		method, path, _ := strings.Cut(in, " ")
		if got := Operation(method, path); got != want {
			t.Errorf("Operation(%s) = %q, want %q", in, got, want)
		}
	}
}
