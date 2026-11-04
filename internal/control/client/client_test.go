package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewRejectsInvalidEndpoint(t *testing.T) {
	for _, bad := range []string{"localhost:4566", "://x", "/path"} {
		if _, err := New(bad, nil); err == nil {
			t.Errorf("New(%q) should fail", bad)
		}
	}
	if _, err := New("", nil); err != nil {
		t.Fatalf("empty endpoint should use default: %v", err)
	}
}

func TestDoDecodesStructuredErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"SnapshotNotFound","message":"nope","hint":"list them"}}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, nil)
	_, err := c.RestoreSnapshot(context.Background(), "baseline")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *Error, got %v", err)
	}
	if apiErr.Status != 404 || apiErr.Code != "SnapshotNotFound" || apiErr.Hint != "list them" {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
}

func TestDoWrapsUnstructuredErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway exploded", http.StatusBadGateway)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, nil)
	_, err := c.Health(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequestsHitExpectedRoutes(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL+"/", nil)
	ctx := context.Background()
	_, _ = c.Reset(ctx, "s3")
	_, _ = c.CreateSnapshot(ctx, "base")
	_, _ = c.ListSnapshots(ctx)
	_, _ = c.RestoreSnapshot(ctx, "base")
	_ = c.DeleteSnapshot(ctx, "base")

	want := []string{
		"POST /_stratus/control/v1/reset",
		"POST /_stratus/control/v1/snapshots",
		"GET /_stratus/control/v1/snapshots",
		"POST /_stratus/control/v1/snapshots/base/restore",
		"DELETE /_stratus/control/v1/snapshots/base",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWaitReadyRetriesUntilReady(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ready":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx, 5*time.Millisecond); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestWaitReadyTimesOutWhenServerIsDown(t *testing.T) {
	c, _ := New("http://127.0.0.1:1", &http.Client{Timeout: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.WaitReady(ctx, 10*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}
