package limits

import (
	"fmt"
	"testing"
	"time"
)

func dynamoWrite(table, pk string, units float64) CapacityRequest {
	return CapacityRequest{
		Request:  Request{RequestID: "req-" + pk, Account: "111111111111", Region: "us-east-1", Service: "dynamodb", Operation: "PutItem", Resource: table},
		OnDemand: true, PartitionKey: pk, Units: units, Write: true,
	}
}

func provisioned(req CapacityRequest, rcu, wcu float64) CapacityRequest {
	req.OnDemand, req.ReadCapacity, req.WriteCapacity = false, rcu, wcu
	return req
}

func dynamoOnly(d DynamoDBSpec) Spec { return Spec{Profile: BaseNone, DynamoDB: &d} }

func consumeN(g *Governor, req CapacityRequest, n int) (admitted int, last *Breach) {
	for i := 0; i < n; i++ {
		if b := g.ConsumeCapacity([]CapacityRequest{req}); b == nil {
			admitted++
		} else {
			last = b[0]
		}
	}
	return admitted, last
}

func TestCapacityIsOffWithoutADynamoDBModel(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	if b := g.ConsumeCapacity([]CapacityRequest{dynamoWrite("orders", "a", 1e9)}); b != nil {
		t.Fatal("throttled with no profile")
	}
	_, _ = g.Apply(onlyRule(RateRule{ID: "ssm", Service: "ssm", PerSecond: 1}))
	if b := g.ConsumeCapacity([]CapacityRequest{dynamoWrite("orders", "a", 1e9)}); b != nil {
		t.Fatal("the none base enforced capacity without a dynamodb section")
	}
	_, _ = g.Apply(Spec{DynamoDB: &DynamoDBSpec{Disabled: true}})
	if b := g.ConsumeCapacity([]CapacityRequest{dynamoWrite("orders", "a", 1e9)}); b != nil {
		t.Fatal("disabled capacity still enforced")
	}
}

func TestProvisionedTableThrottlesPastItsCapacityAndBurst(t *testing.T) {
	clock := newFakeClock()
	g := NewGovernor(nil, clock)
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{BurstSeconds: seconds(2)}))

	// 5 WCU with 2 seconds of burst: 10 one-unit writes, then throttled.
	req := provisioned(dynamoWrite("orders", "a", 1), 5, 5)
	admitted, b := consumeN(g, req, 12)
	if admitted != 10 || b == nil {
		t.Fatalf("admitted %d, want 10 then a breach", admitted)
	}
	if b.Reason != ReasonTableWriteProvisioned || b.RuleID != RuleTableCapacity || b.Kind != KindCapacity || b.Limit != 5 {
		t.Fatalf("unexpected breach: %+v", b)
	}
	clock.Advance(time.Second)
	if admitted, _ := consumeN(g, req, 10); admitted != 5 {
		t.Fatalf("after a second admitted %d, want 5", admitted)
	}
}

func TestProvisionedCapacityIsNotScaled(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(Spec{Profile: BaseNone, Scale: 0.01, DynamoDB: &DynamoDBSpec{BurstSeconds: seconds(1)}})
	// Partitions are scaled to 10 WCU, so 50 provisioned WCU is spread
	// over five partitions and the table, not a partition, is the cap.
	admitted := 0
	for i := 0; i < 60; i++ {
		if g.ConsumeCapacity([]CapacityRequest{provisioned(dynamoWrite("orders", fmt.Sprint(i), 1), 1, 50)}) == nil {
			admitted++
		}
	}
	if admitted != 50 {
		t.Fatalf("admitted %d, want the 50 provisioned units", admitted)
	}
}

func TestHotPartitionKeyThrottlesWhileTheTableHasRoom(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{PartitionWriteUnits: 10}))

	admitted, b := consumeN(g, dynamoWrite("orders", "hot", 1), 20)
	if admitted != 10 || b == nil || b.Reason != ReasonTableWriteKeyRange || b.RuleID != RulePartitionCapacity {
		t.Fatalf("hot key: admitted %d, breach %+v", admitted, b)
	}

	// Other keys land on other partitions and still get through.
	cold := 0
	for i := 0; i < 20; i++ {
		if g.ConsumeCapacity([]CapacityRequest{dynamoWrite("orders", fmt.Sprintf("key-%d", i), 1)}) == nil {
			cold++
		}
	}
	if cold == 0 {
		t.Fatal("every other key was throttled with the hot one")
	}
}

func TestOnDemandTableMaxThrottles(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{OnDemandWriteUnits: 3}))
	admitted := 0
	var last *Breach
	for i := 0; i < 10; i++ {
		if b := g.ConsumeCapacity([]CapacityRequest{dynamoWrite("orders", fmt.Sprint(i), 1)}); b == nil {
			admitted++
		} else {
			last = b[0]
		}
	}
	if admitted != 3 || last.Reason != ReasonTableWriteMaxOnDemand {
		t.Fatalf("admitted %d, last breach %+v", admitted, last)
	}
}

func TestReadsAndWritesHaveSeparateCapacity(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{BurstSeconds: seconds(1)}))
	write := provisioned(dynamoWrite("orders", "a", 1), 1, 1)
	read := write
	read.Write = false
	if g.ConsumeCapacity([]CapacityRequest{write}) != nil || g.ConsumeCapacity([]CapacityRequest{read}) != nil {
		t.Fatal("a read and a write of one unit each should both fit")
	}
	b := g.ConsumeCapacity([]CapacityRequest{read})
	if b == nil || b[0].Reason != ReasonTableReadProvisioned {
		t.Fatalf("second read: %+v", b)
	}
}

