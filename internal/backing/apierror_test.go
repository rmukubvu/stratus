package backing

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAPIErrorMapsRunnerFailures(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		status   int
		code     string
		mentions string
	}{
		{"unavailable", ErrUnavailable, http.StatusServiceUnavailable, "ServiceUnavailable", "Docker is not reachable"},
		{"image missing", &ImageMissingError{Image: "postgres:16-alpine"}, http.StatusServiceUnavailable, "ServiceUnavailable", "docker pull postgres:16-alpine"},
		{"not ready", &NotReadyError{Name: "app-db", Timeout: time.Minute, Logs: "FATAL: bad password"}, http.StatusInternalServerError, "InternalFailure", "FATAL: bad password"},
		{"other", errors.New("boom"), http.StatusInternalServerError, "InternalFailure", "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := APIError(tc.err, "DB instance app-db")
			if got.StatusCode != tc.status || got.Code != tc.code {
				t.Fatalf("got %d %s, want %d %s", got.StatusCode, got.Code, tc.status, tc.code)
			}
			if !strings.Contains(got.Message, tc.mentions) || !strings.Contains(got.Message, "DB instance app-db") {
				t.Fatalf("message should name the resource and the cause: %q", got.Message)
			}
		})
	}
}
