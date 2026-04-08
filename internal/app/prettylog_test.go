package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestPrettyHandlerFormatsRequestEvents(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := NewPrettyHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	logger.Info("request complete",
		"request_id", "1234567890",
		"service", "sts",
		"operation", "GetCallerIdentity",
		"method", "POST",
		"path", "/",
		"status", 200,
		"duration_ms", 4,
	)

	out := buf.String()
	if !strings.Contains(out, "sts.GetCallerIdentity") {
		t.Fatalf("expected service.operation in output: %s", out)
	}
	if !strings.Contains(out, "200") || !strings.Contains(out, "POST /") {
		t.Fatalf("expected request details in output: %s", out)
	}
}

func TestResolveLogFormatAutoFallsBackToJSONOffTTY(t *testing.T) {
	if got := resolveLogFormat("auto", nil); got != "json" {
		t.Fatalf("resolveLogFormat(auto,nil) = %q, want json", got)
	}
}

func TestPrettyHandlerEnabled(t *testing.T) {
	handler := NewPrettyHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("expected info to be disabled")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("expected error to be enabled")
	}
}
