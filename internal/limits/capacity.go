package limits

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"
)

// KindCapacity is a DynamoDB table or partition running out of read or
// write capacity units.
const KindCapacity Kind = "capacity"

// Throttling reasons, named as DynamoDB names them in ThrottlingReasons.
const (
	ReasonTableReadProvisioned  = "TableReadProvisionedThroughputExceeded"
	ReasonTableWriteProvisioned = "TableWriteProvisionedThroughputExceeded"
	ReasonTableReadKeyRange     = "TableReadKeyRangeThroughputExceeded"
	ReasonTableWriteKeyRange    = "TableWriteKeyRangeThroughputExceeded"
	ReasonTableReadMaxOnDemand  = "TableReadMaxOnDemandThroughputExceeded"
	ReasonTableWriteMaxOnDemand = "TableWriteMaxOnDemandThroughputExceeded"
)

// Rule IDs of the capacity buckets, as breaches and status report them.
const (
	RuleTableCapacity     = "dynamodb-table-capacity"
	RulePartitionCapacity = "dynamodb-partition-capacity"
)

// DynamoDB defaults: what one partition serves, the most an on-demand table
// serves by default, how long a provisioned table can bank unused capacity,
// and how many partitions a new on-demand table starts with.
const (
	defaultPartitionReadUnits  = 3000
	defaultPartitionWriteUnits = 1000
	defaultOnDemandReadUnits   = 40000
	defaultOnDemandWriteUnits  = 40000
	defaultBurstSeconds        = 300
	defaultOnDemandPartitions  = 4
	maxPartitions              = 1000
)

// DynamoDBSpec tunes DynamoDB table capacity. Every field is optional.
type DynamoDBSpec struct {
	// Disabled turns capacity enforcement off while other limits apply.
	Disabled bool `json:"disabled,omitempty"`
	// PartitionReadUnits and PartitionWriteUnits are what one partition
	// serves per second, before scaling: 3000 and 1000 in AWS.
	PartitionReadUnits  float64 `json:"partition_read_units,omitempty"`
	PartitionWriteUnits float64 `json:"partition_write_units,omitempty"`
	// OnDemandReadUnits and OnDemandWriteUnits cap an on-demand table per
	// second, before scaling: 40000 each by default in AWS.
	OnDemandReadUnits  float64 `json:"on_demand_read_units,omitempty"`
	OnDemandWriteUnits float64 `json:"on_demand_write_units,omitempty"`
	// BurstSeconds is how many seconds of unused provisioned capacity a
	// table banks for bursts: 300 in AWS. Set 1 to see throttles at once.
	BurstSeconds float64 `json:"burst_seconds,omitempty"`
	// Tables sets the partition count of named tables.
	Tables map[string]TableSpec `json:"tables,omitempty"`
}

// TableSpec overrides how one table is laid out.
type TableSpec struct {
	// Partitions fixes the table's partition count; zero derives it.
	Partitions int `json:"partitions,omitempty"`
}

// normalise validates a DynamoDB section and fills in the defaults.
func (d DynamoDBSpec) normalise() (DynamoDBSpec, error) {
	for name, v := range map[string]float64{
		"partition_read_units": d.PartitionReadUnits, "partition_write_units": d.PartitionWriteUnits,
		"on_demand_read_units": d.OnDemandReadUnits, "on_demand_write_units": d.OnDemandWriteUnits,
		"burst_seconds": d.BurstSeconds,
	} {
		if v < 0 {
			return DynamoDBSpec{}, invalid("InvalidLimitsProfile", "dynamodb.%s must not be negative", name)
		}
	}
	tables := make(map[string]TableSpec, len(d.Tables))
	for name, t := range d.Tables {
		if t.Partitions < 0 || t.Partitions > maxPartitions {
			return DynamoDBSpec{}, invalid("InvalidLimitsProfile", "dynamodb.tables.%s.partitions must be between 1 and %d", name, maxPartitions)
		}
		tables[name] = t
	}
	d.Tables = tables
	d.PartitionReadUnits = orDefault(d.PartitionReadUnits, defaultPartitionReadUnits)
	d.PartitionWriteUnits = orDefault(d.PartitionWriteUnits, defaultPartitionWriteUnits)
	d.OnDemandReadUnits = orDefault(d.OnDemandReadUnits, defaultOnDemandReadUnits)
	d.OnDemandWriteUnits = orDefault(d.OnDemandWriteUnits, defaultOnDemandWriteUnits)
	d.BurstSeconds = orDefault(d.BurstSeconds, defaultBurstSeconds)
	return d, nil
}

