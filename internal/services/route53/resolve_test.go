package route53

import (
	"testing"
	"time"
)

func zoneWith(t *testing.T, id, name string, private bool, sets ...RecordSet) HostedZone {
	t.Helper()
	z, err := newHostedZone(id, name, id, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	z.Private = private
	changes := make([]Change, 0, len(sets))
	for _, rs := range sets {
		changes = append(changes, Change{ActionCreate, rs})
	}
	if len(changes) > 0 {
		if err := z.apply(changes); err != nil {
			t.Fatal(err)
		}
	}
	return z
}

func values(res Resolution, rtype string) []string {
	var out []string
	for _, ans := range res.Answers {
		if ans.Type == rtype {
			out = append(out, ans.Value)
		}
	}
	return out
}

func TestResolveReturnsEveryValueOfEveryMatchingSet(t *testing.T) {
	one := a("api.example.com", "10.0.0.1")
	one.SetIdentifier = "i-1"
	two := a("api.example.com", "10.0.0.2", "10.0.0.3")
	two.SetIdentifier = "i-2"
	zones := []HostedZone{zoneWith(t, "Z1", "example.com", false, one, two)}

	res, err := resolve(zones, "API.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Found || res.Type != "A" || len(values(res, "A")) != 3 {
		t.Fatalf("expected all three addresses: %+v", res)
	}
}

func TestResolveFollowsCNAMEAndAliasAcrossZones(t *testing.T) {
	zones := []HostedZone{
		zoneWith(t, "Z1", "example.com", false,
			RecordSet{Name: "www.example.com", Type: "CNAME", TTL: ttlOf(60), Values: []string{"web.internal.test"}},
			RecordSet{Name: "example.com", Type: "A", Alias: &AliasTarget{DNSName: "www.example.com"}},
		),
		zoneWith(t, "Z2", "internal.test", true, a("web.internal.test", "10.1.0.5")),
	}

	viaCNAME, _ := resolve(zones, "www.example.com", "A")
	if got := values(viaCNAME, "CNAME"); len(got) != 1 || got[0] != "web.internal.test" {
		t.Fatalf("CNAME step should be reported: %+v", viaCNAME)
	}
	if got := values(viaCNAME, "A"); len(got) != 1 || got[0] != "10.1.0.5" {
		t.Fatalf("CNAME target should be resolved: %+v", viaCNAME)
	}

	viaAlias, _ := resolve(zones, "example.com", "A")
	if got := values(viaAlias, "A"); len(got) != 1 || got[0] != "10.1.0.5" {
		t.Fatalf("apex alias should resolve through the CNAME: %+v", viaAlias)
	}
}

func TestResolveMatchesClosestWildcard(t *testing.T) {
	zones := []HostedZone{zoneWith(t, "Z1", "example.com", false,
		a("*.example.com", "10.0.0.1"),
		a("*.eu.example.com", "10.0.0.2"),
		a("exact.eu.example.com", "10.0.0.3"),
	)}

	cases := map[string]string{
		"anything.example.com":     "10.0.0.1",
		"shop.eu.example.com":      "10.0.0.2",
		"exact.eu.example.com":     "10.0.0.3",
		"deep.shop.eu.example.com": "10.0.0.2",
	}
	for name, want := range cases {
		res, _ := resolve(zones, name, "A")
		if got := values(res, "A"); len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %v, want %s", name, got, want)
		}
	}
}

func TestResolvePrefersMostSpecificAndPrivateZones(t *testing.T) {
	zones := []HostedZone{
		zoneWith(t, "Z1", "example.com", false, a("db.svc.example.com", "1.1.1.1")),
		zoneWith(t, "Z2", "svc.example.com", false, a("db.svc.example.com", "2.2.2.2")),
		zoneWith(t, "Z3", "svc.example.com", true, a("db.svc.example.com", "3.3.3.3")),
	}

	res, _ := resolve(zones, "db.svc.example.com", "A")
	if got := values(res, "A"); len(got) != 1 || got[0] != "3.3.3.3" || res.Answers[0].ZoneID != "Z3" {
		t.Fatalf("the private, most specific zone should answer: %+v", res)
	}
}

func TestResolveReportsMissingNamesAndTypes(t *testing.T) {
	zones := []HostedZone{zoneWith(t, "Z1", "example.com", false, a("www.example.com", "1.1.1.1"))}

	for _, q := range [][2]string{{"www.example.com", "AAAA"}, {"nope.example.com", "A"}, {"www.other.test", "A"}} {
		res, err := resolve(zones, q[0], q[1])
		if err != nil || res.Found || len(res.Answers) != 0 {
			t.Errorf("%s %s should find nothing: %+v %v", q[0], q[1], res, err)
		}
	}
}

func TestResolveStopsOnCNAMELoops(t *testing.T) {
	zones := []HostedZone{zoneWith(t, "Z1", "example.com", false,
		RecordSet{Name: "a.example.com", Type: "CNAME", TTL: ttlOf(60), Values: []string{"b.example.com"}},
		RecordSet{Name: "b.example.com", Type: "CNAME", TTL: ttlOf(60), Values: []string{"a.example.com"}},
	)}

	res, err := resolve(zones, "a.example.com", "A")
	if err != nil || res.Found || len(res.Answers) != maxHops {
		t.Fatalf("a loop should end after %d steps: %+v %v", maxHops, res, err)
	}
}
