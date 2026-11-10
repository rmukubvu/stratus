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

// StartChaos starts a chaos experiment. spec is the experiment JSON.
func (c *Client) StartChaos(ctx context.Context, spec json.RawMessage) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPut, "/chaos", spec)
}

// ChaosReport describes the running chaos experiment.
func (c *Client) ChaosReport(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/chaos", nil)
}

// StopChaos stops the chaos experiment and returns its final report.
func (c *Client) StopChaos(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodDelete, "/chaos", nil)
}

// Diagnose explains recent failures. query may set since, service,
// exclude_injected and max_groups.
func (c *Client) Diagnose(ctx context.Context, query url.Values) (json.RawMessage, error) {
	path := "/diagnostics"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.Do(ctx, http.MethodGet, path, nil)
}

// SESMessages lists emails captured by the SES inbox. query may set to,
// from, subject, since and limit.
func (c *Client) SESMessages(ctx context.Context, query url.Values) (json.RawMessage, error) {
	path := "/ses/messages"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.Do(ctx, http.MethodGet, path, nil)
}

// SchedulerAdvance moves the EventBridge Scheduler clock forward by a
// duration (by) or to an RFC 3339 time (to) and fires what became due.
func (c *Client) SchedulerAdvance(ctx context.Context, by, to string) (json.RawMessage, error) {
	body := map[string]string{}
	if by != "" {
		body["by"] = by
	}
	if to != "" {
		body["to"] = to
	}
	return c.Do(ctx, http.MethodPost, "/scheduler/advance", body)
}

// SchedulerRun fires one schedule immediately.
func (c *Client) SchedulerRun(ctx context.Context, group, name string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/scheduler/run", map[string]string{"name": name, "group": group})
}

// BedrockAddFixture adds a runtime Bedrock fixture (the JSON body of POST
// /bedrock/fixtures) and returns it with its id.
func (c *Client) BedrockAddFixture(ctx context.Context, fixture json.RawMessage) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/bedrock/fixtures", fixture)
}

// BedrockInvocations lists logged Bedrock model calls. query may set model,
// contains, outcome and limit.
func (c *Client) BedrockInvocations(ctx context.Context, query url.Values) (json.RawMessage, error) {
	path := "/bedrock/invocations"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.Do(ctx, http.MethodGet, path, nil)
}

// KafkaClusters lists MSK clusters with the bootstrap address of each.
func (c *Client) KafkaClusters(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/kafka/clusters", nil)
}

// DNSResolve looks a name up in the emulated Route 53 zones, including the
// records Cloud Map publishes. rtype defaults to A.
func (c *Client) DNSResolve(ctx context.Context, name, rtype string) (json.RawMessage, error) {
	q := url.Values{"name": {name}}
	if rtype != "" {
		q.Set("type", rtype)
	}
	return c.Do(ctx, http.MethodGet, "/dns/resolve?"+q.Encode(), nil)
}

// Endpoint returns the base URL the client talks to.
func (c *Client) Endpoint() string { return c.base.String() }

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
