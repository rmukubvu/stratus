package bedrock

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/awscompat"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestAWSUpstreamSignsAndEscapesTheModelID(t *testing.T) {
	var gotPath, gotAuth, gotToken, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("X-Amz-Security-Token")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}}}`))
	}))
	defer server.Close()

	up := &AWSUpstream{
		Endpoint:    server.URL,
		Region:      "eu-west-1",
		Credentials: awscompat.Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "tok"},
		Now:         func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) },
	}
	body, err := up.Call(context.Background(), KindConverse, "anthropic.claude-3-haiku-20240307-v1:0", []byte(`{"messages":[]}`))
	if err != nil || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("call: %v %s", err, body)
	}
	if gotPath != "/model/anthropic.claude-3-haiku-20240307-v1%3A0/converse" {
		t.Fatalf("path %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKID/20261109/eu-west-1/bedrock/aws4_request") || gotToken != "tok" || gotBody != `{"messages":[]}` {
		t.Fatalf("signed request: auth=%q token=%q body=%q", gotAuth, gotToken, gotBody)
	}

	if _, err := up.Call(context.Background(), KindInvoke, "meta.llama3", []byte(`{}`)); err != nil || gotPath != "/model/meta.llama3/invoke" {
		t.Fatalf("invoke path %q: %v", gotPath, err)
	}
}

func TestAWSUpstreamPassesErrorsThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException:http://internal.amazon.com/coral/")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You don't have access to the model"}`))
	}))
	defer server.Close()
	up := &AWSUpstream{Endpoint: server.URL, Region: "us-east-1", Credentials: awscompat.Credentials{AccessKeyID: "a", SecretAccessKey: "b"}}
	_, err := up.Call(context.Background(), KindConverse, "m", []byte(`{}`))
	apiErr := requireCode(t, err, "AccessDeniedException")
	if apiErr.StatusCode != http.StatusForbidden || !strings.Contains(apiErr.Message, "real Bedrock") || !strings.Contains(apiErr.Message, "access to the model") {
		t.Fatalf("passed-through error: %+v", apiErr)
	}
}

func TestAWSUpstreamErrorWithoutHeaderUsesBodyType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"__type":"com.amazon.bedrock#ThrottlingException","Message":"slow down"}`))
	}))
	defer server.Close()
	up := &AWSUpstream{Endpoint: server.URL, Region: "us-east-1", Credentials: awscompat.Credentials{AccessKeyID: "a", SecretAccessKey: "b"}}
	_, err := up.Call(context.Background(), KindConverse, "m", []byte(`{}`))
	if apiErr := requireCode(t, err, "ThrottlingException"); !strings.Contains(apiErr.Message, "slow down") {
		t.Fatalf("message: %s", apiErr.Message)
	}
}

func TestAWSUpstreamNeedsCredentials(t *testing.T) {
	_, err := (&AWSUpstream{Endpoint: "http://127.0.0.1:1", Region: "us-east-1"}).Call(context.Background(), KindConverse, "m", nil)
	if apiErr := requireCode(t, err, "UnrecognizedClientException"); !strings.Contains(apiErr.Message, "STRATUS_BEDROCK_ACCESS_KEY_ID") {
		t.Fatalf("message should name the variables: %s", apiErr.Message)
	}
}

func TestAWSUpstreamUnreachable(t *testing.T) {
	up := &AWSUpstream{Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Credentials: awscompat.Credentials{AccessKeyID: "a", SecretAccessKey: "b"}}
	_, err := up.Call(context.Background(), KindConverse, "m", []byte(`{}`))
	requireCode(t, err, "ServiceUnavailableException")
}

func TestUpstreamFromEnv(t *testing.T) {
	up := UpstreamFromEnv(env(map[string]string{
		"AWS_REGION": "eu-central-1", "AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test",
		"STRATUS_BEDROCK_ACCESS_KEY_ID": "AKIDREAL", "STRATUS_BEDROCK_SECRET_ACCESS_KEY": "real", "STRATUS_BEDROCK_SESSION_TOKEN": "tok",
	}))
	if up.Region != "eu-central-1" || up.Endpoint != "https://bedrock-runtime.eu-central-1.amazonaws.com" {
		t.Fatalf("region and endpoint: %+v", up)
	}
	if up.Credentials.AccessKeyID != "AKIDREAL" || up.Credentials.SessionToken != "tok" {
		t.Fatalf("STRATUS_BEDROCK_ keys win over AWS_ keys: %+v", up.Credentials)
	}
	if up.Describe() != "https://bedrock-runtime.eu-central-1.amazonaws.com (eu-central-1)" || strings.Contains(up.Describe(), "real") {
		t.Fatalf("describe names the endpoint and no secrets: %s", up.Describe())
	}

	fallback := UpstreamFromEnv(env(map[string]string{"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "b", "STRATUS_BEDROCK_ENDPOINT": "http://localhost:9/", "STRATUS_BEDROCK_REGION": "ap-south-1"}))
	if fallback.Credentials.AccessKeyID != "a" || fallback.Endpoint != "http://localhost:9" || fallback.Region != "ap-south-1" {
		t.Fatalf("fallbacks: %+v", fallback)
	}
	if UpstreamFromEnv(env(nil)).Region != "us-east-1" {
		t.Fatal("region defaults to us-east-1")
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{"STRATUS_BEDROCK_MODE": "record", "STRATUS_BEDROCK_FIXTURES": "testdata/bedrock"}))
	if err != nil || cfg.Mode != ModeRecord || !filepath.IsAbs(cfg.FixtureDir) || !strings.HasSuffix(filepath.ToSlash(cfg.FixtureDir), "testdata/bedrock") {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if cfg, err := ConfigFromEnv(env(nil)); err != nil || cfg.Mode != ModeReplay || cfg.FixtureDir != "" {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	if _, err := ConfigFromEnv(env(map[string]string{"STRATUS_BEDROCK_MODE": "live"})); err == nil || !strings.Contains(err.Error(), "STRATUS_BEDROCK_MODE") {
		t.Fatalf("bad mode names the variable: %v", err)
	}
}
