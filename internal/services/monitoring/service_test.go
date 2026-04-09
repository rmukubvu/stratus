package monitoring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stratus/internal/store/bbolt"
)

func TestNormalizeOperationFallsBackToAction(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader("Action=PutMetricData&Version=2010-08-01"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := normalizeOperation("", req); got != "PutMetricData" {
		t.Fatalf("normalizeOperation() = %q, want PutMetricData", got)
	}
}

func TestNormalizeOperationFallsBackToTarget(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(`{}`))
	req.Header.Set("X-Amz-Target", "AmazonCloudWatch.PutMetricData")

	if got := normalizeOperation("", req); got != "PutMetricData" {
		t.Fatalf("normalizeOperation() = %q, want PutMetricData", got)
	}
}

func TestPutMetricDataAcceptsJSONTargetPayload(t *testing.T) {
	t.Parallel()

	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer metadata.Close()

	svc := NewService(metadata)
	req := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(`{"Namespace":"Stratus/Test","MetricData":[{"MetricName":"Requests","Value":42,"Unit":"Count","Dimensions":[{"Name":"Service","Value":"API"}]}]}`))
	req = req.WithContext(context.Background())
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonCloudWatch.PutMetricData")
	rec := httptest.NewRecorder()

	if err := svc.Handle(rec, req, "", "req-1"); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(`{"Namespace":"Stratus/Test","MetricName":"Requests"}`))
	listReq.Header.Set("Content-Type", "application/x-amz-json-1.1")
	listReq.Header.Set("X-Amz-Target", "AmazonCloudWatch.ListMetrics")
	listRec := httptest.NewRecorder()
	if err := svc.Handle(listRec, listReq, "", "req-2"); err != nil {
		t.Fatalf("list Handle() error = %v", err)
	}
	if !strings.Contains(listRec.Body.String(), "Stratus/Test") || !strings.Contains(listRec.Body.String(), "Requests") {
		t.Fatalf("unexpected list response: %s", listRec.Body.String())
	}
}
