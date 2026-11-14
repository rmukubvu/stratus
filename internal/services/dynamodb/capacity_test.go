package dynamodb

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/reqnote"
)

// fakeGate throttles demands on hot partitions, recording every call.
type fakeGate struct {
	hot   map[string]bool
	calls [][]CapacityDemand
}

func (g *fakeGate) Consume(_ context.Context, demands []CapacityDemand) []*CapacityThrottle {
	g.calls = append(g.calls, demands)
	var out []*CapacityThrottle
	for i, d := range demands {
		if !g.hot[d.PartitionKey] {
			continue
		}
		if out == nil {
			out = make([]*CapacityThrottle, len(demands))
		}
		reason := "TableReadKeyRangeThroughputExceeded"
		if d.Write {
			reason = "TableWriteKeyRangeThroughputExceeded"
		}
		out[i] = &CapacityThrottle{Reason: reason}
	}
	return out
}

func (g *fakeGate) last() []CapacityDemand { return g.calls[len(g.calls)-1] }

// throttled sets up the orders table with a gate that throttles the
// partitions named.
func throttled(t *testing.T, hot ...string) (*harness, *fakeGate) {
	h := newHarness(t)
	h.ordersTable()
	gate := &fakeGate{hot: map[string]bool{}}
	for _, pk := range hot {
		gate.hot[canonicalKey(map[string]any{"S": pk})] = true
	}
	h.svc.SetCapacity(gate)
	return h, gate
}

func TestThrottledWriteFailsWithThrottlingReasonsAndWritesNothing(t *testing.T) {
	h, gate := throttled(t, "ann")
	out, code := h.call("PutItem", `{"TableName":"orders","Item":{"customer":{"S":"ann"},"order":{"N":"99"}}}`)
	if code != "ProvisionedThroughputExceededException" {
		t.Fatalf("code = %q, want ProvisionedThroughputExceededException", code)
	}
	reasons, _ := out["ThrottlingReasons"].([]any)
	if len(reasons) != 1 || reasons[0].(map[string]any)["reason"] != "TableWriteKeyRangeThroughputExceeded" ||
		!strings.HasSuffix(reasons[0].(map[string]any)["resource"].(string), ":table/orders") {
		t.Fatalf("ThrottlingReasons = %v", out["ThrottlingReasons"])
	}
	d := gate.last()[0]
	if !d.Write || !d.OnDemand || d.Table != "orders" || d.Units != 1 {
		t.Fatalf("demand = %+v", d)
	}
	h.svc.SetCapacity(nil)
	got := h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"99"}}}`)
	if _, ok := got["Item"]; ok {
		t.Fatal("a throttled write was stored")
	}
}

func TestEveryItemOperationDrawsCapacity(t *testing.T) {
	h, gate := throttled(t, "ann")
	cases := []struct{ op, body string }{
		{"GetItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}}}`},
		{"UpdateItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}},"UpdateExpression":"SET x = :x","ExpressionAttributeValues":{":x":{"N":"1"}}}`},
		{"DeleteItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}}}`},
		{"Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c","ExpressionAttributeValues":{":c":{"S":"ann"}}}`},
	}
	for _, tc := range cases {
		if _, code := h.call(tc.op, tc.body); code != "ProvisionedThroughputExceededException" {
			t.Errorf("%s on a hot partition: code %q", tc.op, code)
		}
	}
	// Bob's partition is fine, and a Scan only meets the table cap.
	h.must("Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c","ExpressionAttributeValues":{":c":{"S":"bob"}}}`)
	h.must("Scan", `{"TableName":"orders"}`)
	if d := gate.last()[0]; d.PartitionKey != "" || d.Write {
		t.Fatalf("scan demand = %+v", d)
	}
}

func TestQueryOnAnIndexDrawsOnTheIndexPartition(t *testing.T) {
	h, gate := throttled(t)
	h.must("Query", `{"TableName":"orders","IndexName":"by-status","KeyConditionExpression":"#s = :s",
		"ExpressionAttributeNames":{"#s":"status"},"ExpressionAttributeValues":{":s":{"S":"open"}}}`)
	if pk := gate.last()[0].PartitionKey; !strings.HasPrefix(pk, "by-status|") {
		t.Fatalf("index query partition = %q", pk)
	}
}

func TestProvisionedTablesReportTheirCapacity(t *testing.T) {
	h := newHarness(t)
	gate := &fakeGate{}
	h.svc.SetCapacity(gate)
	h.must("CreateTable", `{"TableName":"slow","BillingMode":"PROVISIONED","ProvisionedThroughput":{"ReadCapacityUnits":3,"WriteCapacityUnits":7},
		"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]}`)
	h.must("PutItem", `{"TableName":"slow","Item":{"id":{"S":"a"}}}`)
	if d := gate.last()[0]; d.OnDemand || d.ReadCapacity != 3 || d.WriteCapacity != 7 {
		t.Fatalf("demand = %+v", d)
	}
	h.must("UpdateTable", `{"TableName":"slow","ProvisionedThroughput":{"ReadCapacityUnits":3,"WriteCapacityUnits":70}}`)
	h.must("PutItem", `{"TableName":"slow","Item":{"id":{"S":"a"}}}`)
	if d := gate.last()[0]; d.WriteCapacity != 70 {
		t.Fatalf("UpdateTable did not apply at once: %+v", d)
	}
}

