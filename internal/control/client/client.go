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

// SetLimits applies a limits profile. spec is the profile JSON.
func (c *Client) SetLimits(ctx context.Context, spec json.RawMessage) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPut, "/limits", spec)
}

// LimitsStatus describes the active limits profile and its counters.
func (c *Client) LimitsStatus(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/limits", nil)
}

// ClearLimits turns limits off and returns the final status.
func (c *Client) ClearLimits(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodDelete, "/limits", nil)
}

// SetSigV4 sets signature verification: strict, audit or off.
func (c *Client) SetSigV4(ctx context.Context, mode string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPut, "/sigv4", map[string]string{"mode": mode})
}

// SigV4Status reports the signature mode, counters and recent failures.
func (c *Client) SigV4Status(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/sigv4", nil)
}

// ClearSigV4 turns signature verification off and returns the final status.
func (c *Client) ClearSigV4(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodDelete, "/sigv4", nil)
}

// StartRun starts recording a load run. spec may set name, limits and
// chaos.
func (c *Client) StartRun(ctx context.Context, spec json.RawMessage) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/runs", spec)
}

// GetRun returns a run's summary and timeline; id may be "current" or
// "latest".
func (c *Client) GetRun(ctx context.Context, id string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/runs/"+url.PathEscape(id), nil)
}

// StopRun stops a run ("current" for the active one) and returns its
// final summary.
func (c *Client) StopRun(ctx context.Context, id string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/runs/"+url.PathEscape(id)+"/stop", nil)
}

// ListRuns lists recent runs, newest first.
func (c *Client) ListRuns(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/runs", nil)
}

// AdviseFamilies lists the decision families stratus advise knows, with
// their questions and options.
func (c *Client) AdviseFamilies(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/advise/families", nil)
}

// AdviseStart starts an architecture decision. in sets family, and may
// set title and first answers.
func (c *Client) AdviseStart(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/advise/decisions", in)
}

// AdviseList lists decisions, newest first.
func (c *Client) AdviseList(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/advise/decisions", nil)
}

// AdviseGet returns a decision with its pending questions and ranking.
func (c *Client) AdviseGet(ctx context.Context, id string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/advise/decisions/"+url.PathEscape(id), nil)
}

// AdviseAnswer records answers, keyed by question ID.
func (c *Client) AdviseAnswer(ctx context.Context, id string, answers map[string]any) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/advise/decisions/"+url.PathEscape(id)+"/answers", map[string]any{"answers": answers})
}

// AdviseAssess judges and ranks a decision's options.
func (c *Client) AdviseAssess(ctx context.Context, id string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/advise/decisions/"+url.PathEscape(id)+"/assess", nil)
}

// AdviseDecide chooses an option, with an optional rationale.
func (c *Client) AdviseDecide(ctx context.Context, id, option, rationale string) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/advise/decisions/"+url.PathEscape(id)+"/decide", map[string]string{"option": option, "rationale": rationale})
}

// AdviseADR returns a decision's ADR as Markdown.
func (c *Client) AdviseADR(ctx context.Context, id string) (string, error) {
	raw, err := c.Do(ctx, http.MethodGet, "/advise/decisions/"+url.PathEscape(id)+"/adr", nil)
	return string(raw), err
}

// AdvisePlanProofs plans load proofs of a decision's candidate options. in
// may set options, scale and duration_seconds.
func (c *Client) AdvisePlanProofs(ctx context.Context, id string, in json.RawMessage) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/advise/decisions/"+url.PathEscape(id)+"/proofs", in)
}

// AdviseRecordEvidence attaches a finished load run to an option as its
// proof. in sets option, run_id and target_rate, and may set scale and
// account.
func (c *Client) AdviseRecordEvidence(ctx context.Context, id string, in json.RawMessage) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/advise/decisions/"+url.PathEscape(id)+"/evidence", in)
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

// ClockStatus reports the emulator clock: its time, real time, the offset
// between them and whether it is frozen.
func (c *Client) ClockStatus(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, "/clock", nil)
}

// ClockAdvance moves the emulator clock forward by a duration (by) or to an
// RFC 3339 time (to), and runs whatever became due, such as schedules.
func (c *Client) ClockAdvance(ctx context.Context, by, to string) (json.RawMessage, error) {
	body := map[string]string{}
	if by != "" {
		body["by"] = by
	}
	if to != "" {
		body["to"] = to
	}
	return c.Do(ctx, http.MethodPost, "/clock/advance", body)
}

// ClockFreeze holds the emulator clock where it stands.
func (c *Client) ClockFreeze(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/clock/freeze", nil)
}

// ClockResume lets a frozen emulator clock run again from where it was held.
func (c *Client) ClockResume(ctx context.Context) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, "/clock/resume", nil)
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
