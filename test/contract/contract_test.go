package contract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

type harness struct {
	t       *testing.T
	cmd     *exec.Cmd
	port    int
	baseURL string
	dataDir string
	output  *bytes.Buffer
}

func TestHealthEndpoint(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	resp, err := http.Get(h.baseURL + "/_stratus/health")
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, body)
	}
}

func TestAWSCLICallerIdentity(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	out := runAWS(t, h.baseURL, "sts", "get-caller-identity", "--output", "json")

	var payload struct {
		Account string `json:"Account"`
		ARN     string `json:"Arn"`
		UserID  string `json:"UserId"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode aws cli output: %v\n%s", err, out)
	}

	if payload.Account != "000000000000" {
		t.Fatalf("unexpected account: %+v", payload)
	}
	if !strings.Contains(payload.ARN, "arn:aws:iam::000000000000:") {
		t.Fatalf("unexpected arn: %+v", payload)
	}
	if payload.UserID == "" {
		t.Fatalf("unexpected empty user id: %+v", payload)
	}
}

func TestSTSPermissiveAuthAndMalformedAction(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	okReq, err := http.NewRequest(http.MethodPost, h.baseURL+"/", strings.NewReader("Action=GetCallerIdentity&Version=2011-06-15"))
	if err != nil {
		t.Fatalf("new ok request: %v", err)
	}
	okReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	okReq.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260407/us-east-1/sts/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatalf("ok request failed: %v", err)
	}
	okBody, _ := io.ReadAll(okResp.Body)
	okResp.Body.Close()

	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected sts status %d: %s", okResp.StatusCode, okBody)
	}
	if !strings.Contains(string(okBody), "<UserId>AKIDEXAMPLE:stratus</UserId>") {
		t.Fatalf("expected parsed access key in user id, got: %s", okBody)
	}

	badReq, err := http.NewRequest(http.MethodPost, h.baseURL+"/", strings.NewReader("Action=NoSuchAction&Version=2011-06-15"))
	if err != nil {
		t.Fatalf("new bad request: %v", err)
	}
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("bad request failed: %v", err)
	}
	badBody, _ := io.ReadAll(badResp.Body)
	badResp.Body.Close()

	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected bad sts status %d: %s", badResp.StatusCode, badBody)
	}
	if !strings.Contains(string(badBody), "<Code>InvalidAction</Code>") {
		t.Fatalf("expected InvalidAction response, got: %s", badBody)
	}
}

func TestAWSCLIS3PathStyleAndPersistence(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	bucket := "mission-two-bucket"
	key := "fixtures/hello.txt"
	sourcePath := filepath.Join(t.TempDir(), "hello.txt")
	expected := "hello from stratus\n"
	if err := os.WriteFile(sourcePath, []byte(expected), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	runAWS(t, h.baseURL, "s3api", "create-bucket", "--bucket", bucket)

	listBucketsOut := runAWS(t, h.baseURL, "s3api", "list-buckets", "--output", "json")
	if !strings.Contains(string(listBucketsOut), bucket) {
		t.Fatalf("expected bucket in list-buckets output: %s", listBucketsOut)
	}

	putOut := runAWS(t, h.baseURL, "s3api", "put-object", "--bucket", bucket, "--key", key, "--body", sourcePath, "--output", "json")
	if !strings.Contains(string(putOut), "\"ETag\"") {
		t.Fatalf("expected ETag in put-object output: %s", putOut)
	}

	listObjectsOut := runAWS(t, h.baseURL, "s3api", "list-objects-v2", "--bucket", bucket, "--output", "json")
	if !strings.Contains(string(listObjectsOut), key) {
		t.Fatalf("expected key in list-objects-v2 output: %s", listObjectsOut)
	}

	downloadPath := filepath.Join(t.TempDir(), "download.txt")
	getOut := runAWS(t, h.baseURL, "s3api", "get-object", "--bucket", bucket, "--key", key, downloadPath, "--output", "json")
	if !strings.Contains(string(getOut), "\"ContentLength\"") {
		t.Fatalf("expected metadata in get-object output: %s", getOut)
	}

	gotBytes, err := os.ReadFile(downloadPath)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if string(gotBytes) != expected {
		t.Fatalf("unexpected downloaded bytes: %q", gotBytes)
	}

	h.Close()

	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	downloadAfterRestart := filepath.Join(t.TempDir(), "download-after-restart.txt")
	_ = runAWS(t, h.baseURL, "s3api", "get-object", "--bucket", bucket, "--key", key, downloadAfterRestart, "--output", "json")

	restartedBytes, err := os.ReadFile(downloadAfterRestart)
	if err != nil {
		t.Fatalf("read restarted download: %v", err)
	}
	if string(restartedBytes) != expected {
		t.Fatalf("unexpected restarted bytes: %q", restartedBytes)
	}

	runAWS(t, h.baseURL, "s3api", "delete-object", "--bucket", bucket, "--key", key)

	finalList := runAWS(t, h.baseURL, "s3api", "list-objects-v2", "--bucket", bucket, "--output", "json")
	if strings.Contains(string(finalList), key) {
		t.Fatalf("expected key to be deleted: %s", finalList)
	}
}

func TestAWSCLILambdaControlPlane(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	functionName := "mission-three-function"
	zipPath := filepath.Join(t.TempDir(), "function.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "def main(event, context):\n    return {'statusCode': 200}\n",
	}); err != nil {
		t.Fatalf("write lambda zip: %v", err)
	}

	createOut := runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", functionName,
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
		"--timeout", "9",
		"--environment", "Variables={FOO=bar,HELLO=world}",
		"--output", "json",
	)

	var created struct {
		FunctionName string `json:"FunctionName"`
		FunctionArn  string `json:"FunctionArn"`
		Runtime      string `json:"Runtime"`
		Handler      string `json:"Handler"`
		Timeout      int    `json:"Timeout"`
		Role         string `json:"Role"`
		CodeSize     int64  `json:"CodeSize"`
		CodeSha256   string `json:"CodeSha256"`
		Version      string `json:"Version"`
		Environment  struct {
			Variables map[string]string `json:"Variables"`
		} `json:"Environment"`
	}
	if err := json.Unmarshal(createOut, &created); err != nil {
		t.Fatalf("decode create-function output: %v\n%s", err, createOut)
	}
	if created.FunctionName != functionName || created.Runtime != "python3.11" || created.Handler != "handler.main" {
		t.Fatalf("unexpected create-function output: %+v", created)
	}
	if created.Timeout != 9 || created.Role == "" || created.CodeSize == 0 || created.CodeSha256 == "" {
		t.Fatalf("unexpected create-function output: %+v", created)
	}
	if created.Environment.Variables["FOO"] != "bar" {
		t.Fatalf("unexpected environment variables: %+v", created.Environment.Variables)
	}

	getOut := runAWS(t, h.baseURL, "lambda", "get-function", "--function-name", functionName, "--output", "json")
	var got struct {
		Code struct {
			Location       string `json:"Location"`
			RepositoryType string `json:"RepositoryType"`
		} `json:"Code"`
		Configuration struct {
			FunctionName string `json:"FunctionName"`
			Runtime      string `json:"Runtime"`
			Handler      string `json:"Handler"`
			CodeSize     int64  `json:"CodeSize"`
			CodeSha256   string `json:"CodeSha256"`
			FunctionArn  string `json:"FunctionArn"`
		} `json:"Configuration"`
	}
	if err := json.Unmarshal(getOut, &got); err != nil {
		t.Fatalf("decode get-function output: %v\n%s", err, getOut)
	}
	if got.Configuration.FunctionName != functionName || got.Configuration.CodeSha256 == "" || got.Code.Location == "" {
		t.Fatalf("unexpected get-function output: %+v", got)
	}

	cfgOut := runAWS(t, h.baseURL, "lambda", "get-function-configuration", "--function-name", functionName, "--output", "json")
	if !strings.Contains(string(cfgOut), "\"Timeout\": 9") && !strings.Contains(string(cfgOut), "\"Timeout\":9") {
		t.Fatalf("expected timeout in get-function-configuration output: %s", cfgOut)
	}

	badCmd := exec.Command("aws",
		"--endpoint-url", h.baseURL,
		"lambda", "create-function",
		"--function-name", "unsupported-layers",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
		"--layers", "arn:aws:lambda:us-east-1:000000000000:layer:test:1",
	)
	badCmd.Env = awsEnv()
	badOut, err := badCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected create-function with layers to fail")
	}
	if !strings.Contains(string(badOut), "NotImplementedException") {
		t.Fatalf("expected explicit unsupported error, got: %s", badOut)
	}

	h.Close()

	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	getAfterRestart := runAWS(t, h.baseURL, "lambda", "get-function", "--function-name", functionName, "--output", "json")
	if !strings.Contains(string(getAfterRestart), functionName) || !strings.Contains(string(getAfterRestart), created.CodeSha256) {
		t.Fatalf("expected durable lambda metadata after restart: %s", getAfterRestart)
	}

	runAWS(t, h.baseURL, "lambda", "delete-function", "--function-name", functionName)

	deletedCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "lambda", "get-function", "--function-name", functionName)
	deletedCmd.Env = awsEnv()
	deletedOut, err := deletedCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected deleted function lookup to fail")
	}
	if !strings.Contains(string(deletedOut), "ResourceNotFoundException") {
		t.Fatalf("expected ResourceNotFoundException after delete, got: %s", deletedOut)
	}
}

func TestAWSCLILambdaInvokeExecution(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	functionName := "mission-four-warm"
	zipPath := filepath.Join(t.TempDir(), "warm.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "counter = 0\n\ndef main(event, context):\n    global counter\n    counter += 1\n    print(f'invoke={counter}')\n    return {'count': counter, 'echo': event}\n",
	}); err != nil {
		t.Fatalf("write warm zip: %v", err)
	}

	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", functionName,
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
	)

	firstPayloadPath := filepath.Join(t.TempDir(), "first.json")
	firstMeta := runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", functionName,
		"--payload", "{\"message\":\"first\"}",
		"--log-type", "Tail",
		firstPayloadPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	if !strings.Contains(string(firstMeta), "\"StatusCode\": 200") && !strings.Contains(string(firstMeta), "\"StatusCode\":200") {
		t.Fatalf("unexpected first invoke metadata: %s", firstMeta)
	}
	if !strings.Contains(string(firstMeta), "LogResult") {
		t.Fatalf("expected Tail logs in invoke metadata: %s", firstMeta)
	}
	firstBody, err := os.ReadFile(firstPayloadPath)
	if err != nil {
		t.Fatalf("read first invoke payload: %v", err)
	}
	if !strings.Contains(string(firstBody), `"count": 1`) && !strings.Contains(string(firstBody), `"count":1`) {
		t.Fatalf("expected first cold invoke count 1: %s", firstBody)
	}

	secondPayloadPath := filepath.Join(t.TempDir(), "second.json")
	_ = runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", functionName,
		"--payload", "{\"message\":\"second\"}",
		secondPayloadPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	secondBody, err := os.ReadFile(secondPayloadPath)
	if err != nil {
		t.Fatalf("read second invoke payload: %v", err)
	}
	if !strings.Contains(string(secondBody), `"count": 2`) && !strings.Contains(string(secondBody), `"count":2`) {
		t.Fatalf("expected warm invoke count 2: %s", secondBody)
	}
}

func TestAWSCLILambdaInvokeErrorTimeoutAndCrash(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	errorZip := filepath.Join(t.TempDir(), "error.zip")
	if err := writeZip(errorZip, map[string]string{
		"handler.py": "def main(event, context):\n    print('before boom')\n    raise ValueError('boom')\n",
	}); err != nil {
		t.Fatalf("write error zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "mission-four-error",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+errorZip,
	)

	errorPayloadPath := filepath.Join(t.TempDir(), "error.json")
	errorMeta := runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", "mission-four-error",
		"--payload", "{}",
		"--log-type", "Tail",
		errorPayloadPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	if !strings.Contains(string(errorMeta), "FunctionError") {
		t.Fatalf("expected function error metadata: %s", errorMeta)
	}
	errorBody, err := os.ReadFile(errorPayloadPath)
	if err != nil {
		t.Fatalf("read error invoke payload: %v", err)
	}
	if !strings.Contains(string(errorBody), "boom") {
		t.Fatalf("expected handler error payload: %s", errorBody)
	}

	timeoutZip := filepath.Join(t.TempDir(), "timeout.zip")
	if err := writeZip(timeoutZip, map[string]string{
		"handler.py": "import time\n\ndef main(event, context):\n    time.sleep(2)\n    return {'ok': True}\n",
	}); err != nil {
		t.Fatalf("write timeout zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "mission-four-timeout",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+timeoutZip,
		"--timeout", "1",
	)

	timeoutPayloadPath := filepath.Join(t.TempDir(), "timeout.json")
	timeoutMeta := runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", "mission-four-timeout",
		"--payload", "{}",
		timeoutPayloadPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	if !strings.Contains(string(timeoutMeta), "FunctionError") {
		t.Fatalf("expected timeout function error metadata: %s", timeoutMeta)
	}
	timeoutBody, err := os.ReadFile(timeoutPayloadPath)
	if err != nil {
		t.Fatalf("read timeout invoke payload: %v", err)
	}
	if !strings.Contains(string(timeoutBody), "Task timed out") {
		t.Fatalf("expected timeout payload: %s", timeoutBody)
	}

	crashZip := filepath.Join(t.TempDir(), "crash.zip")
	if err := writeZip(crashZip, map[string]string{
		"handler.py": "import os\n\ndef main(event, context):\n    os._exit(1)\n",
	}); err != nil {
		t.Fatalf("write crash zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "mission-four-crash",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+crashZip,
	)

	crashCmd := exec.Command("aws",
		"--endpoint-url", h.baseURL,
		"lambda", "invoke",
		"--function-name", "mission-four-crash",
		"--payload", "{}",
		filepath.Join(t.TempDir(), "crash.json"),
		"--cli-binary-format", "raw-in-base64-out",
	)
	crashCmd.Env = awsEnv()
	crashOut, err := crashCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected crash invoke to fail")
	}
	if !strings.Contains(string(crashOut), "ServiceException") {
		t.Fatalf("expected ServiceException for crash path: %s", crashOut)
	}
}

func TestLambdaDeleteCleansWarmRuntime(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	functionName := "mission-four-delete"
	zipPath := filepath.Join(t.TempDir(), "delete.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "counter = 0\n\ndef main(event, context):\n    global counter\n    counter += 1\n    return {'count': counter}\n",
	}); err != nil {
		t.Fatalf("write delete zip: %v", err)
	}

	create := func() {
		runAWS(t, h.baseURL,
			"lambda", "create-function",
			"--function-name", functionName,
			"--runtime", "python3.11",
			"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
			"--handler", "handler.main",
			"--zip-file", "fileb://"+zipPath,
		)
	}

	invokeCount := func(path string) string {
		_ = runAWS(t, h.baseURL,
			"lambda", "invoke",
			"--function-name", functionName,
			"--payload", "{}",
			path,
			"--cli-binary-format", "raw-in-base64-out",
			"--output", "json",
		)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read invoke payload: %v", err)
		}
		return string(body)
	}

	create()
	first := invokeCount(filepath.Join(t.TempDir(), "first-delete.json"))
	if !strings.Contains(first, `"count": 1`) && !strings.Contains(first, `"count":1`) {
		t.Fatalf("expected initial count 1: %s", first)
	}
	second := invokeCount(filepath.Join(t.TempDir(), "second-delete.json"))
	if !strings.Contains(second, `"count": 2`) && !strings.Contains(second, `"count":2`) {
		t.Fatalf("expected warm count 2 before delete: %s", second)
	}

	runAWS(t, h.baseURL, "lambda", "delete-function", "--function-name", functionName)
	create()
	afterRecreate := invokeCount(filepath.Join(t.TempDir(), "after-recreate.json"))
	if !strings.Contains(afterRecreate, `"count": 1`) && !strings.Contains(afterRecreate, `"count":1`) {
		t.Fatalf("expected recreated function to start cold: %s", afterRecreate)
	}
}

func TestAWSCLISSMParameterWorkflow(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	putOut := runAWS(t, h.baseURL, "ssm", "put-parameter", "--name", "/app/config/foo", "--value", "bar", "--type", "String", "--output", "json")
	if !strings.Contains(string(putOut), `"Version": 1`) && !strings.Contains(string(putOut), `"Version":1`) {
		t.Fatalf("expected version in put-parameter output: %s", putOut)
	}

	getOut := runAWS(t, h.baseURL, "ssm", "get-parameter", "--name", "/app/config/foo", "--output", "json")
	if !strings.Contains(string(getOut), `/app/config/foo`) || !strings.Contains(string(getOut), `"Value": "bar"`) && !strings.Contains(string(getOut), `"Value":"bar"`) {
		t.Fatalf("unexpected get-parameter output: %s", getOut)
	}

	describeOut := runAWS(t, h.baseURL, "ssm", "describe-parameters", "--output", "json")
	if !strings.Contains(string(describeOut), `/app/config/foo`) {
		t.Fatalf("unexpected describe-parameters output: %s", describeOut)
	}
}

func TestAWSCLIDynamoDBBasicWorkflow(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	createOut := runAWS(t, h.baseURL,
		"dynamodb", "create-table",
		"--table-name", "books",
		"--attribute-definitions", "AttributeName=id,AttributeType=S",
		"--key-schema", "AttributeName=id,KeyType=HASH",
		"--billing-mode", "PAY_PER_REQUEST",
		"--output", "json",
	)
	if !strings.Contains(string(createOut), `"TableStatus": "ACTIVE"`) && !strings.Contains(string(createOut), `"TableStatus":"ACTIVE"`) {
		t.Fatalf("unexpected create-table output: %s", createOut)
	}

	listOut := runAWS(t, h.baseURL, "dynamodb", "list-tables", "--output", "json")
	if !strings.Contains(string(listOut), `"books"`) {
		t.Fatalf("unexpected list-tables output: %s", listOut)
	}

	runAWS(t, h.baseURL,
		"dynamodb", "put-item",
		"--table-name", "books",
		"--item", `{"id":{"S":"1"},"title":{"S":"Dune"}}`,
	)

	getOut := runAWS(t, h.baseURL,
		"dynamodb", "get-item",
		"--table-name", "books",
		"--key", `{"id":{"S":"1"}}`,
		"--output", "json",
	)
	if !strings.Contains(string(getOut), `"title"`) || !strings.Contains(string(getOut), `"Dune"`) {
		t.Fatalf("unexpected get-item output: %s", getOut)
	}
}

func TestAWSCLICloudWatchLogsWorkflow(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runAWS(t, h.baseURL, "logs", "create-log-group", "--log-group-name", "/stratus/test")
	runAWS(t, h.baseURL, "logs", "create-log-stream", "--log-group-name", "/stratus/test", "--log-stream-name", "main")

	putOut := runAWS(t, h.baseURL,
		"logs", "put-log-events",
		"--log-group-name", "/stratus/test",
		"--log-stream-name", "main",
		"--log-events", fmt.Sprintf("timestamp=%d,message=hello", time.Now().UnixMilli()),
		"--output", "json",
	)
	if !strings.Contains(string(putOut), "nextSequenceToken") {
		t.Fatalf("unexpected put-log-events output: %s", putOut)
	}

	describeOut := runAWS(t, h.baseURL, "logs", "describe-log-streams", "--log-group-name", "/stratus/test", "--output", "json")
	if !strings.Contains(string(describeOut), `"main"`) {
		t.Fatalf("unexpected describe-log-streams output: %s", describeOut)
	}
}

func TestAWSCLISQSWorkflowAndPersistence(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	createOut := runAWS(t, h.baseURL,
		"sqs", "create-queue",
		"--queue-name", "jobs",
		"--attributes", "VisibilityTimeout=30",
		"--output", "json",
	)
	if !strings.Contains(string(createOut), `"QueueUrl"`) {
		t.Fatalf("unexpected create-queue output: %s", createOut)
	}

	getURL := runAWS(t, h.baseURL, "sqs", "get-queue-url", "--queue-name", "jobs", "--output", "json")
	var urlPayload struct {
		QueueURL string `json:"QueueUrl"`
	}
	if err := json.Unmarshal(getURL, &urlPayload); err != nil {
		t.Fatalf("decode get-queue-url output: %v\n%s", err, getURL)
	}
	if urlPayload.QueueURL == "" {
		t.Fatalf("expected QueueUrl in output: %s", getURL)
	}

	sendOut := runAWS(t, h.baseURL,
		"sqs", "send-message",
		"--queue-url", urlPayload.QueueURL,
		"--message-body", "hello queue",
		"--output", "json",
	)
	if !strings.Contains(string(sendOut), `"MessageId"`) {
		t.Fatalf("unexpected send-message output: %s", sendOut)
	}

	h.Close()

	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	getURL = runAWS(t, h.baseURL, "sqs", "get-queue-url", "--queue-name", "jobs", "--output", "json")
	if err := json.Unmarshal(getURL, &urlPayload); err != nil {
		t.Fatalf("decode restarted get-queue-url output: %v\n%s", err, getURL)
	}

	receiveOut := runAWS(t, h.baseURL,
		"sqs", "receive-message",
		"--queue-url", urlPayload.QueueURL,
		"--attribute-names", "All",
		"--max-number-of-messages", "1",
		"--output", "json",
	)
	var receivePayload struct {
		Messages []struct {
			Body          string            `json:"Body"`
			ReceiptHandle string            `json:"ReceiptHandle"`
			Attributes    map[string]string `json:"Attributes"`
		} `json:"Messages"`
	}
	if err := json.Unmarshal(receiveOut, &receivePayload); err != nil {
		t.Fatalf("decode receive-message output: %v\n%s", err, receiveOut)
	}
	if len(receivePayload.Messages) != 1 {
		t.Fatalf("expected one message after restart: %s", receiveOut)
	}
	if receivePayload.Messages[0].Body != "hello queue" {
		t.Fatalf("unexpected message body: %s", receiveOut)
	}
	if receivePayload.Messages[0].ReceiptHandle == "" {
		t.Fatalf("expected receipt handle: %s", receiveOut)
	}
	if receivePayload.Messages[0].Attributes["SentTimestamp"] == "" {
		t.Fatalf("expected sent timestamp attributes: %s", receiveOut)
	}

	runAWS(t, h.baseURL,
		"sqs", "delete-message",
		"--queue-url", urlPayload.QueueURL,
		"--receipt-handle", receivePayload.Messages[0].ReceiptHandle,
	)

	emptyReceive := runAWS(t, h.baseURL,
		"sqs", "receive-message",
		"--queue-url", urlPayload.QueueURL,
		"--max-number-of-messages", "1",
		"--output", "json",
	)
	if strings.Contains(string(emptyReceive), `"Body"`) {
		t.Fatalf("expected queue to be empty after delete-message: %s", emptyReceive)
	}

	listOut := runAWS(t, h.baseURL, "sqs", "list-queues", "--output", "json")
	if !strings.Contains(string(listOut), urlPayload.QueueURL) {
		t.Fatalf("expected queue in list-queues output: %s", listOut)
	}

	runAWS(t, h.baseURL, "sqs", "delete-queue", "--queue-url", urlPayload.QueueURL)

	missingCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "sqs", "get-queue-url", "--queue-name", "jobs")
	missingCmd.Env = awsEnv()
	missingOut, err := missingCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected get-queue-url after delete to fail")
	}
	if !strings.Contains(string(missingOut), "NonExistentQueue") {
		t.Fatalf("expected NonExistentQueue after delete: %s", missingOut)
	}
}

func TestAWSCLIIAMRoleWorkflowAndPersistence(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	trustPolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	inlinePolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"logs:CreateLogGroup","Resource":"*"}]}`

	createOut := runAWS(t, h.baseURL,
		"iam", "create-role",
		"--role-name", "app-exec-role",
		"--assume-role-policy-document", trustPolicy,
		"--description", "execution role",
		"--output", "json",
	)
	if !strings.Contains(string(createOut), `"RoleName": "app-exec-role"`) && !strings.Contains(string(createOut), `"RoleName":"app-exec-role"`) {
		t.Fatalf("unexpected create-role output: %s", createOut)
	}
	if !strings.Contains(string(createOut), `arn:aws:iam::000000000000:role/app-exec-role`) {
		t.Fatalf("expected role arn in create-role output: %s", createOut)
	}

	runAWS(t, h.baseURL,
		"iam", "put-role-policy",
		"--role-name", "app-exec-role",
		"--policy-name", "lambda-logs",
		"--policy-document", inlinePolicy,
	)

	h.Close()

	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	getOut := runAWS(t, h.baseURL, "iam", "get-role", "--role-name", "app-exec-role", "--output", "json")
	if !strings.Contains(string(getOut), `"RoleName": "app-exec-role"`) && !strings.Contains(string(getOut), `"RoleName":"app-exec-role"`) {
		t.Fatalf("unexpected get-role output: %s", getOut)
	}
	if !strings.Contains(string(getOut), `"Description": "execution role"`) && !strings.Contains(string(getOut), `"Description":"execution role"`) {
		t.Fatalf("expected description in get-role output: %s", getOut)
	}

	listOut := runAWS(t, h.baseURL, "iam", "list-roles", "--output", "json")
	if !strings.Contains(string(listOut), `"app-exec-role"`) {
		t.Fatalf("unexpected list-roles output: %s", listOut)
	}

	getPolicyOut := runAWS(t, h.baseURL,
		"iam", "get-role-policy",
		"--role-name", "app-exec-role",
		"--policy-name", "lambda-logs",
		"--output", "json",
	)
	if !strings.Contains(string(getPolicyOut), `"PolicyName": "lambda-logs"`) && !strings.Contains(string(getPolicyOut), `"PolicyName":"lambda-logs"`) {
		t.Fatalf("unexpected get-role-policy output: %s", getPolicyOut)
	}
	if !strings.Contains(string(getPolicyOut), `logs:CreateLogGroup`) {
		t.Fatalf("expected policy document in get-role-policy output: %s", getPolicyOut)
	}

	listPoliciesOut := runAWS(t, h.baseURL, "iam", "list-role-policies", "--role-name", "app-exec-role", "--output", "json")
	if !strings.Contains(string(listPoliciesOut), `"lambda-logs"`) {
		t.Fatalf("unexpected list-role-policies output: %s", listPoliciesOut)
	}

	conflictCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "iam", "delete-role", "--role-name", "app-exec-role")
	conflictCmd.Env = awsEnv()
	conflictOut, err := conflictCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected delete-role with inline policy to fail")
	}
	if !strings.Contains(string(conflictOut), "DeleteConflict") {
		t.Fatalf("expected DeleteConflict when policies remain: %s", conflictOut)
	}

	runAWS(t, h.baseURL, "iam", "delete-role-policy", "--role-name", "app-exec-role", "--policy-name", "lambda-logs")
	runAWS(t, h.baseURL, "iam", "delete-role", "--role-name", "app-exec-role")

	missingCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "iam", "get-role", "--role-name", "app-exec-role")
	missingCmd.Env = awsEnv()
	missingOut, err := missingCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected get-role after delete to fail")
	}
	if !strings.Contains(string(missingOut), "NoSuchEntity") {
		t.Fatalf("expected NoSuchEntity after delete: %s", missingOut)
	}
}