func TestBatchWriteReturnsThrottledItemsAsUnprocessed(t *testing.T) {
	h, _ := throttled(t, "ann")
	out := h.must("BatchWriteItem", `{"RequestItems":{"orders":[
		{"PutRequest":{"Item":{"customer":{"S":"ann"},"order":{"N":"50"}}}},
		{"PutRequest":{"Item":{"customer":{"S":"cat"},"order":{"N":"1"}}}}]}}`)
	unprocessed := out["UnprocessedItems"].(map[string]any)["orders"].([]any)
	if len(unprocessed) != 1 {
		t.Fatalf("UnprocessedItems = %v", out["UnprocessedItems"])
	}
	put := unprocessed[0].(map[string]any)["PutRequest"].(map[string]any)["Item"].(map[string]any)
	if put["customer"].(map[string]any)["S"] != "ann" {
		t.Fatalf("wrong item unprocessed: %v", put)
	}
	h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"cat"},"order":{"N":"1"}}}`)

	// When nothing fits, the whole call fails.
	h.fails("BatchWriteItem", `{"RequestItems":{"orders":[
		{"DeleteRequest":{"Key":{"customer":{"S":"ann"},"order":{"N":"1"}}}}]}}`, "ProvisionedThroughputExceededException", "")
}

func TestBatchGetReturnsThrottledKeysAsUnprocessed(t *testing.T) {
	h, _ := throttled(t, "ann")
	out := h.must("BatchGetItem", `{"RequestItems":{"orders":{"Keys":[
		{"customer":{"S":"ann"},"order":{"N":"1"}},{"customer":{"S":"bob"},"order":{"N":"1"}}]}}}`)
	if got := len(out["Responses"].(map[string]any)["orders"].([]any)); got != 1 {
		t.Fatalf("got %d items, want bob's only", got)
	}
	keys := out["UnprocessedKeys"].(map[string]any)["orders"].(map[string]any)["Keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("UnprocessedKeys = %v", out["UnprocessedKeys"])
	}
	h.fails("BatchGetItem", `{"RequestItems":{"orders":{"Keys":[{"customer":{"S":"ann"},"order":{"N":"1"}}]}}}`,
		"ProvisionedThroughputExceededException", "")
}

func TestThrottledTransactionIsCancelledWithThrottlingError(t *testing.T) {
	h, gate := throttled(t, "ann")
	out, code := h.call("TransactWriteItems", `{"TransactItems":[
		{"Put":{"TableName":"orders","Item":{"customer":{"S":"bob"},"order":{"N":"7"}}}},
		{"Put":{"TableName":"orders","Item":{"customer":{"S":"ann"},"order":{"N":"7"}}}}]}`)
	if code != "TransactionCanceledException" {
		t.Fatalf("code = %q", code)
	}
	reasons := out["CancellationReasons"].([]any)
	if reasons[0].(map[string]any)["Code"] != "None" || reasons[1].(map[string]any)["Code"] != "ThrottlingError" {
		t.Fatalf("CancellationReasons = %v", reasons)
	}
	if units := gate.last()[0].Units; units != 2 {
		t.Fatalf("transactional write cost %v units, want 2", units)
	}
	got := h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"bob"},"order":{"N":"7"}}}`)
	if _, ok := got["Item"]; ok {
		t.Fatal("a cancelled transaction wrote an item")
	}

	if _, code := h.call("TransactGetItems", `{"TransactItems":[{"Get":{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}}}}]}`); code != "TransactionCanceledException" {
		t.Fatalf("TransactGetItems code = %q", code)
	}
}

func TestOperationOfReadsTheTargetHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Amz-Target", "DynamoDB_20120810.PutItem")
	if got := operationOf(r); got != "PutItem" {
		t.Fatalf("operationOf = %q", got)
	}
}

func TestRequestsNotePartitionsWithoutAGate(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	ctx, notes := reqnote.With(context.Background())
	req := httptest.NewRequest("POST", "http://localhost:4566/", strings.NewReader(
		`{"RequestItems":{"orders":[{"PutRequest":{"Item":{"customer":{"S":"ann"},"order":{"N":"1"}}}},{"PutRequest":{"Item":{"customer":{"S":"bob"},"order":{"N":"2"}}}}]}}`,
	)).WithContext(ctx)
	if err := h.svc.Handle(httptest.NewRecorder(), req, "BatchWriteItem"); err != nil {
		t.Fatal(err)
	}
	keys := notes.Keys()
	if len(keys) != 2 || keys[0].Resource != "orders" || keys[0].Partition != canonicalKey(map[string]any{"S": "ann"}) {
		t.Fatalf("noted %+v", keys)
	}
}
