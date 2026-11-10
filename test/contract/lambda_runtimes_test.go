package contract

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests run functions on AWS's own Lambda base images through the
// Runtime Interface Emulator. They need Docker and the images; CI pre-pulls
// public.ecr.aws/lambda/{java:21,provided:al2023,python:3.11}.

const javaHandlerSource = `package example;

import java.util.HashMap;
import java.util.Map;

public class Handler {
    private int count = 0;

    public Map<String, Object> handleRequest(Map<String, Object> event) {
        count++;
        if (event != null && "boom".equals(event.get("mode"))) {
            throw new IllegalStateException("java boom");
        }
        Map<String, Object> out = new HashMap<>();
        out.put("runtime", "java");
        out.put("count", count);
        out.put("name", event == null ? null : event.get("name"));
        out.put("function", System.getenv("AWS_LAMBDA_FUNCTION_NAME"));
        return out;
    }
}
`

// goBootstrapSource is a custom runtime written against the Lambda Runtime
// API with only the standard library, as a Go or Rust binary would be.
const goBootstrapSource = `package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	api := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation/"
	count := 0
	for {
		resp, err := http.Get(api + "next")
		if err != nil {
			fmt.Fprintln(os.Stderr, "next:", err)
			os.Exit(1)
		}
		id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
		event, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		count++
		fmt.Println("go handler", os.Getenv("_HANDLER"), "invocation", count)
		body := fmt.Sprintf("{\"runtime\":\"go\",\"count\":%d,\"event\":%s}", count, event)
		post, err := http.Post(api+id+"/response", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			fmt.Fprintln(os.Stderr, "response:", err)
			os.Exit(1)
		}
		post.Body.Close()
	}
}
`

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
}

func invokeJSON(t *testing.T, baseURL, function, payload string) (map[string]any, string) {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "out.json")
	meta := runAWS(t, baseURL,
		"lambda", "invoke",
		"--function-name", function,
		"--payload", payload,
		"--log-type", "Tail",
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
		outPath,
	)
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read invoke output: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode invoke output: %v\n%s\nmeta: %s", err, body, meta)
	}
	return decoded, string(meta)
}

func TestLambdaJava21RuntimeRunsOnAWSBaseImage(t *testing.T) {
	requireDocker(t)
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not installed")
	}

	srcDir := t.TempDir()
	classDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "example", "Handler.java")
	if err := os.MkdirAll(filepath.Dir(srcFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte(javaHandlerSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("javac", "--release", "17", "-d", classDir, srcFile).CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, out)
	}
	class, err := os.ReadFile(filepath.Join(classDir, "example", "Handler.class"))
	if err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "java.zip")
	if err := writeZip(zipPath, map[string]string{"example/Handler.class": string(class)}); err != nil {
		t.Fatal(err)
	}

	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "java-orders",
		"--runtime", "java21",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "example.Handler::handleRequest",
		"--timeout", "30",
		"--zip-file", "fileb://"+zipPath,
	)

	first, meta := invokeJSON(t, h.baseURL, "java-orders", `{"name":"first"}`)
	if first["runtime"] != "java" || first["name"] != "first" || first["count"] != float64(1) || first["function"] != "java-orders" {
		t.Fatalf("unexpected first java result: %v\n%s", first, h.output.String())
	}
	if !strings.Contains(meta, "LogResult") {
		t.Fatalf("expected Tail logs: %s", meta)
	}
	second, _ := invokeJSON(t, h.baseURL, "java-orders", `{"name":"second"}`)
	if second["count"] != float64(2) {
		t.Fatalf("expected a warm JVM on the second invoke: %v", second)
	}

	failure, meta := invokeJSON(t, h.baseURL, "java-orders", `{"mode":"boom"}`)
	if !strings.Contains(meta, "FunctionError") || !strings.Contains(failure["errorMessage"].(string), "java boom") {
		t.Fatalf("expected a function error from the Java exception: %v\n%s", failure, meta)
	}
}

