package dynamodb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

type harness struct {
	t   *testing.T
	svc *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	return &harness{t: t, svc: NewService(metadata)}
}

// call runs an operation and returns the decoded body, or the API error code.
func (h *harness) call(op string, body string) (map[string]any, string) {
	h.t.Helper()
	rec := httptest.NewRecorder()
	err := h.svc.Handle(rec, httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewBufferString(body)), op)
	if err != nil {
		responder, ok := err.(interface{ WriteErrorResponse(http.ResponseWriter) })
		if !ok {
			h.t.Fatalf("%s: unexpected error %v", op, err)
		}
		var apiErr *apierror.Error
		if !errors.As(err, &apiErr) {
			h.t.Fatalf("%s: error %v does not unwrap to an API error", op, err)
		}
		rec = httptest.NewRecorder()
		responder.WriteErrorResponse(rec)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("%s: decode %q: %v", op, rec.Body.String(), err)
	}
	if typ, ok := out["__type"].(string); ok {
		return out, strings.TrimPrefix(typ, errorTypePrefix)
	}
	return out, ""
}

func (h *harness) must(op, body string) map[string]any {
	h.t.Helper()
	out, code := h.call(op, body)
	if code != "" {
		h.t.Fatalf("%s failed: %s %v", op, code, out["message"])
	}
	return out
}

func (h *harness) fails(op, body, wantCode, wantMessage string) {
	h.t.Helper()
	out, code := h.call(op, body)
	if code != wantCode {
		h.t.Fatalf("%s: code = %q (%v), want %q", op, code, out, wantCode)
	}
	msg, _ := out["message"].(string)
	if msg == "" {
		msg, _ = out["Message"].(string)
	}
	if !strings.Contains(msg, wantMessage) {
		h.t.Fatalf("%s: message %q does not contain %q", op, msg, wantMessage)
	}
}

// ordersTable has a composite key, a GSI on status/amount and an LSI on amount.
func (h *harness) ordersTable() {
	h.must("CreateTable", `{
		"TableName":"orders","BillingMode":"PAY_PER_REQUEST",
		"AttributeDefinitions":[
			{"AttributeName":"customer","AttributeType":"S"},
			{"AttributeName":"order","AttributeType":"N"},
			{"AttributeName":"status","AttributeType":"S"},
			{"AttributeName":"amount","AttributeType":"N"}],
		"KeySchema":[{"AttributeName":"customer","KeyType":"HASH"},{"AttributeName":"order","KeyType":"RANGE"}],
		"GlobalSecondaryIndexes":[{"IndexName":"by-status",
			"KeySchema":[{"AttributeName":"status","KeyType":"HASH"},{"AttributeName":"amount","KeyType":"RANGE"}],
			"Projection":{"ProjectionType":"KEYS_ONLY"}}],
		"LocalSecondaryIndexes":[{"IndexName":"by-amount",
			"KeySchema":[{"AttributeName":"customer","KeyType":"HASH"},{"AttributeName":"amount","KeyType":"RANGE"}],
			"Projection":{"ProjectionType":"ALL"}}]
	}`)
	for i, o := range []struct {
		customer, status string
		order, amount    int
	}{
		{"ann", "open", 1, 30}, {"ann", "shipped", 2, 10}, {"ann", "open", 10, 20},
		{"bob", "open", 1, 5}, {"bob", "shipped", 3, 50},
	} {
		h.must("PutItem", fmt.Sprintf(`{"TableName":"orders","Item":{
			"customer":{"S":%q},"order":{"N":"%d"},"status":{"S":%q},"amount":{"N":"%d"},"seq":{"N":"%d"}}}`,
			o.customer, o.order, o.status, o.amount, i))
	}
}

func numbers(t *testing.T, out map[string]any, attr string) []string {
	t.Helper()
	items, _ := out["Items"].([]any)
	var got []string
	for _, it := range items {
		v := it.(map[string]any)[attr].(map[string]any)["N"].(string)
		got = append(got, v)
	}
	return got
}