func TestAWSCLICloudFormationWorkflowAndPersistence(t *testing.T) {
	dataDir := t.TempDir()
	h := startHarnessWithDataDir(t, dataDir)

	templatePath := filepath.Join(t.TempDir(), "template.json")
	templateBody := `{"AWSTemplateFormatVersion":"2010-09-09","Description":"minimal stratus stack","Resources":{}}`
	if err := os.WriteFile(templatePath, []byte(templateBody), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	validateOut := runAWS(t, h.baseURL,
		"cloudformation", "validate-template",
		"--template-body", "file://"+templatePath,
		"--output", "json",
	)
	if !strings.Contains(string(validateOut), `"Description": "minimal stratus stack"`) && !strings.Contains(string(validateOut), `"Description":"minimal stratus stack"`) {
		t.Fatalf("unexpected validate-template output: %s", validateOut)
	}

	createOut := runAWS(t, h.baseURL,
		"cloudformation", "create-stack",
		"--stack-name", "minimal-stack",
		"--template-body", "file://"+templatePath,
		"--output", "json",
	)
	if !strings.Contains(string(createOut), `"StackId"`) {
		t.Fatalf("unexpected create-stack output: %s", createOut)
	}

	h.Close()

	h = startHarnessWithDataDir(t, dataDir)
	defer h.Close()

	describeOut := runAWS(t, h.baseURL,
		"cloudformation", "describe-stacks",
		"--stack-name", "minimal-stack",
		"--output", "json",
	)
	if !strings.Contains(string(describeOut), `"StackStatus": "CREATE_COMPLETE"`) && !strings.Contains(string(describeOut), `"StackStatus":"CREATE_COMPLETE"`) {
		t.Fatalf("unexpected describe-stacks output: %s", describeOut)
	}
	if !strings.Contains(string(describeOut), `"Description": "minimal stratus stack"`) && !strings.Contains(string(describeOut), `"Description":"minimal stratus stack"`) {
		t.Fatalf("expected description in describe-stacks output: %s", describeOut)
	}

	listOut := runAWS(t, h.baseURL, "cloudformation", "list-stacks", "--output", "json")
	if !strings.Contains(string(listOut), `"minimal-stack"`) {
		t.Fatalf("unexpected list-stacks output: %s", listOut)
	}

	getTemplateOut := runAWS(t, h.baseURL,
		"cloudformation", "get-template",
		"--stack-name", "minimal-stack",
		"--output", "json",
	)
	if !strings.Contains(string(getTemplateOut), `"TemplateBody"`) || !strings.Contains(string(getTemplateOut), `minimal stratus stack`) {
		t.Fatalf("unexpected get-template output: %s", getTemplateOut)
	}

	runAWS(t, h.baseURL, "cloudformation", "delete-stack", "--stack-name", "minimal-stack")

	missingCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "cloudformation", "describe-stacks", "--stack-name", "minimal-stack")
	missingCmd.Env = awsEnv()
	missingOut, err := missingCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected describe-stacks after delete to fail")
	}
	if !strings.Contains(string(missingOut), "ValidationError") {
		t.Fatalf("expected ValidationError after delete: %s", missingOut)
	}
}