func TestUpdatedCapacityAppliesAtOnce(t *testing.T) {
	clock := newFakeClock()
	g := NewGovernor(nil, clock)
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{BurstSeconds: seconds(1)}))
	if admitted, _ := consumeN(g, provisioned(dynamoWrite("orders", "a", 1), 1, 1), 3); admitted != 1 {
		t.Fatalf("1 WCU admitted %d", admitted)
	}
	clock.Advance(time.Second)
	if admitted, _ := consumeN(g, provisioned(dynamoWrite("orders", "a", 1), 1, 10), 20); admitted != 10 {
		t.Fatalf("after raising to 10 WCU admitted %d, want 10", admitted)
	}
}

func TestBigItemIsSlowedNotRefusedForever(t *testing.T) {
	clock := newFakeClock()
	g := NewGovernor(nil, clock)
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{BurstSeconds: seconds(1)}))
	big := provisioned(dynamoWrite("orders", "a", 400), 1, 5)
	if g.ConsumeCapacity([]CapacityRequest{big}) != nil {
		t.Fatal("a 400 WCU write on a full 5 WCU table should be admitted into debt")
	}
	clock.Advance(time.Second)
	if g.ConsumeCapacity([]CapacityRequest{big}) == nil {
		t.Fatal("the table should still be in debt a second later")
	}
}

func TestConsumeCapacityIsAllOrNothing(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{BurstSeconds: seconds(1)}))
	a := provisioned(dynamoWrite("orders", "a", 1), 1, 2)
	busy := provisioned(dynamoWrite("audit", "x", 1), 1, 1)
	if g.ConsumeCapacity([]CapacityRequest{busy}) != nil {
		t.Fatal("first audit write throttled")
	}

	got := g.ConsumeCapacity([]CapacityRequest{a, busy})
	if got == nil || got[0] != nil || got[1] == nil {
		t.Fatalf("want only the audit write to breach: %+v", got)
	}
	// Nothing was taken from orders, so both its units are still there.
	if g.ConsumeCapacity([]CapacityRequest{a, a}) != nil {
		t.Fatal("orders lost capacity to a cancelled transaction")
	}
	// Two requests in one call share the bucket and are checked together.
	if got := g.ConsumeCapacity([]CapacityRequest{a, a}); got == nil {
		t.Fatal("two writes over a 2 WCU table with nothing left were admitted")
	}
}

func TestCapacityStatusCountsUnitsAndThrottles(t *testing.T) {
	g := NewGovernor(nil, newFakeClock())
	_, _ = g.Apply(dynamoOnly(DynamoDBSpec{BurstSeconds: seconds(1), Tables: map[string]TableSpec{"orders": {Partitions: 7}}}))
	consumeN(g, provisioned(dynamoWrite("orders", "a", 2), 1, 4), 3)
	status, _ := g.Status()
	if len(status.Capacity) != 1 {
		t.Fatalf("capacity status: %+v", status.Capacity)
	}
	c := status.Capacity[0]
	if c.Table != "orders" || c.WriteUnits != 4 || c.WriteThrottled != 1 || c.Partitions != 7 || status.TotalThrottled != 1 {
		t.Fatalf("unexpected status: %+v (total %d)", c, status.TotalThrottled)
	}
	if status.DynamoDB == nil || status.DynamoDB.PartitionWriteUnits != defaultPartitionWriteUnits {
		t.Fatalf("status should show the resolved model: %+v", status.DynamoDB)
	}
}

func TestResolveDynamoDB(t *testing.T) {
	def, _ := Resolve(Spec{}, known)
	if def.DynamoDB == nil || def.DynamoDB.burstSeconds() != defaultBurstSeconds {
		t.Fatalf("aws-default should enforce capacity: %+v", def.DynamoDB)
	}
	onlyDynamo, err := Resolve(dynamoOnly(DynamoDBSpec{}), known)
	if err != nil || onlyDynamo.DynamoDB == nil || len(onlyDynamo.Rates) != 0 {
		t.Fatalf("none with a dynamodb section: %+v, %v", onlyDynamo, err)
	}
	for name, spec := range map[string]DynamoDBSpec{
		"negative units":   {PartitionReadUnits: -1},
		"too many parts":   {Tables: map[string]TableSpec{"t": {Partitions: maxPartitions + 1}}},
		"negative parts":   {Tables: map[string]TableSpec{"t": {Partitions: -1}}},
		"negative burst":   {BurstSeconds: seconds(-1)},
		"negative demand":  {OnDemandWriteUnits: -5},
		"negative reading": {OnDemandReadUnits: -5},
	} {
		if _, err := Resolve(dynamoOnly(spec), known); controlCode(t, err) != "InvalidLimitsProfile" {
			t.Errorf("%s: want InvalidLimitsProfile, got %v", name, err)
		}
	}
}

func seconds(v float64) *float64 { return &v }

func TestZeroBurstSecondsBanksNothing(t *testing.T) {
	zero, err := Resolve(dynamoOnly(DynamoDBSpec{BurstSeconds: seconds(0)}), known)
	if err != nil {
		t.Fatal(err)
	}
	if got := zero.DynamoDB.burstSeconds(); got != 0 {
		t.Fatalf("explicit burst_seconds 0 resolved to %v, want 0 (not the 300s default)", got)
	}
	_, capacity := tableCapacity{spec: *zero.DynamoDB, req: CapacityRequest{Write: true, WriteCapacity: 5}}.tableRate()
	if capacity != 5 {
		t.Fatalf("a 5 WCU table with no burst should hold one second of capacity, got %v", capacity)
	}
}
