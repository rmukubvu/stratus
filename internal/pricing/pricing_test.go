package pricing

import (
	"strings"
	"testing"
)

func TestTheEmbeddedSnapshotIsDatedAndCoversUSEast1(t *testing.T) {
	s := Default()
	if len(s.Published) != len("2006-01-02") || !strings.HasPrefix(s.Source, "https://pricing.us-east-1.amazonaws.com/") {
		t.Fatalf("snapshot published %q from %q", s.Published, s.Source)
	}
	for _, id := range s.IDs() {
		p, _ := s.Lookup(id)
		if _, ok := p.In("us-east-1"); !ok || p.Unit == "" || p.Description == "" || p.ID != id {
			t.Errorf("%s is incomplete: %+v", id, p)
		}
	}
	if len(s.IDs()) == 0 {
		t.Fatal("no prices")
	}
}

func TestParseRejectsTiersWithGapsOrTwoUnboundedBands(t *testing.T) {
	bad := []string{
		`{"published":"2026-11-26","source":"x","prices":{"p":{"regions":{"r":[{"from":1,"to":0,"usd":1}]}}}}`,
		`{"published":"2026-11-26","source":"x","prices":{"p":{"regions":{"r":[{"from":0,"to":0,"usd":1},{"from":5,"to":0,"usd":1}]}}}}`,
		`{"published":"2026-11-26","source":"x","prices":{"p":{"regions":{"r":[{"from":0,"to":10,"usd":1}]}}}}`,
		`{"source":"x","prices":{}}`,
	}
	for _, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
