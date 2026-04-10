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

func TestGetMetricStatisticsAcceptsMislabeledQueryBody(t *testing.T) {
	t.Parallel()

	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer metadata.Close()

	svc := NewService(metadata)

	putReq := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(
		"Action=PutMetricData&Version=2010-08-01&Namespace=Stratus%2FTest&MetricData.member.1.MetricName=Requests&MetricData.member.1.Value=42&MetricData.member.1.Unit=Count&MetricData.member.1.Dimensions.member.1.Name=Service&MetricData.member.1.Dimensions.member.1.Value=API",
	))
	putReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	putRec := httptest.NewRecorder()
	if err := svc.Handle(putRec, putReq, "", "req-put"); err != nil {
		t.Fatalf("put Handle() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://localhost/", strings.NewReader(
		"Action=GetMetricStatistics&Version=2010-08-01&Namespace=Stratus%2FTest&MetricName=Requests&StartTime=2026-04-10T06%3A21%3A27Z&EndTime=2026-04-10T06%3A31%3A27Z&Period=60&Statistics.member.1=Average&Dimensions.member.1.Name=Service&Dimensions.member.1.Value=API",
	))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	rec := httptest.NewRecorder()

	if err := svc.Handle(rec, req, "", "req-stats"); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Average") {
		t.Fatalf("unexpected stats response: %s", rec.Body.String())
	}
}
