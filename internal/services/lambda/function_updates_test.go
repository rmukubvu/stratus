package lambda

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// cleanupInvoker records which warm environments were discarded.
type cleanupInvoker struct {
	mu      sync.Mutex
	cleaned []string
}

func (c *cleanupInvoker) Invoke(context.Context, FunctionSpec, []byte) (InvokeResult, error) {
	return InvokeResult{Payload: []byte(`{}`)}, nil
}
func (c *cleanupInvoker) CleanupFunction(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleaned = append(c.cleaned, key)
	return nil
}
func (c *cleanupInvoker) Close() error { return nil }

func zipWith(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func configuration(t *testing.T, body []byte) functionConfigurationResponse {
	t.Helper()
	var out functionConfigurationResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode configuration: %v\n%s", err, body)
	}
	return out
}

func TestListFunctionsPagesInNameOrder(t *testing.T) {
	svc := newTestService(t)
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		provisionTestFunction(t, svc, name)
	}

	rec, err := call(t, svc, http.MethodGet, "/2015-03-31/functions?MaxItems=2", "ListFunctions", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var page struct {
		Functions  []functionConfigurationResponse
		NextMarker string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Functions) != 2 || page.Functions[0].FunctionName != "alpha" || page.Functions[1].FunctionName != "bravo" || page.NextMarker != "charlie" {
		t.Fatalf("unexpected first page: %+v", page)
	}

	rec, err = call(t, svc, http.MethodGet, "/2015-03-31/functions?Marker="+page.NextMarker, "ListFunctions", "")
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	page.NextMarker = ""
	page.Functions = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Functions) != 1 || page.Functions[0].FunctionName != "charlie" || page.NextMarker != "" {
		t.Fatalf("unexpected last page: %+v", page)
	}

	if _, err := call(t, svc, http.MethodGet, "/2015-03-31/functions?MaxItems=0", "ListFunctions", ""); errorCode(err) != "InvalidParameterValueException" {
		t.Fatalf("MaxItems=0 should be rejected, got %v", err)
	}
}

func TestUpdateFunctionCodeReplacesTheCodeAndRecyclesTheEnvironment(t *testing.T) {
	svc := newTestService(t)
	inv := &cleanupInvoker{}
	svc.invoker = inv
	provisionTestFunction(t, svc, "worker")
	before, _ := svc.loadRecord("worker")

	newZip := zipWith(t, "index.js", "exports.handler = async () => 'v2'")
	body := `{"ZipFile":"` + base64.StdEncoding.EncodeToString(newZip) + `","RevisionId":"` + before.RevisionID + `"}`
	rec, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/code", "UpdateFunctionCode", body)
	if err != nil {
		t.Fatalf("update code: %v", err)
	}
	cfg := configuration(t, rec.Body.Bytes())
	if cfg.CodeSha256 == before.CodeSHA256 || cfg.CodeSize != int64(len(newZip)) || cfg.RevisionID == before.RevisionID {
		t.Fatalf("code not replaced: %+v", cfg)
	}
	source := svc.specFromRecord(before).CodeDir
	if got, err := os.ReadFile(filepath.Join(source, "index.js")); err != nil || string(got) != "exports.handler = async () => 'v2'" {
		t.Fatalf("new source not extracted: %q, %v", got, err)
	}
	if len(inv.cleaned) != 1 || inv.cleaned[0] != RuntimeKey(svc.scope, "worker") {
		t.Fatalf("warm environment not recycled: %v", inv.cleaned)
	}

	if _, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/code", "UpdateFunctionCode", body); errorCode(err) != "PreconditionFailedException" {
		t.Fatalf("a stale RevisionId should be rejected, got %v", err)
	}
}

func TestUpdateFunctionCodeDryRunAndPublish(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "worker")
	before, _ := svc.loadRecord("worker")
	zipB64 := base64.StdEncoding.EncodeToString(zipWith(t, "index.js", "v2"))

	if _, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/code", "UpdateFunctionCode", `{"DryRun":true,"ZipFile":"`+zipB64+`"}`); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if after, _ := svc.loadRecord("worker"); after.CodeSHA256 != before.CodeSHA256 {
		t.Fatalf("a dry run changed the code")
	}

	rec, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/code", "UpdateFunctionCode", `{"Publish":true,"ZipFile":"`+zipB64+`"}`)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if cfg := configuration(t, rec.Body.Bytes()); cfg.Version != "1" {
		t.Fatalf("published version = %q, want 1", cfg.Version)
	}
}

func TestUpdateFunctionCodeValidatesThePackage(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "worker")

	for name, body := range map[string]string{
		"no code":         `{}`,
		"image for a zip": `{"ImageUri":"repo/image:latest"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/code", "UpdateFunctionCode", body); errorCode(err) != "InvalidParameterValueException" {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/code", "UpdateFunctionCode", `{"S3Bucket":"b","S3Key":"k"}`); errorCode(err) != "NotImplementedException" {
		t.Fatalf("S3 code: %v", err)
	}
	if _, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/missing/code", "UpdateFunctionCode", `{}`); errorCode(err) != "ResourceNotFoundException" {
		t.Fatalf("missing function: %v", err)
	}
}

func TestUpdateFunctionConfigurationChangesOnlyWhatItNames(t *testing.T) {
	svc := newTestService(t)
	inv := &cleanupInvoker{}
	svc.invoker = inv
	provisionTestFunction(t, svc, "worker")

	rec, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/configuration", "UpdateFunctionConfiguration",
		`{"Timeout":30,"Environment":{"Variables":{"TABLE":"orders"}}}`)
	if err != nil {
		t.Fatalf("update configuration: %v", err)
	}
	cfg := configuration(t, rec.Body.Bytes())
	if cfg.Timeout != 30 || cfg.Environment.Variables["TABLE"] != "orders" || cfg.Handler != "index.handler" || cfg.MemorySize != 128 {
		t.Fatalf("unexpected configuration: %+v", cfg)
	}
	if len(inv.cleaned) != 1 {
		t.Fatalf("warm environment not recycled: %v", inv.cleaned)
	}

	for name, body := range map[string]string{
		"timeout too long":    `{"Timeout":901}`,
		"memory too small":    `{"MemorySize":64}`,
		"empty handler":       `{"Handler":""}`,
		"image config on zip": `{"ImageConfig":{"Command":["x"]}}`,
		"unknown layer":       `{"Layers":["arn:aws:lambda:us-east-1:000000000000:layer:nope:1"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/configuration", "UpdateFunctionConfiguration", body)
			if code := errorCode(err); code != "InvalidParameterValueException" && code != "ResourceNotFoundException" {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := call(t, svc, http.MethodPut, "/2015-03-31/functions/worker/configuration", "UpdateFunctionConfiguration", `{"VpcConfig":{"SubnetIds":["s"]}}`); errorCode(err) != "NotImplementedException" {
		t.Fatalf("unsupported feature: %v", err)
	}
	if after, _ := svc.loadRecord("worker"); after.Timeout != 30 {
		t.Fatalf("a rejected update changed the function: %+v", after)
	}
}