func TestAWSCLICloudFormationExecutesSupportedResources(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	templatePath := filepath.Join(t.TempDir(), "exec-template.json")
	templateBody := `{
  "AWSTemplateFormatVersion": "2010-09-09",
  "Description": "resource execution stack",
  "Resources": {
    "JobQueue": {
      "Type": "AWS::SQS::Queue",
      "Properties": {
        "QueueName": "cfn-job-queue",
        "VisibilityTimeout": 45
      }
    },
    "AppLogs": {
      "Type": "AWS::Logs::LogGroup",
      "Properties": {
        "LogGroupName": "/stratus/cfn/exec"
      }
    },
    "ExecRole": {
      "Type": "AWS::IAM::Role",
      "Properties": {
        "RoleName": "cfn-exec-role",
        "AssumeRolePolicyDocument": {
          "Version": "2012-10-17",
          "Statement": [
            {
              "Effect": "Allow",
              "Principal": {
                "Service": "lambda.amazonaws.com"
              },
              "Action": "sts:AssumeRole"
            }
          ]
        },
        "Policies": [
          {
            "PolicyName": "logs-access",
            "PolicyDocument": {
              "Version": "2012-10-17",
              "Statement": [
                {
                  "Effect": "Allow",
                  "Action": "logs:CreateLogGroup",
                  "Resource": "*"
                }
              ]
            }
          }
        ]
      }
    },
    "BooksTable": {
      "Type": "AWS::DynamoDB::Table",
      "Properties": {
        "TableName": "cfn-books",
        "AttributeDefinitions": [
          {
            "AttributeName": "id",
            "AttributeType": "S"
          }
        ],
        "KeySchema": [
          {
            "AttributeName": "id",
            "KeyType": "HASH"
          }
        ],
        "BillingMode": "PAY_PER_REQUEST"
      }
    }
  }
}`
	if err := os.WriteFile(templatePath, []byte(templateBody), 0o644); err != nil {
		t.Fatalf("write exec template: %v", err)
	}

	runAWS(t, h.baseURL,
		"cloudformation", "create-stack",
		"--stack-name", "exec-stack",
		"--template-body", "file://"+templatePath,
	)

	queueOut := runAWS(t, h.baseURL, "sqs", "get-queue-url", "--queue-name", "cfn-job-queue", "--output", "json")
	if !strings.Contains(string(queueOut), `"QueueUrl"`) {
		t.Fatalf("expected queue created by stack: %s", queueOut)
	}

	queueAttrs := runAWS(t, h.baseURL,
		"sqs", "get-queue-attributes",
		"--queue-url", h.baseURL+"/000000000000/cfn-job-queue",
		"--attribute-names", "VisibilityTimeout",
		"--output", "json",
	)
	if !strings.Contains(string(queueAttrs), `"VisibilityTimeout": "45"`) && !strings.Contains(string(queueAttrs), `"VisibilityTimeout":"45"`) {
		t.Fatalf("expected queue attributes from stack: %s", queueAttrs)
	}

	runAWS(t, h.baseURL, "logs", "create-log-stream", "--log-group-name", "/stratus/cfn/exec", "--log-stream-name", "main")
	logStreams := runAWS(t, h.baseURL, "logs", "describe-log-streams", "--log-group-name", "/stratus/cfn/exec", "--output", "json")
	if !strings.Contains(string(logStreams), `"main"`) {
		t.Fatalf("expected log group created by stack: %s", logStreams)
	}

	roleOut := runAWS(t, h.baseURL, "iam", "get-role", "--role-name", "cfn-exec-role", "--output", "json")
	if !strings.Contains(string(roleOut), `"RoleName": "cfn-exec-role"`) && !strings.Contains(string(roleOut), `"RoleName":"cfn-exec-role"`) {
		t.Fatalf("expected role created by stack: %s", roleOut)
	}

	rolePolicies := runAWS(t, h.baseURL, "iam", "list-role-policies", "--role-name", "cfn-exec-role", "--output", "json")
	if !strings.Contains(string(rolePolicies), `"logs-access"`) {
		t.Fatalf("expected inline role policy created by stack: %s", rolePolicies)
	}

	tableList := runAWS(t, h.baseURL, "dynamodb", "list-tables", "--output", "json")
	if !strings.Contains(string(tableList), `"cfn-books"`) {
		t.Fatalf("expected table created by stack: %s", tableList)
	}

	runAWS(t, h.baseURL,
		"dynamodb", "put-item",
		"--table-name", "cfn-books",
		"--item", `{"id":{"S":"1"},"title":{"S":"from-cfn"}}`,
	)

	tableItem := runAWS(t, h.baseURL,
		"dynamodb", "get-item",
		"--table-name", "cfn-books",
		"--key", `{"id":{"S":"1"}}`,
		"--output", "json",
	)
	if !strings.Contains(string(tableItem), `from-cfn`) {
		t.Fatalf("expected table item in cfn-created table: %s", tableItem)
	}

	runAWS(t, h.baseURL, "cloudformation", "delete-stack", "--stack-name", "exec-stack")

	deletedQueueCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "sqs", "get-queue-url", "--queue-name", "cfn-job-queue")
	deletedQueueCmd.Env = awsEnv()
	deletedQueueOut, err := deletedQueueCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected cfn-created queue to be deleted")
	}
	if !strings.Contains(string(deletedQueueOut), "NonExistentQueue") {
		t.Fatalf("expected deleted queue lookup to fail: %s", deletedQueueOut)
	}

	deletedRoleCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "iam", "get-role", "--role-name", "cfn-exec-role")
	deletedRoleCmd.Env = awsEnv()
	deletedRoleOut, err := deletedRoleCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected cfn-created role to be deleted")
	}
	if !strings.Contains(string(deletedRoleOut), "NoSuchEntity") {
		t.Fatalf("expected deleted role lookup to fail: %s", deletedRoleOut)
	}

	tableListAfterDelete := runAWS(t, h.baseURL, "dynamodb", "list-tables", "--output", "json")
	if strings.Contains(string(tableListAfterDelete), `"cfn-books"`) {
		t.Fatalf("expected cfn-created table to be deleted: %s", tableListAfterDelete)
	}

	deletedLogCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "logs", "create-log-stream", "--log-group-name", "/stratus/cfn/exec", "--log-stream-name", "after-delete")
	deletedLogCmd.Env = awsEnv()
	deletedLogOut, err := deletedLogCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected cfn-created log group to be deleted")
	}
	if !strings.Contains(string(deletedLogOut), "ResourceNotFoundException") {
		t.Fatalf("expected deleted log group lookup to fail: %s", deletedLogOut)
	}
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	return startHarnessWithDataDir(t, t.TempDir())
}