func TestDescribeTimeToLiveReturnsDisabledForExistingTable(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	out := h.must("DescribeTimeToLive", `{"TableName":"orders"}`)
	if got := out["TimeToLiveDescription"].(map[string]any)["TimeToLiveStatus"]; got != "DISABLED" {
		t.Fatalf("status = %v", got)
	}
}

func TestCreateTableValidatesKeySchemaAndBilling(t *testing.T) {
	h := newHarness(t)
	h.fails("CreateTable", `{"TableName":"tbl","AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]}`, "ValidationException", "ReadCapacityUnits")
	h.fails("CreateTable", `{"TableName":"tbl","BillingMode":"PAY_PER_REQUEST","AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema":[{"AttributeName":"other","KeyType":"HASH"}]}`, "ValidationException", "")
	h.must("CreateTable", `{"TableName":"tbl","BillingMode":"PAY_PER_REQUEST","AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]}`)
	h.fails("CreateTable", `{"TableName":"tbl","BillingMode":"PAY_PER_REQUEST","AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]}`, "ResourceInUseException", "")
	h.fails("DescribeTable", `{"TableName":"missing"}`, "ResourceNotFoundException", "")
}

func TestDescribeTableReportsKeysIndexesAndCounts(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	table := h.must("DescribeTable", `{"TableName":"orders"}`)["Table"].(map[string]any)
	if table["ItemCount"].(float64) != 5 {
		t.Fatalf("ItemCount = %v", table["ItemCount"])
	}
	if ks := table["KeySchema"].([]any); len(ks) != 2 || ks[1].(map[string]any)["KeyType"] != "RANGE" {
		t.Fatalf("KeySchema = %v", ks)
	}
	if len(table["GlobalSecondaryIndexes"].([]any)) != 1 || len(table["LocalSecondaryIndexes"].([]any)) != 1 {
		t.Fatalf("indexes missing: %v", table)
	}
}

func TestItemsWithTheSameHashKeyAreDistinctBySortKey(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	out := h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"10"}}}`)
	if got := out["Item"].(map[string]any)["amount"].(map[string]any)["N"]; got != "20" {
		t.Fatalf("amount = %v", got)
	}
	// 1e1 is the same number as 10, so it names the same item.
	out = h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1e1"}}}`)
	if out["Item"] == nil {
		t.Fatal("number keys must compare by value")
	}
	h.fails("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"}}}`, "ValidationException", "does not match the schema")
	h.fails("PutItem", `{"TableName":"orders","Item":{"customer":{"S":"ann"},"order":{"S":"x"}}}`, "ValidationException", "Type mismatch for key order")
}

func TestQueryOrdersBySortKeyAndPages(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	out := h.must("Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c",
		"ExpressionAttributeValues":{":c":{"S":"ann"}}}`)
	if got := strings.Join(numbers(t, out, "order"), ","); got != "1,2,10" {
		t.Fatalf("ascending order = %s (numbers must sort numerically)", got)
	}
	out = h.must("Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c AND #o BETWEEN :lo AND :hi",
		"ExpressionAttributeNames":{"#o":"order"},"ExpressionAttributeValues":{":c":{"S":"ann"},":lo":{"N":"2"},":hi":{"N":"10"}},
		"ScanIndexForward":false}`)
	if got := strings.Join(numbers(t, out, "order"), ","); got != "10,2" {
		t.Fatalf("descending between = %s", got)
	}

	var pages []string
	start := ""
	for i := 0; i < 5; i++ {
		body := `{"TableName":"orders","KeyConditionExpression":"customer = :c","Limit":1,
			"ExpressionAttributeValues":{":c":{"S":"ann"}}` + start + `}`
		out = h.must("Query", body)
		pages = append(pages, numbers(t, out, "order")...)
		lek, ok := out["LastEvaluatedKey"]
		if !ok {
			break
		}
		raw, _ := json.Marshal(lek)
		start = `,"ExclusiveStartKey":` + string(raw)
	}
	if got := strings.Join(pages, ","); got != "1,2,10" {
		t.Fatalf("paged query = %s", got)
	}
}

func TestQueryRejectsInvalidKeyConditions(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	h.fails("Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c AND status = :s",
		"ExpressionAttributeValues":{":c":{"S":"ann"},":s":{"S":"open"}}}`, "ValidationException", "")
	h.fails("Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c",
		"ExpressionAttributeValues":{":c":{"S":"ann"},":unused":{"S":"x"}}}`, "ValidationException", "unused")
	h.fails("Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c",
		"ExpressionAttributeValues":{":c":{"S":"ann"}},"IndexName":"nope"}`, "ValidationException", "")
}