func orDefault(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

// CapacityRequest is one read or write against a DynamoDB table.
type CapacityRequest struct {
	Request
	// OnDemand is true for PAY_PER_REQUEST tables.
	OnDemand bool
	// ReadCapacity and WriteCapacity are the provisioned units of a
	// PROVISIONED table.
	ReadCapacity, WriteCapacity float64
	// PartitionKey identifies the item's partition; empty for a Scan,
	// which spreads across every partition and meets only the table cap.
	PartitionKey string
	// Units is what the request consumes, in read or write units.
	Units float64
	Write bool
}

// tableCapacity is the capacity model of one table under a profile.
type tableCapacity struct {
	spec  DynamoDBSpec
	scale float64
	req   CapacityRequest
}

func (c tableCapacity) partitionRate() float64 {
	if c.req.Write {
		return c.spec.PartitionWriteUnits * c.scale
	}
	return c.spec.PartitionReadUnits * c.scale
}

// partitions is how many partitions the table has: as configured, or, for
// a provisioned table, enough for its partitions to serve what it was
// provisioned, or four for an on-demand table, as AWS starts them.
func (c tableCapacity) partitions() int {
	if t, ok := c.spec.Tables[c.req.Resource]; ok && t.Partitions > 0 {
		return t.Partitions
	}
	if c.req.OnDemand {
		return defaultOnDemandPartitions
	}
	need := c.req.ReadCapacity/(c.spec.PartitionReadUnits*c.scale) + c.req.WriteCapacity/(c.spec.PartitionWriteUnits*c.scale)
	return int(math.Min(maxPartitions, math.Max(1, math.Ceil(need))))
}

// tableRate is the table's sustained rate and burst size. Provisioned
// capacity is what the table owner chose, so it is never scaled.
func (c tableCapacity) tableRate() (rate, capacity float64) {
	switch {
	case c.req.OnDemand && c.req.Write:
		rate = c.spec.OnDemandWriteUnits * c.scale
	case c.req.OnDemand:
		rate = c.spec.OnDemandReadUnits * c.scale
	case c.req.Write:
		rate, capacity = c.req.WriteCapacity, c.req.WriteCapacity*c.spec.BurstSeconds
	default:
		rate, capacity = c.req.ReadCapacity, c.req.ReadCapacity*c.spec.BurstSeconds
	}
	return rate, math.Max(1, math.Max(capacity, rate))
}

func (c tableCapacity) tableReason() string {
	switch {
	case c.req.OnDemand && c.req.Write:
		return ReasonTableWriteMaxOnDemand
	case c.req.OnDemand:
		return ReasonTableReadMaxOnDemand
	case c.req.Write:
		return ReasonTableWriteProvisioned
	}
	return ReasonTableReadProvisioned
}

func (c tableCapacity) partitionReason() string {
	if c.req.Write {
		return ReasonTableWriteKeyRange
	}
	return ReasonTableReadKeyRange
}

func (c tableCapacity) direction() string {
	if c.req.Write {
		return "write"
	}
	return "read"
}

func (c tableCapacity) tableKey() QuotaKey {
	return QuotaKey{RuleID: RuleTableCapacity, Account: c.req.Account, Region: c.req.Region, Resource: c.req.Resource + "/" + c.direction()}
}

// partitionKey hashes the item's partition key onto one of the table's
// partitions, as DynamoDB does.
func (c tableCapacity) partitionKey(n int) QuotaKey {
	h := fnv.New32a()
	_, _ = h.Write([]byte(c.req.PartitionKey))
	index := int(h.Sum32() % uint32(n))
	return QuotaKey{RuleID: RulePartitionCapacity, Account: c.req.Account, Region: c.req.Region,
		Resource: fmt.Sprintf("%s/%s/p%d-of-%d", c.req.Resource, c.direction(), index, n)}
}

// demand is one bucket a request draws on, and the breach it raises when
// the bucket is short.
type demand struct {
	key      QuotaKey
	rate     float64
	capacity float64
	units    float64
	reason   string
	request  int
}

func (c tableCapacity) demands(request int) []demand {
	rate, capacity := c.tableRate()
	out := []demand{{key: c.tableKey(), rate: rate, capacity: capacity, units: c.req.Units, reason: c.tableReason(), request: request}}
	if c.req.PartitionKey != "" {
		pr := c.partitionRate()
		out = append(out, demand{key: c.partitionKey(c.partitions()), rate: pr, capacity: math.Max(1, pr),
			units: c.req.Units, reason: c.partitionReason(), request: request})
	}
	return out
}

// capacityStats counts one table's traffic.
type capacityStats struct {
	table          string
	account        string
	region         string
	readUnits      float64
	writeUnits     float64
	readThrottled  int
	writeThrottled int
	partitions     int
}

// ConsumeCapacity draws read or write units from DynamoDB tables for a
// set of requests that succeed or fail together, as a transaction does.
// It returns nil when every request was admitted and its units consumed.
// Otherwise nothing is consumed, and the result holds a breach for each
// request whose table or partition is short (nil for the others).
//
// A request larger than a whole bucket is admitted when the bucket is
// full and leaves it in debt, so a big item is slowed, never refused
// forever.
func (g *Governor) ConsumeCapacity(reqs []CapacityRequest) []*Breach {
	if g.active.Load() == nil || len(reqs) == 0 {
		return nil
	}
	g.mu.Lock()
	st := g.active.Load()
	if st == nil || st.profile.DynamoDB == nil {
		g.mu.Unlock()
		return nil
	}
	now := g.clock.Now()

	var all []demand
	models := make([]tableCapacity, len(reqs))
	for i, req := range reqs {
		models[i] = tableCapacity{spec: *st.profile.DynamoDB, scale: st.profile.Scale, req: req}
		all = append(all, models[i].demands(i)...)
	}

	// Requests in one call may share a bucket, so check the totals.
	totals := map[QuotaKey]float64{}
	for _, d := range all {
		totals[d.key] += d.units
	}
	var breaches []*Breach
	for _, d := range all {
		b := st.resizedBucket(d.key, d.rate, d.capacity, now)
		if b.has(now, math.Min(totals[d.key], b.capacity)) {
			continue
		}
		if breaches == nil {
			breaches = make([]*Breach, len(reqs))
		}
		if breaches[d.request] != nil {
			continue
		}
		req := reqs[d.request]
		breaches[d.request] = &Breach{
			Time: now.UTC(), RuleID: d.key.RuleID, Kind: KindCapacity, Key: d.key.String(), Limit: d.rate,
			Reason: d.reason, RequestID: req.RequestID, Service: req.Service, Operation: req.Operation, Resource: req.Resource,
		}
	}

	if breaches != nil {
		var recorded []Breach
		for i, b := range breaches {
			if b == nil {
				continue
			}
			st.capacityStats(models[i]).throttle(reqs[i].Write)
			st.record(*b)
			recorded = append(recorded, *b)
		}
		sinks := g.sinks
		g.mu.Unlock()
		for _, b := range recorded {
			notify(sinks, b)
		}
		return breaches
	}

	for _, d := range all {
		st.buckets[d.key].take(d.units)
	}
	for i, m := range models {
		st.capacityStats(m).consume(reqs[i].Write, reqs[i].Units)
	}
	g.mu.Unlock()
	return nil
}

// resizedBucket returns the bucket under key, adopting a new rate and size
// when they changed, as UpdateTable changes a table's capacity at once.
func (st *state) resizedBucket(key QuotaKey, rate, capacity float64, now time.Time) *tokenBucket {
	b := st.bucket(key, rate, capacity, now)
	if b.rate != rate || b.capacity != capacity {
		// Raised capacity is usable at once; lowered capacity drops what
		// no longer fits.
		b.refill(now)
		b.tokens = math.Min(capacity, b.tokens+math.Max(0, capacity-b.capacity))
		b.rate, b.capacity = rate, capacity
	}
	return b
}

func (st *state) capacityStats(m tableCapacity) *capacityStats {
	key := strings.Join([]string{m.req.Account, m.req.Region, m.req.Resource}, "/")
	s := st.capacity[key]
	if s == nil {
		s = &capacityStats{table: m.req.Resource, account: m.req.Account, region: m.req.Region}
		st.capacity[key] = s
	}
	s.partitions = m.partitions()
	return s
}

func (s *capacityStats) throttle(write bool) {
	if write {
		s.writeThrottled++
	} else {
		s.readThrottled++
	}
}

func (s *capacityStats) consume(write bool, units float64) {
	if write {
		s.writeUnits += units
	} else {
		s.readUnits += units
	}
}
