package route53

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
)

func ttlOf(v int64) *int64 { return &v }

func a(name string, values ...string) RecordSet {
	return RecordSet{Name: name, Type: "A", TTL: ttlOf(300), Values: values}
}

func testZone(t *testing.T) HostedZone {
	t.Helper()
	z, err := newHostedZone("ZTEST", "Example.com", "ref-1", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func requireAPICode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func TestNewHostedZoneHasApexSOAAndNS(t *testing.T) {
	z := testZone(t)

	if z.Name != "example.com." {
		t.Fatalf("zone name should be canonical, got %q", z.Name)
	}
	if len(z.RecordSets) != 2 || z.RecordSets[0].Type != "NS" || z.RecordSets[1].Type != "SOA" {
		t.Fatalf("expected NS and SOA records, got %+v", z.RecordSets)
	}
	if z.hasUserRecords() {
		t.Fatal("a new zone holds only required records")
	}
}

func TestApplyCreatesUpsertsAndDeletes(t *testing.T) {
	z := testZone(t)

	if err := z.apply([]Change{{ActionCreate, a("WWW.example.com", "10.0.0.1")}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := z.apply([]Change{{ActionUpsert, a("www.example.com.", "10.0.0.2")}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := withName(z, "www.example.com."); len(got) != 1 || got[0].Values[0] != "10.0.0.2" {
		t.Fatalf("upsert should replace the record: %+v", got)
	}
	if err := z.apply([]Change{{ActionDelete, a("www.example.com.", "10.0.0.2")}}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if z.hasUserRecords() {
		t.Fatalf("record should be gone: %+v", z.RecordSets)
	}
}

func TestApplyIsAtomic(t *testing.T) {
	z := testZone(t)
	err := z.apply([]Change{
		{ActionCreate, a("ok.example.com", "10.0.0.1")},
		{ActionDelete, a("missing.example.com", "10.0.0.9")},
	})

	requireAPICode(t, err, "InvalidChangeBatch")
	if z.hasUserRecords() {
		t.Fatal("a failed batch must not apply any change")
	}
}

func TestApplyRejectsInvalidChanges(t *testing.T) {
	cname := RecordSet{Name: "www.example.com", Type: "CNAME", TTL: ttlOf(60), Values: []string{"web.example.com"}}
	cases := map[string]struct {
		setup  []Change
		change Change
		code   string
	}{
		"create existing":   {[]Change{{ActionCreate, a("x.example.com", "1.1.1.1")}}, Change{ActionCreate, a("x.example.com", "1.1.1.1")}, "InvalidChangeBatch"},
		"delete mismatched": {[]Change{{ActionCreate, a("x.example.com", "1.1.1.1")}}, Change{ActionDelete, a("x.example.com", "2.2.2.2")}, "InvalidChangeBatch"},
		"outside zone":      {nil, Change{ActionCreate, a("x.example.org", "1.1.1.1")}, "InvalidChangeBatch"},
		"cname conflict":    {[]Change{{ActionCreate, a("www.example.com", "1.1.1.1")}}, Change{ActionCreate, cname}, "InvalidChangeBatch"},
		"no values":         {nil, Change{ActionCreate, RecordSet{Name: "x.example.com", Type: "A"}}, "InvalidChangeBatch"},
		"unknown action":    {nil, Change{"REPLACE", a("x.example.com", "1.1.1.1")}, "InvalidInput"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			z := testZone(t)
			if tc.setup != nil {
				if err := z.apply(tc.setup); err != nil {
					t.Fatal(err)
				}
			}
			requireAPICode(t, z.apply([]Change{tc.change}), tc.code)
		})
	}
}

func TestApexNSAndSOACannotBeDeleted(t *testing.T) {
	z := testZone(t)
	ns := z.RecordSets[0]

	requireAPICode(t, z.apply([]Change{{ActionDelete, ns}}), "InvalidChangeBatch")
}

func TestRecordSetsSortByReversedLabels(t *testing.T) {
	z := testZone(t)
	if err := z.apply([]Change{
		{ActionCreate, a("b.example.com", "1.1.1.1")},
		{ActionCreate, a("a.z.example.com", "1.1.1.1")},
		{ActionCreate, a("z.a.example.com", "1.1.1.1")},
	}); err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, rs := range z.RecordSets {
		names = append(names, rs.Name+" "+rs.Type)
	}
	want := "example.com. NS,example.com. SOA,z.a.example.com. A,b.example.com. A,a.z.example.com. A"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
}

func TestWildcardNamesAreEscapedOnOutput(t *testing.T) {
	name, err := normalizeName(`\052.example.com`)
	if err != nil || name != "*.example.com." {
		t.Fatalf("escaped wildcard should normalize to *: %q %v", name, err)
	}
	if display(name) != `\052.example.com.` {
		t.Fatalf("wildcard should be escaped on output, got %q", display(name))
	}
}