func startHarnessWithDataDir(t *testing.T, dataDir string) *harness {
	t.Helper()

	if _, err := exec.LookPath("aws"); err != nil {
		t.Skip("aws cli not installed")
	}

	bin := buildBinary(t)
	for attempt := 0; attempt < 5; attempt++ {
		port := reservePort(t)
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
		output := &bytes.Buffer{}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		cmd := exec.CommandContext(ctx, bin, "--port", fmt.Sprintf("%d", port), "--data-dir", dataDir, "--log-level", "debug")
		cmd.Dir = moduleRoot(t)
		cmd.Env = serverEnv()
		cmd.Stdout = output
		cmd.Stderr = output

		if err := cmd.Start(); err != nil {
			t.Fatalf("start stratus: %v", err)
		}

		if err := waitForHealthy(baseURL); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if strings.Contains(output.String(), "address already in use") {
				continue
			}
			t.Fatalf("server did not become healthy: %v\n%s", err, output.String())
		}

		return &harness{
			t:       t,
			cmd:     cmd,
			port:    port,
			baseURL: baseURL,
			dataDir: dataDir,
			output:  output,
		}
	}

	t.Fatal("failed to start stratus after repeated port conflicts")
	return nil
}

func (h *harness) Close() {
	h.t.Helper()

	if h.cmd == nil || h.cmd.Process == nil {
		return
	}

	_ = h.cmd.Process.Signal(os.Interrupt)

	done := make(chan error, 1)
	go func() {
		done <- h.cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "signal: interrupt") {
			h.t.Fatalf("wait for process: %v\n%s", err, h.output.String())
		}
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		h.t.Fatalf("timed out waiting for process shutdown\n%s", h.output.String())
	}

	h.cmd = nil
}