func TestQueryFilterCountsScannedItems(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	out := h.must("Query", `{"TableName":"orders","KeyConditionExpression":"customer = :c",
		"FilterExpression":"#s = :open AND amount > :min",
		"ExpressionAttributeNames":{"#s":"status"},
		"ExpressionAttributeValues":{":c":{"S":"ann"},":open":{"S":"open"},":min":{"N":"25"}}}`)
	if out["Count"].(float64) != 1 || out["ScannedCount"].(float64) != 3 {
		t.Fatalf("Count/ScannedCount = %v/%v", out["Count"], out["ScannedCount"])
	}
}

func TestQueryUsesSecondaryIndexesAndTheirProjections(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	out := h.must("Query", `{"TableName":"orders","IndexName":"by-status","KeyConditionExpression":"#s = :s",
		"ExpressionAttributeNames":{"#s":"status"},"ExpressionAttributeValues":{":s":{"S":"open"}}}`)
	if got := strings.Join(numbers(t, out, "amount"), ","); got != "5,20,30" {
		t.Fatalf("GSI order = %s", got)
	}
	first := out["Items"].([]any)[0].(map[string]any)
	if _, ok := first["seq"]; ok {
		t.Fatalf("KEYS_ONLY index returned a non-key attribute: %v", first)
	}
	if _, ok := first["order"]; !ok {
		t.Fatalf("index items must carry the table key: %v", first)
	}
	out = h.must("Query", `{"TableName":"orders","IndexName":"by-amount","KeyConditionExpression":"customer = :c AND amount >= :t",
		"ExpressionAttributeValues":{":c":{"S":"ann"},":t":{"N":"20"}}}`)
	if got := strings.Join(numbers(t, out, "order"), ","); got != "10,1" {
		t.Fatalf("LSI order = %s", got)
	}
}

func TestScanSegmentsPartitionTheTable(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	amount := 0
	for seg := 0; seg < 3; seg++ {
		out := h.must("Scan", fmt.Sprintf(`{"TableName":"orders","Segment":%d,"TotalSegments":3}`, seg))
		amount += int(out["Count"].(float64))
	}
	if amount != 5 {
		t.Fatalf("segments returned %d items, want 5", amount)
	}
	out := h.must("Scan", `{"TableName":"orders","Select":"COUNT","FilterExpression":"begins_with(#s, :p)",
		"ExpressionAttributeNames":{"#s":"status"},"ExpressionAttributeValues":{":p":{"S":"ship"}}}`)
	if out["Count"].(float64) != 2 || out["Items"] != nil {
		t.Fatalf("COUNT scan = %v", out)
	}
}

func TestConditionalPutAndDelete(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	h.fails("PutItem", `{"TableName":"orders","Item":{"customer":{"S":"ann"},"order":{"N":"1"}},
		"ConditionExpression":"attribute_not_exists(customer)"}`, "ConditionalCheckFailedException", "The conditional request failed")
	h.must("PutItem", `{"TableName":"orders","Item":{"customer":{"S":"cat"},"order":{"N":"1"}},
		"ConditionExpression":"attribute_not_exists(customer)"}`)
	h.fails("DeleteItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}},
		"ConditionExpression":"amount < :t","ExpressionAttributeValues":{":t":{"N":"30"}}}`, "ConditionalCheckFailedException", "")
	out := h.must("DeleteItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}},
		"ConditionExpression":"amount <= :t","ExpressionAttributeValues":{":t":{"N":"30"}},"ReturnValues":"ALL_OLD"}`)
	if out["Attributes"].(map[string]any)["amount"].(map[string]any)["N"] != "30" {
		t.Fatalf("ALL_OLD = %v", out)
	}
}

