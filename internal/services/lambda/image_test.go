package lambda

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
)

type recordingInvoker struct{ specs []FunctionSpec }

func (r *recordingInvoker) Invoke(_ context.Context, spec FunctionSpec, _ []byte) (InvokeResult, error) {
	r.specs = append(r.specs, spec)
	return InvokeResult{Payload: []byte(`{}`)}, nil
}
func (r *recordingInvoker) CleanupFunction(context.Context, string) error { return nil }
func (r *recordingInvoker) Close() error                                  { return nil }

func createFunctionRequest(t *testing.T, svc *Service, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://localhost:4566/2015-03-31/functions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	return rec, svc.Handle(rec, req, "CreateFunction")
}

func TestCreateImageFunctionStoresImageAndReportsIt(t *testing.T) {
	svc := newTestService(t)
	invoker := &recordingInvoker{}
	svc.invoker = invoker

	rec, err := createFunctionRequest(t, svc, `{
		"FunctionName":"orders",
		"Role":"arn:aws:iam::000000000000:role/demo",
		"PackageType":"Image",
		"Code":{"ImageUri":"orders-fn:latest"},
		"ImageConfig":{"Command":["app.handler"]}
	}`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	var created struct {
		PackageType         string
		ImageConfigResponse struct{ ImageConfig ImageConfig }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.PackageType != "Image" || created.ImageConfigResponse.ImageConfig.Command[0] != "app.handler" {
		t.Fatalf("create response = %s", rec.Body.Bytes())
	}

	getReq := httptest.NewRequest(http.MethodGet, "http://localhost:4566/2015-03-31/functions/orders", nil)
	getRec := httptest.NewRecorder()
	if err := svc.Handle(getRec, getReq, "GetFunction"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Code struct{ ImageUri, RepositoryType string }
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code.ImageUri != "orders-fn:latest" || got.Code.RepositoryType != "ECR" {
		t.Fatalf("get response = %s", getRec.Body.Bytes())
	}

	if _, err := svc.InvokeByName(context.Background(), "orders", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	spec := invoker.specs[0]
	if spec.PackageType != PackageTypeImage || spec.ImageURI != "orders-fn:latest" || spec.ImageConfig.Command[0] != "app.handler" {
		t.Fatalf("invoker spec = %+v", spec)
	}
}

func TestZipFunctionSpecDefaultsToZipPackage(t *testing.T) {
	svc := newTestService(t)
	invoker := &recordingInvoker{}
	svc.invoker = invoker
	provisionTestFunction(t, svc, "zipped")

	if _, err := svc.InvokeByName(context.Background(), "zipped", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if got := invoker.specs[0]; got.PackageType != PackageTypeZip || got.Region == "" {
		t.Fatalf("spec = %+v", got)
	}
}

func TestCreateFunctionPackageValidation(t *testing.T) {
	cases := map[string]string{
		"image without uri":     `{"FunctionName":"f","Role":"r","PackageType":"Image","Code":{}}`,
		"image with runtime":    `{"FunctionName":"f","Role":"r","PackageType":"Image","Runtime":"java21","Code":{"ImageUri":"x:1"}}`,
		"image with layers":     `{"FunctionName":"f","Role":"r","PackageType":"Image","Layers":["arn:aws:lambda:us-east-1:000000000000:layer:l:1"],"Code":{"ImageUri":"x:1"}}`,
		"zip with image uri":    `{"FunctionName":"f","Role":"r","Runtime":"java21","Handler":"h","Code":{"ZipFile":"UEsFBgAAAAAAAAAAAAAAAAAAAAAAAA==","ImageUri":"x:1"}}`,
		"zip with image config": `{"FunctionName":"f","Role":"r","Runtime":"java21","Handler":"h","ImageConfig":{"Command":["x"]},"Code":{"ZipFile":"UEsFBgAAAAAAAAAAAAAAAAAAAAAAAA=="}}`,
		"unknown package type":  `{"FunctionName":"f","Role":"r","PackageType":"Jar","Code":{}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := createFunctionRequest(t, newTestService(t), body)
			var apiErr *apierror.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "InvalidParameterValueException" {
				t.Fatalf("error = %v, want InvalidParameterValueException", err)
			}
		})
	}
}

func TestExtractKeepsExecuteBitsAndMarksBootstrapExecutable(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows files carry no execute bits")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, mode os.FileMode) {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("x"))
	}
	add("bootstrap", 0o644) // as zipped on Windows: no execute bit
	add("bin/tool", 0o755)
	add("data.txt", 0o644)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "src")
	if err := extractArchiveToDir(dir, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	for name, wantExec := range map[string]bool{"bootstrap": true, "bin/tool": true, "data.txt": false} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if gotExec := info.Mode().Perm()&0o100 != 0; gotExec != wantExec {
			t.Fatalf("%s executable = %v, want %v (mode %v)", name, gotExec, wantExec, info.Mode())
		}
	}
}
