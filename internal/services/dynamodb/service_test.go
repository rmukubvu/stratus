package dynamodb

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/store/bbolt"
)

func TestDescribeTimeToLiveReturnsDisabledForExistingTable(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	svc := NewService(metadata)

	createReq := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewBufferString(`{
		"TableName":"fixture",
		"BillingMode":"PAY_PER_REQUEST",
		"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]
	}`))
	createRec := httptest.NewRecorder()
	if err := svc.Handle(createRec, createReq, "CreateTable"); err != nil {
		t.Fatalf("CreateTable failed: %v", err)
	}

	ttlReq := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewBufferString(`{"TableName":"fixture"}`))
	ttlRec := httptest.NewRecorder()
	if err := svc.Handle(ttlRec, ttlReq, "DescribeTimeToLive"); err != nil {
		t.Fatalf("DescribeTimeToLive failed: %v", err)
	}
	if body := ttlRec.Body.String(); !strings.Contains(body, `"TimeToLiveStatus":"DISABLED"`) {
		t.Fatalf("unexpected DescribeTimeToLive response: %s", body)
	}
}