func TestUpdateItemAppliesEveryClause(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	// info does not exist, so the nested SET must fail without changing anything.
	h.fails("UpdateItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"2"}},
		"UpdateExpression":"SET info.city = :city","ExpressionAttributeValues":{":city":{"S":"Leeds"}}}`,
		"ValidationException", "document path")

	out := h.must("UpdateItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"2"}},
		"UpdateExpression":"SET amount = amount + :inc, notes = list_append(if_not_exists(notes, :empty), :note) REMOVE seq ADD tags :tags",
		"ExpressionAttributeValues":{":inc":{"N":"0.5"},":empty":{"L":[]},":note":{"L":[{"S":"gift"}]},":tags":{"SS":["a","b"]}},
		"ReturnValues":"ALL_NEW"}`)
	attrs := out["Attributes"].(map[string]any)
	if attrs["amount"].(map[string]any)["N"] != "10.5" {
		t.Fatalf("amount = %v", attrs["amount"])
	}
	if _, ok := attrs["seq"]; ok {
		t.Fatal("REMOVE seq did not remove it")
	}
	if len(attrs["notes"].(map[string]any)["L"].([]any)) != 1 || len(attrs["tags"].(map[string]any)["SS"].([]any)) != 2 {
		t.Fatalf("list_append/ADD = %v", attrs)
	}

	out = h.must("UpdateItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"2"}},
		"UpdateExpression":"DELETE tags :gone SET #s = :s","ExpressionAttributeNames":{"#s":"status"},
		"ExpressionAttributeValues":{":gone":{"SS":["a"]},":s":{"S":"closed"}},"ReturnValues":"UPDATED_OLD"}`)
	old := out["Attributes"].(map[string]any)
	if old["status"].(map[string]any)["S"] != "shipped" || len(old) != 2 {
		t.Fatalf("UPDATED_OLD = %v", old)
	}

	h.fails("UpdateItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"2"}},
		"UpdateExpression":"SET #o = :o","ExpressionAttributeNames":{"#o":"order"},"ExpressionAttributeValues":{":o":{"N":"9"}}}`,
		"ValidationException", "key")
	h.fails("UpdateItem", `{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"2"}},
		"UpdateExpression":"SET amount = :a, amount = :b","ExpressionAttributeValues":{":a":{"N":"1"},":b":{"N":"2"}}}`,
		"ValidationException", "overlap")
}

func TestUpdateItemCreatesMissingItems(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	out := h.must("UpdateItem", `{"TableName":"orders","Key":{"customer":{"S":"dan"},"order":{"N":"1"}},
		"UpdateExpression":"ADD visits :one","ExpressionAttributeValues":{":one":{"N":"1"}},"ReturnValues":"ALL_NEW"}`)
	attrs := out["Attributes"].(map[string]any)
	if attrs["visits"].(map[string]any)["N"] != "1" || attrs["customer"].(map[string]any)["S"] != "dan" {
		t.Fatalf("upsert = %v", attrs)
	}
}

