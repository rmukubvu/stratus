// Package client is a small Go client for the Stratus control API. It is
// what the stratus CLI uses, and doubles as a reference for SDKs in other
// languages.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultEndpoint is where a local Stratus listens by default.
	DefaultEndpoint = "http://127.0.0.1:4566"
	apiPrefix       = "/_stratus/control/v1"
)

// Error is a structured error returned by the control API.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (hint: %s)", e.Code, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Client talks to one Stratus instance.
type Client struct {
	base *url.URL
	http *http.Client
}

// New returns a client for endpoint, e.g. "http://127.0.0.1:4566".
func New(endpoint string, httpClient *http.Client) (*Client, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	base, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid endpoint %q: want a URL such as %s", endpoint, DefaultEndpoint)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{base: base, http: httpClient}, nil
}

// Do sends a request to a control API path and returns the raw JSON body.
// A non-2xx response is returned as *Error.
func (c *Client) Do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+apiPrefix+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	var envelope struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil {
		envelope.Error.Status = resp.StatusCode
		return raw, envelope.Error
	}
	return raw, &Error{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(raw))}
}

// Health returns the full health report.
func (c *Client) Health(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/health", nil)
}

// Reset resets the named services, or everything when none are given.
func (c *Client) Reset(ctx context.Context, services ...string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/reset", map[string][]string{"services": services})
}

// CreateSnapshot captures current state; an empty name lets the server pick.
func (c *Client) CreateSnapshot(ctx context.Context, name string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/snapshots", map[string]string{"name": name})
}

// ListSnapshots lists snapshots, oldest first.
func (c *Client) ListSnapshots(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/snapshots", nil)
}

// RestoreSnapshot replaces current state with a snapshot.
func (c *Client) RestoreSnapshot(ctx context.Context, name string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/snapshots/"+url.PathEscape(name)+"/restore", nil)
}

// DeleteSnapshot removes a snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, name string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/snapshots/"+url.PathEscape(name), nil)
	return err
}

// WaitReady polls until the emulator reports ready or ctx ends. Connection
// errors are expected while the server starts and are retried.
func (c *Client) WaitReady(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastErr error
	for {
		_, err := c.Do(ctx, http.MethodGet, "/ready", nil)
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("stratus at %s not ready: %w (last error: %v)", c.base, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}
