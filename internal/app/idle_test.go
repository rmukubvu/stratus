package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stratus/internal/config"
)

func TestServeStopsByItselfOnceIdleAndProbesDoNotCount(t *testing.T) {
	cfg := config.Config{Port: 1, DataDir: t.TempDir(), LogLevel: "error", LogFormat: "json", IdleTimeout: 300 * time.Millisecond}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	application, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- application.Serve(context.Background(), ln) }()

	health := "http://" + ln.Addr().String() + "/_stratus/control/v1/health"
	stopProbing := time.After(2 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve returned %v, want a clean stop", err)
			}
			return
		case <-stopProbing:
			t.Fatal("Serve kept running while only health probes arrived")
		case <-time.After(50 * time.Millisecond):
			if resp, err := http.Get(health); err == nil {
				resp.Body.Close()
			}
		}
	}
}