func TestProjectionExpressionReturnsNestedPaths(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	h.must("PutItem", `{"TableName":"orders","Item":{"customer":{"S":"eve"},"order":{"N":"1"},
		"info":{"M":{"city":{"S":"York"},"zip":{"S":"Y1"}}},"parts":{"L":[{"S":"a"},{"S":"b"}]}}}`)
	out := h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"eve"},"order":{"N":"1"}},
		"ProjectionExpression":"info.city, parts[1]"}`)
	it := out["Item"].(map[string]any)
	if len(it) != 2 {
		t.Fatalf("projection = %v", it)
	}
	info := it["info"].(map[string]any)["M"].(map[string]any)
	if _, ok := info["zip"]; ok || info["city"] == nil {
		t.Fatalf("nested projection = %v", info)
	}
	if parts := it["parts"].(map[string]any)["L"].([]any); len(parts) != 1 || parts[0].(map[string]any)["S"] != "b" {
		t.Fatalf("list projection = %v", parts)
	}
}

func TestBatchWriteAndGet(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	h.must("BatchWriteItem", `{"RequestItems":{"orders":[
		{"PutRequest":{"Item":{"customer":{"S":"fay"},"order":{"N":"1"}}}},
		{"DeleteRequest":{"Key":{"customer":{"S":"ann"},"order":{"N":"1"}}}}]}}`)
	out := h.must("BatchGetItem", `{"RequestItems":{"orders":{"Keys":[
		{"customer":{"S":"fay"},"order":{"N":"1"}},{"customer":{"S":"ann"},"order":{"N":"1"}}]}}}`)
	if got := len(out["Responses"].(map[string]any)["orders"].([]any)); got != 1 {
		t.Fatalf("BatchGetItem returned %d items, want 1", got)
	}
	h.fails("BatchWriteItem", `{"RequestItems":{"orders":[
		{"PutRequest":{"Item":{"customer":{"S":"x"},"order":{"N":"1"}}}},
		{"DeleteRequest":{"Key":{"customer":{"S":"x"},"order":{"N":"1"}}}}]}}`, "ValidationException", "duplicates")
}

func TestTransactWriteIsAllOrNothing(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	out, code := h.call("TransactWriteItems", `{"TransactItems":[
		{"Put":{"TableName":"orders","Item":{"customer":{"S":"gus"},"order":{"N":"1"}}}},
		{"ConditionCheck":{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}},
			"ConditionExpression":"amount > :t","ExpressionAttributeValues":{":t":{"N":"100"}},
			"ReturnValuesOnConditionCheckFailure":"ALL_OLD"}}]}`)
	if code != "TransactionCanceledException" {
		t.Fatalf("code = %q", code)
	}
	reasons := out["CancellationReasons"].([]any)
	if reasons[0].(map[string]any)["Code"] != "None" || reasons[1].(map[string]any)["Code"] != "ConditionalCheckFailed" {
		t.Fatalf("reasons = %v", reasons)
	}
	if reasons[1].(map[string]any)["Item"] == nil {
		t.Fatal("ALL_OLD must return the item on the failed check")
	}
	if got := h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"gus"},"order":{"N":"1"}}}`); got["Item"] != nil {
		t.Fatal("a cancelled transaction must not write")
	}

	h.must("TransactWriteItems", `{"TransactItems":[
		{"Put":{"TableName":"orders","Item":{"customer":{"S":"gus"},"order":{"N":"1"}}}},
		{"Update":{"TableName":"orders","Key":{"customer":{"S":"ann"},"order":{"N":"1"}},
			"UpdateExpression":"SET amount = :t","ExpressionAttributeValues":{":t":{"N":"1"}}}},
		{"Delete":{"TableName":"orders","Key":{"customer":{"S":"bob"},"order":{"N":"1"}}}}]}`)
	got := h.must("TransactGetItems", `{"TransactItems":[
		{"Get":{"TableName":"orders","Key":{"customer":{"S":"gus"},"order":{"N":"1"}}}},
		{"Get":{"TableName":"orders","Key":{"customer":{"S":"bob"},"order":{"N":"1"}}}}]}`)
	responses := got["Responses"].([]any)
	if responses[0].(map[string]any)["Item"] == nil || responses[1].(map[string]any)["Item"] != nil {
		t.Fatalf("TransactGetItems = %v", responses)
	}
	h.fails("TransactWriteItems", `{"TransactItems":[
		{"Put":{"TableName":"orders","Item":{"customer":{"S":"gus"},"order":{"N":"1"}}}},
		{"Delete":{"TableName":"orders","Key":{"customer":{"S":"gus"},"order":{"N":"1"}}}}]}`,
		"ValidationException", "multiple operations on one item")
}

