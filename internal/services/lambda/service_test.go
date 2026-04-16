package lambda

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

func TestGetFunctionCodeSigningConfigReturnsProviderCompatibleJSON(t *testing.T) {
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	svc := NewService(Options{
		Metadata: metadata,
		Blobs:    fsblob.New(t.TempDir()),
	})

	if _, err := svc.Provision(ProvisionInput{
		CodeZip:      emptyZip(t),
		FunctionName: "demo-function",
		Handler:      "index.handler",
		Role:         "arn:aws:iam::000000000000:role/demo",
		Runtime:      "nodejs20.x",
		Timeout:      3,
	}); err != nil {
		t.Fatalf("provision function: %v", err)
	}

	req := httptest.NewRequest("GET", "http://localhost:4566/2020-06-30/functions/demo-function/code-signing-config", nil)
	rec := httptest.NewRecorder()
	err = svc.Handle(rec, req, "GetFunctionCodeSigningConfig")
	if err != nil {
		t.Fatalf("handle request: %v", err)
	}

	if rec.Code != 200 {
		t.Fatalf("unexpected status code: %d", rec.Code)
	}

	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload["FunctionName"] != "demo-function" {
		t.Fatalf("unexpected function name: %+v", payload)
	}
	if payload["CodeSigningConfigArn"] != "" {
		t.Fatalf("unexpected code signing config arn: %+v", payload)
	}
}

func TestAddPermissionAndGetPolicy(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "demo-function")

	addReq := httptest.NewRequest(http.MethodPost, "http://localhost:4566/2015-03-31/functions/demo-function/policy", bytes.NewBufferString(`{
		"Action":"lambda:InvokeFunction",
		"Principal":"apigateway.amazonaws.com",
		"SourceArn":"arn:aws:execute-api:us-east-1:000000000000:demo/*/POST/items",
		"StatementId":"sid-1"
	}`))
	addRec := httptest.NewRecorder()
	if err := svc.Handle(addRec, addReq, "AddPermission"); err != nil {
		t.Fatalf("add permission: %v", err)
	}
	if addRec.Code != http.StatusCreated {
		t.Fatalf("unexpected add permission status: %d", addRec.Code)
	}

	var addPayload map[string]string
	if err := json.Unmarshal(addRec.Body.Bytes(), &addPayload); err != nil {
		t.Fatalf("unmarshal add permission response: %v", err)
	}
	if addPayload["Statement"] == "" {
		t.Fatalf("expected statement in add permission response: %+v", addPayload)
	}

	getReq := httptest.NewRequest(http.MethodGet, "http://localhost:4566/2015-03-31/functions/demo-function/policy", nil)
	getRec := httptest.NewRecorder()
	if err := svc.Handle(getRec, getReq, "GetPolicy"); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if getRec.Code != http.StatusOK {
		t.Fatalf("unexpected get policy status: %d", getRec.Code)
	}

	var getPayload map[string]string
	if err := json.Unmarshal(getRec.Body.Bytes(), &getPayload); err != nil {
		t.Fatalf("unmarshal get policy response: %v", err)
	}
	if getPayload["Policy"] == "" || getPayload["RevisionId"] == "" {
		t.Fatalf("unexpected get policy payload: %+v", getPayload)
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()

	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })

	return NewService(Options{
		Metadata: metadata,
		Blobs:    fsblob.New(t.TempDir()),
	})
}

func provisionTestFunction(t *testing.T, svc *Service, functionName string) {
	t.Helper()

	if _, err := svc.Provision(ProvisionInput{
		CodeZip:      emptyZip(t),
		FunctionName: functionName,
		Handler:      "index.handler",
		Role:         "arn:aws:iam::000000000000:role/demo",
		Runtime:      "nodejs20.x",
		Timeout:      3,
	}); err != nil {
		t.Fatalf("provision function: %v", err)
	}
}

func emptyZip(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}