func TestLambdaProvidedAL2023RunsGoBootstrap(t *testing.T) {
	requireDocker(t)

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(goBootstrapSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module bootstrap\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(t.TempDir(), "bootstrap")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = srcDir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build bootstrap: %v\n%s", err, out)
	}
	bin, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(t.TempDir(), "go.zip")
	if err := writeZip(zipPath, map[string]string{"bootstrap": string(bin)}); err != nil {
		t.Fatal(err)
	}

	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "go-orders",
		"--runtime", "provided.al2023",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "bootstrap",
		"--zip-file", "fileb://"+zipPath,
	)

	first, meta := invokeJSON(t, h.baseURL, "go-orders", `{"order":42}`)
	event, _ := first["event"].(map[string]any)
	if first["runtime"] != "go" || first["count"] != float64(1) || event["order"] != float64(42) {
		t.Fatalf("unexpected go result: %v\n%s", first, h.output.String())
	}
	if !strings.Contains(meta, "LogResult") {
		t.Fatalf("expected Tail logs: %s", meta)
	}
	second, _ := invokeJSON(t, h.baseURL, "go-orders", `{}`)
	if second["count"] != float64(2) {
		t.Fatalf("expected the bootstrap process to stay warm: %v", second)
	}
}

func TestLambdaContainerImageFunctionFromAWSBaseImage(t *testing.T) {
	requireDocker(t)

	buildDir := t.TempDir()
	files := map[string]string{
		"Dockerfile": "FROM public.ecr.aws/lambda/python:3.11\nCOPY app.py ${LAMBDA_TASK_ROOT}/\nCMD [\"app.handler\"]\n",
		"app.py":     "def handler(event, context):\n    return {'packaged': 'image', 'echo': event}\n\ndef other(event, context):\n    return {'packaged': 'image', 'handler': 'other'}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(buildDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tag := "stratus-contract/python-image-fn:latest"
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, buildDir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "image-fn",
		"--package-type", "Image",
		"--code", "ImageUri="+tag,
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
	)
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "image-fn-other",
		"--package-type", "Image",
		"--code", "ImageUri="+tag,
		"--image-config", "Command=app.other",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
	)

	got, _ := invokeJSON(t, h.baseURL, "image-fn", `{"k":"v"}`)
	echo, _ := got["echo"].(map[string]any)
	if got["packaged"] != "image" || echo["k"] != "v" {
		t.Fatalf("unexpected image function result: %v\n%s", got, h.output.String())
	}
	other, _ := invokeJSON(t, h.baseURL, "image-fn-other", `{}`)
	if other["handler"] != "other" {
		t.Fatalf("expected ImageConfig.Command to pick the handler: %v", other)
	}

	cfg := runAWS(t, h.baseURL, "lambda", "get-function", "--function-name", "image-fn", "--output", "json")
	if !strings.Contains(string(cfg), tag) || !strings.Contains(string(cfg), `"PackageType": "Image"`) {
		t.Fatalf("expected image metadata in get-function: %s", cfg)
	}
}

// An image on a non-AWS base starts the runtime interface client directly.
// Stratus wraps it in an emulator copied from the provided.al2023 image, so
// it runs the same as an AWS base image.
func TestLambdaContainerImageOnCustomBaseIsWrappedInEmulator(t *testing.T) {
	requireDocker(t)

	buildDir := t.TempDir()
	files := map[string]string{
		"Dockerfile": "FROM python:3.11-slim\n" +
			"RUN pip install --no-cache-dir awslambdaric\n" +
			"WORKDIR /function\n" +
			"COPY app.py .\n" +
			"ENTRYPOINT [\"/usr/local/bin/python\", \"-m\", \"awslambdaric\"]\n" +
			"CMD [\"app.handler\"]\n",
		"app.py": "def handler(event, context):\n    return {'base': 'custom', 'function': context.function_name}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(buildDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tag := "stratus-contract/custom-base-fn:latest"
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, buildDir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "custom-base-fn",
		"--package-type", "Image",
		"--code", "ImageUri="+tag,
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
	)

	got, _ := invokeJSON(t, h.baseURL, "custom-base-fn", `{}`)
	if got["base"] != "custom" || got["function"] != "custom-base-fn" {
		t.Fatalf("unexpected custom-base result: %v\n%s", got, h.output.String())
	}
}