func runAWS(t *testing.T, endpoint string, args ...string) []byte {
	t.Helper()

	cmdArgs := append([]string{"--endpoint-url", endpoint}, args...)
	cmd := exec.Command("aws", cmdArgs...)
	cmd.Env = awsEnv()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aws %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func awsEnv() []string {
	return append(os.Environ(),
		"AWS_ACCESS_KEY_ID=test",
		"AWS_SECRET_ACCESS_KEY=test",
		"AWS_DEFAULT_REGION=us-east-1",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_PAGER=",
	)
}

func serverEnv() []string {
	env := os.Environ()
	if os.Getenv("DOCKER_HOST") != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return env
	}
	socketPath := filepath.Join(home, ".docker", "run", "docker.sock")
	if _, err := os.Stat(socketPath); err == nil {
		return append(env, "DOCKER_HOST=unix://"+socketPath)
	}
	return env
}

func writeZip(path string, files map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := io.WriteString(w, content); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

func buildBinary(t *testing.T) string {
	t.Helper()

	buildOnce.Do(func() {
		buildDir, err := os.MkdirTemp("", "stratus-contract-build-*")
		if err != nil {
			buildErr = fmt.Errorf("create build dir: %w", err)
			return
		}
		out := filepath.Join(buildDir, "stratus-testbin")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/stratus")
		cmd.Dir = moduleRoot(t)
		cmd.Env = os.Environ()
		output, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, output)
			return
		}
		buildPath = out
	})

	if buildErr != nil {
		t.Fatal(buildErr)
	}

	return buildPath
}

func waitForHealthy(baseURL string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/_stratus/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for health check")
}

func reservePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()

	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", l.Addr())
	}
	return addr.Port
}

func moduleRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve current file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}
