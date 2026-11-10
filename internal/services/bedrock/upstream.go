package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
)

// Upstream answers requests no fixture covers, in record mode.
type Upstream interface {
	// Call sends a non-streaming request (InvokeModel or Converse) and
	// returns the response body. Streaming calls are recorded through
	// their non-streaming sibling and replayed as a stream.
	Call(ctx context.Context, kind Kind, model string, body []byte) ([]byte, error)
	// Describe names the upstream for status output, without secrets.
	Describe() string
}

// AWSUpstream calls real Bedrock Runtime with SigV4-signed requests.
type AWSUpstream struct {
	Endpoint    string
	Region      string
	Credentials awscompat.Credentials
	Client      *http.Client
	Now         func() time.Time
}

// UpstreamFromEnv configures real Bedrock from the environment:
//
//	STRATUS_BEDROCK_REGION                region; else AWS_REGION, AWS_DEFAULT_REGION, us-east-1
//	STRATUS_BEDROCK_ENDPOINT              endpoint; else https://bedrock-runtime.<region>.amazonaws.com
//	STRATUS_BEDROCK_ACCESS_KEY_ID         credentials; else AWS_ACCESS_KEY_ID,
//	STRATUS_BEDROCK_SECRET_ACCESS_KEY     AWS_SECRET_ACCESS_KEY and
//	STRATUS_BEDROCK_SESSION_TOKEN         AWS_SESSION_TOKEN
//
// The STRATUS_ names exist because the process that runs Stratus often
// carries fake AWS_ keys meant for the emulator.
func UpstreamFromEnv(getenv func(string) string) *AWSUpstream {
	first := func(names ...string) string {
		for _, n := range names {
			if v := strings.TrimSpace(getenv(n)); v != "" {
				return v
			}
		}
		return ""
	}
	region := first("STRATUS_BEDROCK_REGION", "AWS_REGION", "AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := first("STRATUS_BEDROCK_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://bedrock-runtime." + region + ".amazonaws.com"
	}
	creds := awscompat.Credentials{
		AccessKeyID:     first("STRATUS_BEDROCK_ACCESS_KEY_ID"),
		SecretAccessKey: first("STRATUS_BEDROCK_SECRET_ACCESS_KEY"),
		SessionToken:    first("STRATUS_BEDROCK_SESSION_TOKEN"),
	}
	if !creds.Valid() {
		creds = awscompat.Credentials{
			AccessKeyID:     first("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: first("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    first("AWS_SESSION_TOKEN"),
		}
	}
	return &AWSUpstream{Endpoint: strings.TrimRight(endpoint, "/"), Region: region, Credentials: creds}
}

// Describe names the endpoint and region.
func (u *AWSUpstream) Describe() string {
	return u.Endpoint + " (" + u.Region + ")"
}

// Call sends one signed request.
func (u *AWSUpstream) Call(ctx context.Context, kind Kind, model string, body []byte) ([]byte, error) {
	if !u.Credentials.Valid() {
		return nil, &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "UnrecognizedClientException",
			Message: "record mode needs AWS credentials for real Bedrock: set STRATUS_BEDROCK_ACCESS_KEY_ID and " +
				"STRATUS_BEDROCK_SECRET_ACCESS_KEY (or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY) where Stratus runs",
		}
	}
	operation := "invoke"
	if kind == KindConverse {
		operation = "converse"
	}
	target, err := url.Parse(u.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("bedrock endpoint %q: %w", u.Endpoint, err)
	}
	target.Path = "/model/" + model + "/" + operation
	target.RawPath = "/model/" + awscompat.URIEncode(model) + "/" + operation

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	now := time.Now
	if u.Now != nil {
		now = u.Now
	}
	awscompat.SignV4(req, body, u.Credentials, u.Region, "bedrock", now())

	client := u.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &apierror.Error{StatusCode: http.StatusBadGateway, Code: "ServiceUnavailableException", Message: "record mode could not reach real Bedrock: " + err.Error()}
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, upstreamError(resp, respBody)
	}
	return respBody, nil
}

// upstreamError passes real Bedrock's error through, marked as coming from
// upstream so it is not mistaken for an emulator bug.
func upstreamError(resp *http.Response, body []byte) error {
	code := resp.Header.Get("X-Amzn-ErrorType")
	code, _, _ = strings.Cut(code, ":")
	var payload struct {
		Message  string `json:"message"`
		MessageU string `json:"Message"`
		Type     string `json:"__type"`
	}
	_ = json.Unmarshal(body, &payload)
	if code == "" {
		code = payload.Type
		if i := strings.LastIndex(code, "#"); i >= 0 {
			code = code[i+1:]
		}
	}
	if code == "" {
		code = "InternalServerException"
	}
	message := payload.Message
	if message == "" {
		message = payload.MessageU
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	return &apierror.Error{StatusCode: resp.StatusCode, Code: code, Message: "real Bedrock (record mode): " + message}
}

// Config is what the environment sets for the service.
type Config struct {
	Mode       Mode
	FixtureDir string
}

// ConfigFromEnv reads STRATUS_BEDROCK_MODE (replay or record, default
// replay) and STRATUS_BEDROCK_FIXTURES (a directory of fixture files,
// resolved against the working directory).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	mode, err := ParseMode(getenv("STRATUS_BEDROCK_MODE"))
	if err != nil {
		return Config{}, fmt.Errorf("STRATUS_BEDROCK_MODE: %w", err)
	}
	cfg := Config{Mode: mode}
	if dir := strings.TrimSpace(getenv("STRATUS_BEDROCK_FIXTURES")); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return Config{}, fmt.Errorf("STRATUS_BEDROCK_FIXTURES: %w", err)
		}
		cfg.FixtureDir = abs
	}
	return cfg, nil
}
