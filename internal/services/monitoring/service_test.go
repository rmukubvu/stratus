package monitoring

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