func TestExpiredItemsDisappear(t *testing.T) {
	h := newHarness(t)
	h.ordersTable()
	now := time.Unix(1_800_000_000, 0)
	h.svc.now = func() time.Time { return now }
	h.must("UpdateTimeToLive", `{"TableName":"orders","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"expires"}}`)
	h.must("PutItem", fmt.Sprintf(`{"TableName":"orders","Item":{"customer":{"S":"hal"},"order":{"N":"1"},"expires":{"N":"%d"}}}`, now.Unix()+60))
	if h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"hal"},"order":{"N":"1"}}}`)["Item"] == nil {
		t.Fatal("item expired early")
	}
	now = now.Add(2 * time.Minute)
	if h.must("GetItem", `{"TableName":"orders","Key":{"customer":{"S":"hal"},"order":{"N":"1"}}}`)["Item"] != nil {
		t.Fatal("expired item is still readable")
	}
}

func TestDeletionProtectionBlocksDeleteTable(t *testing.T) {
	h := newHarness(t)
	h.must("CreateTable", `{"TableName":"keep","BillingMode":"PAY_PER_REQUEST","DeletionProtectionEnabled":true,
		"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]}`)
	h.fails("DeleteTable", `{"TableName":"keep"}`, "ValidationException", "deletion protection")
	h.must("UpdateTable", `{"TableName":"keep","DeletionProtectionEnabled":false}`)
	h.must("DeleteTable", `{"TableName":"keep"}`)
}

func TestTablesWrittenByCloudFormationStillWork(t *testing.T) {
	h := newHarness(t)
	// CloudFormation writes the older record shape directly.
	raw := `{"attribute_definitions":[{"AttributeName":"id","AttributeType":"S"}],"billing_mode":"PAY_PER_REQUEST",
		"hash_key":"id","hash_key_type":"S","table_name":"legacy","table_status":"ACTIVE"}`
	if err := h.svc.metadata.Put(tablesBucket, "legacy", []byte(raw)); err != nil {
		t.Fatal(err)
	}
	h.must("PutItem", `{"TableName":"legacy","Item":{"id":{"S":"1"},"v":{"N":"2"}}}`)
	out := h.must("Scan", `{"TableName":"legacy"}`)
	if out["Count"].(float64) != 1 {
		t.Fatalf("scan = %v", out)
	}
}

func TestBatchGetReturnsUnprocessedKeysPastSixteenMegabytes(t *testing.T) {
	h := newHarness(t)
	h.must("CreateTable", `{"TableName":"big","BillingMode":"PAY_PER_REQUEST",
		"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]}`)
	blob := strings.Repeat("x", 390*1024)
	var keys []string
	for i := 0; i < 45; i++ {
		h.must("PutItem", fmt.Sprintf(`{"TableName":"big","Item":{"id":{"S":"k%02d"},"blob":{"S":%q}}}`, i, blob))
		keys = append(keys, fmt.Sprintf(`{"id":{"S":"k%02d"}}`, i))
	}
	out := h.must("BatchGetItem", `{"RequestItems":{"big":{"Keys":[`+strings.Join(keys, ",")+`],"ConsistentRead":true}}}`)
	got := len(out["Responses"].(map[string]any)["big"].([]any))
	pending, ok := out["UnprocessedKeys"].(map[string]any)["big"].(map[string]any)
	if !ok {
		t.Fatalf("no UnprocessedKeys after %d items", got)
	}
	if left := len(pending["Keys"].([]any)); got+left != 45 || pending["ConsistentRead"] != true {
		t.Fatalf("returned %d, unprocessed %d (ConsistentRead %v)", got, left, pending["ConsistentRead"])
	}
}

func TestResponsesCarryCRC32AndErrorsUnwrap(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	if err := h.svc.Handle(rec, httptest.NewRequest("POST", "/", bytes.NewBufferString(`{}`)), "ListTables"); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprint(crc32.ChecksumIEEE(rec.Body.Bytes())); rec.Header().Get("X-Amz-Crc32") != want {
		t.Fatalf("x-amz-crc32 = %q, want %s", rec.Header().Get("X-Amz-Crc32"), want)
	}
	err := h.svc.Handle(httptest.NewRecorder(), httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"TableName":"missing"}`)), "DescribeTable")
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "ResourceNotFoundException" {
		t.Fatalf("error %v must unwrap to ResourceNotFoundException so the request log records it", err)
	}
}
