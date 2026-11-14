package loadrun

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Hot partition thresholds. A key is hot when it takes a quarter of a
// table's traffic, or most of its throttles, once the table has seen
// enough traffic to tell.
const (
	hotMinRequests    = 100
	hotShare          = 0.25
	hotThrottleShare  = 0.5
	hotMinThrottles   = 10
	hotTrackedKeys    = 32  // heavy hitters kept per table
	hotTrackedTables  = 200 // tables tracked per run
	hotReportedKeys   = 3   // hot keys reported per table
	hotMaxKeyInReport = 64  // characters of a key value shown
)

// hotPartitions finds partition keys that take a disproportionate share
// of a table's traffic or throttles. Each table keeps its heaviest keys
// with the Space-Saving algorithm, so memory is bounded however many
// distinct keys the run touches, and a key's count is over by at most the
// error recorded with it.
type hotPartitions struct {
	tables map[string]*tableKeys
}

type tableKeys struct {
	requests, throttled int
	keys                map[string]*keyCount
}

type keyCount struct {
	count, err, throttled int
}

func newHotPartitions() *hotPartitions { return &hotPartitions{tables: map[string]*tableKeys{}} }

func (h *hotPartitions) observe(o Observation, oc outcome, _ int) {
	if o.Service != "dynamodb" {
		return
	}
	throttled := oc == outcomeThrottled
	seen := map[string]bool{}
	for _, k := range o.Keys {
		if k.Partition == "" {
			continue
		}
		t := h.tables[k.Resource]
		if t == nil {
			if len(h.tables) >= hotTrackedTables {
				continue
			}
			t = &tableKeys{keys: map[string]*keyCount{}}
			h.tables[k.Resource] = t
		}
		// A request counts once per table, and once per key in it.
		if !seen[k.Resource] {
			seen[k.Resource] = true
			t.requests++
			if throttled {
				t.throttled++
			}
		}
		if id := k.Resource + "\x00" + k.Partition; !seen[id] {
			seen[id] = true
			t.add(k.Partition, throttled)
		}
	}
}

func (t *tableKeys) add(key string, throttled bool) {
	kc := t.keys[key]
	if kc == nil {
		if len(t.keys) < hotTrackedKeys {
			kc = &keyCount{}
		} else {
			// Replace the smallest counter, inheriting its count as error.
			var minKey string
			var min *keyCount
			for k, c := range t.keys {
				if min == nil || c.count < min.count || c.count == min.count && k < minKey {
					minKey, min = k, c
				}
			}
			delete(t.keys, minKey)
			kc = &keyCount{count: min.count, err: min.count}
		}
		t.keys[key] = kc
	}
	kc.count++
	if throttled {
		kc.throttled++
	}
}

func (h *hotPartitions) findings(runFacts) []Finding {
	var out []Finding
	for table, t := range h.tables {
		if t.requests < hotMinRequests {
			continue
		}
		type hot struct {
			key string
			c   *keyCount
		}
		var hots []hot
		for k, c := range t.keys {
			share := float64(c.count-c.err) / float64(t.requests)
			throttleShare := 0.0
			if t.throttled > 0 {
				throttleShare = float64(c.throttled) / float64(t.throttled)
			}
			if share >= hotShare || c.throttled >= hotMinThrottles && throttleShare >= hotThrottleShare {
				hots = append(hots, hot{k, c})
			}
		}
		sort.Slice(hots, func(i, j int) bool {
			if hots[i].c.count != hots[j].c.count {
				return hots[i].c.count > hots[j].c.count
			}
			return hots[i].key < hots[j].key
		})
		if len(hots) > hotReportedKeys {
			hots = hots[:hotReportedKeys]
		}
		for _, hk := range hots {
			share := float64(hk.c.count-hk.c.err) / float64(t.requests)
			f := Finding{
				Detector: DetectorHotPartition, Severity: SeverityMedium, Service: "dynamodb", Resource: table,
				Title: fmt.Sprintf("Hot partition key \"%s\" on table %s", shortKey(hk.key), table),
				Detail: fmt.Sprintf("At least %d of %d requests to %s (%s) went to one partition key, and %d of them were throttled.",
					hk.c.count-hk.c.err, t.requests, table, pct(share), hk.c.throttled),
				Suggestion: "A partition serves at most 3,000 reads and 1,000 writes per second, whatever the table's capacity. " +
					"Spread writes by adding a suffix to the key (write sharding), cache reads of this item, or pick a key with more distinct values.",
				Evidence: map[string]any{
					"partition_key": shortKey(hk.key), "key_requests": hk.c.count - hk.c.err, "table_requests": t.requests,
					"share": round(share), "key_throttled": hk.c.throttled, "table_throttled": t.throttled,
				},
			}
			if hk.c.throttled > 0 {
				f.Severity = SeverityHigh
			}
			out = append(out, f)
		}
	}
	return out
}

// shortKey shows a partition key value readably: DynamoDB's canonical
// form S:"abc" becomes abc and N:5 becomes 5, trimmed to a sensible length.
func shortKey(k string) string {
	switch {
	case strings.HasPrefix(k, `S:"`):
		if v, err := strconv.Unquote(k[2:]); err == nil {
			k = v
		}
	case strings.HasPrefix(k, "N:"), strings.HasPrefix(k, "B:"):
		k = k[2:]
	}
	if len(k) > hotMaxKeyInReport {
		return k[:hotMaxKeyInReport] + "..."
	}
	return k
}
