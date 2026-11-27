package limits

import (
	"strings"
	"testing"
)

func TestDocumentedQuotasAreUniqueSourcedAndPositive(t *testing.T) {
	seen := map[string]bool{}
	for _, q := range Documented() {
		if seen[q.ID] {
			t.Errorf("duplicate quota %s", q.ID)
		}
		seen[q.ID] = true
		if q.Value <= 0 || q.Service == "" || q.Unit == "" || q.Per == "" {
			t.Errorf("%s is incomplete: %+v", q.ID, q)
		}
		if !strings.HasPrefix(q.Source, "https://docs.aws.amazon.com/") {
			t.Errorf("%s has no AWS documentation source: %q", q.ID, q.Source)
		}
		for region, v := range q.RegionValues {
			if v <= 0 {
				t.Errorf("%s: %s value %v", q.ID, region, v)
			}
		}
	}
}

func TestADocumentedQuotaTakesItsRegionalValueWhereAWSSetsOne(t *testing.T) {
	q, ok := DocumentedQuota("sns-publish-rate")
	if !ok {
		t.Fatal("sns-publish-rate missing")
	}
	for region, want := range map[string]float64{"us-east-1": 30000, "eu-west-1": 9000, "eu-central-1": 1500, "af-south-1": 300} {
		if got := q.In(region); got != want {
			t.Errorf("%s: %v, want %v", region, got, want)
		}
	}
	if _, ok := DocumentedQuota("no-such-quota"); ok {
		t.Error("unknown quota found")
	}
}
