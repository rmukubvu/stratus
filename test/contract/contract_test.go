package contract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
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

func dockerReachableEndpoint(endpoint string) string {
	endpoint = strings.Replace(endpoint, "http://127.0.0.1", "http://host.docker.internal", 1)
	endpoint = strings.Replace(endpoint, "http://localhost", "http://host.docker.internal", 1)
	return endpoint
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

func TestJavaSDKSmoke(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runJavaSDKSmoke(t, h.baseURL, h.output)
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

func TestAWSCLIS3MultipartCopyHeadAndPresign(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	bucket := "depth-s3-bucket"
	runAWS(t, h.baseURL, "s3api", "create-bucket", "--bucket", bucket)

	createUploadOut := runAWS(t, h.baseURL, "s3api", "create-multipart-upload", "--bucket", bucket, "--key", "multi.txt", "--output", "json")
	var upload struct {
		UploadID string `json:"UploadId"`
	}
	if err := json.Unmarshal(createUploadOut, &upload); err != nil {
		t.Fatalf("decode create-multipart-upload output: %v\n%s", err, createUploadOut)
	}
	if upload.UploadID == "" {
		t.Fatalf("expected upload id in create-multipart-upload output: %s", createUploadOut)
	}

	part1Path := filepath.Join(t.TempDir(), "part1.txt")
	part2Path := filepath.Join(t.TempDir(), "part2.txt")
	if err := os.WriteFile(part1Path, []byte("hello "), 0o644); err != nil {
		t.Fatalf("write part1: %v", err)
	}
	if err := os.WriteFile(part2Path, []byte("multipart"), 0o644); err != nil {
		t.Fatalf("write part2: %v", err)
	}

	uploadPart1Out := runAWS(t, h.baseURL, "s3api", "upload-part", "--bucket", bucket, "--key", "multi.txt", "--part-number", "1", "--upload-id", upload.UploadID, "--body", part1Path, "--output", "json")
	uploadPart2Out := runAWS(t, h.baseURL, "s3api", "upload-part", "--bucket", bucket, "--key", "multi.txt", "--part-number", "2", "--upload-id", upload.UploadID, "--body", part2Path, "--output", "json")
	var part1 struct {
		ETag string `json:"ETag"`
	}
	var part2 struct {
		ETag string `json:"ETag"`
	}
	if err := json.Unmarshal(uploadPart1Out, &part1); err != nil {
		t.Fatalf("decode upload-part 1 output: %v\n%s", err, uploadPart1Out)
	}
	if err := json.Unmarshal(uploadPart2Out, &part2); err != nil {
		t.Fatalf("decode upload-part 2 output: %v\n%s", err, uploadPart2Out)
	}

	partsPath := filepath.Join(t.TempDir(), "parts.json")
	partsBody := fmt.Sprintf(`{"Parts":[{"ETag":%q,"PartNumber":1},{"ETag":%q,"PartNumber":2}]}`, part1.ETag, part2.ETag)
	if err := os.WriteFile(partsPath, []byte(partsBody), 0o644); err != nil {
		t.Fatalf("write multipart payload: %v", err)
	}
	completeOut := runAWS(t, h.baseURL, "s3api", "complete-multipart-upload", "--bucket", bucket, "--key", "multi.txt", "--upload-id", upload.UploadID, "--multipart-upload", "file://"+partsPath, "--output", "json")
	if !strings.Contains(string(completeOut), `"ETag"`) {
		t.Fatalf("expected ETag in complete-multipart-upload output: %s", completeOut)
	}

	headOut := runAWS(t, h.baseURL, "s3api", "head-object", "--bucket", bucket, "--key", "multi.txt", "--output", "json")
	if !strings.Contains(string(headOut), `"ContentLength": 15`) && !strings.Contains(string(headOut), `"ContentLength":15`) {
		t.Fatalf("expected content length in head-object output: %s", headOut)
	}

	copyOut := runAWS(t, h.baseURL, "s3api", "copy-object", "--bucket", bucket, "--key", "copied.txt", "--copy-source", bucket+"/multi.txt", "--output", "json")
	if !strings.Contains(string(copyOut), `"CopyObjectResult"`) {
		t.Fatalf("expected copy-object result: %s", copyOut)
	}

	presignCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "s3", "presign", "s3://"+bucket+"/copied.txt", "--expires-in", "60")
	presignCmd.Env = awsEnv()
	presignedURL, err := presignCmd.Output()
	if err != nil {
		t.Fatalf("presign object: %v", err)
	}
	resp, err := http.Get(strings.TrimSpace(string(presignedURL)))
	if err != nil {
		t.Fatalf("GET presigned url: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello multipart" {
		t.Fatalf("unexpected presigned GET response %d: %s", resp.StatusCode, body)
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
	if !strings.Contains(string(badOut), "ResourceNotFoundException") || !strings.Contains(string(badOut), "Layer version not found") {
		t.Fatalf("expected explicit missing-layer error, got: %s", badOut)
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

func TestAWSCLILambdaAsyncInvokeAndVersionsAliases(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	functionName := "lambda-depth"
	zipPath := filepath.Join(t.TempDir(), "depth.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "counter = 0\n\ndef main(event, context):\n    global counter\n    counter += 1\n    return {'count': counter}\n",
	}); err != nil {
		t.Fatalf("write lambda zip: %v", err)
	}

	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", functionName,
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
	)

	asyncPayloadPath := filepath.Join(t.TempDir(), "async.json")
	asyncOut := runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", functionName,
		"--invocation-type", "Event",
		"--payload", "{}",
		asyncPayloadPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	if !strings.Contains(string(asyncOut), `"StatusCode": 202`) && !strings.Contains(string(asyncOut), `"StatusCode":202`) {
		t.Fatalf("expected async invoke status 202: %s", asyncOut)
	}

	time.Sleep(600 * time.Millisecond)

	syncPayloadPath := filepath.Join(t.TempDir(), "sync.json")
	runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", functionName,
		"--payload", "{}",
		syncPayloadPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	syncBody, err := os.ReadFile(syncPayloadPath)
	if err != nil {
		t.Fatalf("read sync payload: %v", err)
	}
	if !strings.Contains(string(syncBody), `"count": 2`) && !strings.Contains(string(syncBody), `"count":2`) {
		t.Fatalf("expected async invoke to advance warm state: %s", syncBody)
	}

	version1Out := runAWS(t, h.baseURL, "lambda", "publish-version", "--function-name", functionName, "--output", "json")
	version2Out := runAWS(t, h.baseURL, "lambda", "publish-version", "--function-name", functionName, "--output", "json")
	if !strings.Contains(string(version1Out), `"Version": "1"`) && !strings.Contains(string(version1Out), `"Version":"1"`) {
		t.Fatalf("expected published version 1: %s", version1Out)
	}
	if !strings.Contains(string(version2Out), `"Version": "2"`) && !strings.Contains(string(version2Out), `"Version":"2"`) {
		t.Fatalf("expected published version 2: %s", version2Out)
	}

	runAWS(t, h.baseURL, "lambda", "create-alias", "--function-name", functionName, "--name", "live", "--function-version", "1", "--output", "json")
	aliasOut := runAWS(t, h.baseURL, "lambda", "get-alias", "--function-name", functionName, "--name", "live", "--output", "json")
	if !strings.Contains(string(aliasOut), `"FunctionVersion": "1"`) && !strings.Contains(string(aliasOut), `"FunctionVersion":"1"`) {
		t.Fatalf("expected alias version 1: %s", aliasOut)
	}
	runAWS(t, h.baseURL, "lambda", "update-alias", "--function-name", functionName, "--name", "live", "--function-version", "2", "--output", "json")

	versionsOut := runAWS(t, h.baseURL, "lambda", "list-versions-by-function", "--function-name", functionName, "--output", "json")
	if !strings.Contains(string(versionsOut), `"Version": "2"`) && !strings.Contains(string(versionsOut), `"Version":"2"`) {
		t.Fatalf("expected version 2 in list-versions-by-function output: %s", versionsOut)
	}
	aliasesOut := runAWS(t, h.baseURL, "lambda", "list-aliases", "--function-name", functionName, "--output", "json")
	if !strings.Contains(string(aliasesOut), `"live"`) {
		t.Fatalf("expected alias in list-aliases output: %s", aliasesOut)
	}

	getQualifiedOut := runAWS(t, h.baseURL, "lambda", "get-function", "--function-name", functionName, "--qualifier", "live", "--output", "json")
	if !strings.Contains(string(getQualifiedOut), `:2"`) {
		t.Fatalf("expected qualifier-resolved version in get-function output: %s", getQualifiedOut)
	}
}

func TestAWSCLILambdaLayersAndEventSourceMappings(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	layerZip := filepath.Join(t.TempDir(), "layer.zip")
	if err := writeZip(layerZip, map[string]string{
		"python/shared.py": "def message():\n    return 'from-layer'\n",
	}); err != nil {
		t.Fatalf("write layer zip: %v", err)
	}
	layerOut := runAWS(t, h.baseURL,
		"lambda", "publish-layer-version",
		"--layer-name", "shared-lib",
		"--zip-file", "fileb://"+layerZip,
		"--compatible-runtimes", "python3.11",
		"--output", "json",
	)
	var layer struct {
		LayerVersionArn string `json:"LayerVersionArn"`
	}
	if err := json.Unmarshal(layerOut, &layer); err != nil {
		t.Fatalf("decode publish-layer-version output: %v\n%s", err, layerOut)
	}
	if layer.LayerVersionArn == "" {
		t.Fatalf("expected layer version arn: %s", layerOut)
	}

	layerFuncZip := filepath.Join(t.TempDir(), "layer-func.zip")
	if err := writeZip(layerFuncZip, map[string]string{
		"handler.py": "from shared import message\n\ndef main(event, context):\n    return {'message': message()}\n",
	}); err != nil {
		t.Fatalf("write layer function zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "layered-func",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+layerFuncZip,
		"--layers", layer.LayerVersionArn,
	)
	layerInvokePath := filepath.Join(t.TempDir(), "layered.json")
	_ = runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", "layered-func",
		"--payload", "{}",
		layerInvokePath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	layerBody, err := os.ReadFile(layerInvokePath)
	if err != nil {
		t.Fatalf("read layered invoke payload: %v", err)
	}
	if !strings.Contains(string(layerBody), `"from-layer"`) {
		t.Fatalf("expected function to load layer module: %s", layerBody)
	}

	queueURLRaw := runAWS(t, h.baseURL, "sqs", "create-queue", "--queue-name", "mapped-queue", "--output", "json")
	var queue struct {
		QueueURL string `json:"QueueUrl"`
	}
	if err := json.Unmarshal(queueURLRaw, &queue); err != nil {
		t.Fatalf("decode create-queue output: %v\n%s", err, queueURLRaw)
	}
	queueAttrs := runAWS(t, h.baseURL, "sqs", "get-queue-attributes", "--queue-url", queue.QueueURL, "--attribute-names", "QueueArn", "--output", "json")
	var queueAttrPayload struct {
		Attributes map[string]string `json:"Attributes"`
	}
	if err := json.Unmarshal(queueAttrs, &queueAttrPayload); err != nil {
		t.Fatalf("decode queue attrs: %v\n%s", err, queueAttrs)
	}

	mappedZip := filepath.Join(t.TempDir(), "mapped.zip")
	if err := writeZip(mappedZip, map[string]string{
		"handler.py": "events = []\n\ndef main(event, context):\n    global events\n    if isinstance(event, dict) and event.get('mode') == 'snapshot':\n        return {'events': events}\n    events.append(event)\n    return {'ok': True}\n",
	}); err != nil {
		t.Fatalf("write mapped zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "mapped-func",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+mappedZip,
	)

	mappingOut := runAWS(t, h.baseURL,
		"lambda", "create-event-source-mapping",
		"--function-name", "mapped-func",
		"--event-source-arn", queueAttrPayload.Attributes["QueueArn"],
		"--batch-size", "1",
		"--output", "json",
	)
	var mapping struct {
		UUID string `json:"UUID"`
	}
	if err := json.Unmarshal(mappingOut, &mapping); err != nil {
		t.Fatalf("decode create-event-source-mapping output: %v\n%s", err, mappingOut)
	}
	if mapping.UUID == "" {
		t.Fatalf("expected event source mapping uuid: %s", mappingOut)
	}

	listMappingsOut := runAWS(t, h.baseURL, "lambda", "list-event-source-mappings", "--function-name", "mapped-func", "--output", "json")
	if !strings.Contains(string(listMappingsOut), mapping.UUID) {
		t.Fatalf("expected mapping in list-event-source-mappings output: %s", listMappingsOut)
	}
	getMappingOut := runAWS(t, h.baseURL, "lambda", "get-event-source-mapping", "--uuid", mapping.UUID, "--output", "json")
	if !strings.Contains(string(getMappingOut), queueAttrPayload.Attributes["QueueArn"]) {
		t.Fatalf("expected queue arn in get-event-source-mapping output: %s", getMappingOut)
	}

	runAWS(t, h.baseURL, "sqs", "send-message", "--queue-url", queue.QueueURL, "--message-body", "hello mapping")
	time.Sleep(750 * time.Millisecond)

	snapshotPath := filepath.Join(t.TempDir(), "mapping-snapshot.json")
	_ = runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", "mapped-func",
		"--payload", `{"mode":"snapshot"}`,
		snapshotPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	snapshotBody, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read mapping snapshot: %v", err)
	}
	if !strings.Contains(string(snapshotBody), `"aws:sqs"`) || !strings.Contains(string(snapshotBody), `"hello mapping"`) {
		t.Fatalf("expected sqs event source delivery in snapshot: %s", snapshotBody)
	}

	runAWS(t, h.baseURL, "lambda", "delete-event-source-mapping", "--uuid", mapping.UUID, "--output", "json")
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

	updateOut := runAWS(t, h.baseURL,
		"dynamodb", "update-item",
		"--table-name", "books",
		"--key", `{"id":{"S":"1"}}`,
		"--update-expression", "SET #t = :t, author = :a",
		"--expression-attribute-names", `{"#t":"title"}`,
		"--expression-attribute-values", `{":t":{"S":"Dune Messiah"},":a":{"S":"Frank Herbert"}}`,
		"--return-values", "ALL_NEW",
		"--output", "json",
	)
	if !strings.Contains(string(updateOut), `"Dune Messiah"`) || !strings.Contains(string(updateOut), `"Frank Herbert"`) {
		t.Fatalf("unexpected update-item output: %s", updateOut)
	}

	queryOut := runAWS(t, h.baseURL,
		"dynamodb", "query",
		"--table-name", "books",
		"--key-condition-expression", "id = :id",
		"--expression-attribute-values", `{":id":{"S":"1"}}`,
		"--output", "json",
	)
	if !strings.Contains(string(queryOut), `"Dune Messiah"`) {
		t.Fatalf("unexpected query output: %s", queryOut)
	}

	scanOut := runAWS(t, h.baseURL, "dynamodb", "scan", "--table-name", "books", "--output", "json")
	if !strings.Contains(string(scanOut), `"Frank Herbert"`) {
		t.Fatalf("unexpected scan output: %s", scanOut)
	}

	runAWS(t, h.baseURL,
		"dynamodb", "batch-write-item",
		"--request-items", `{"books":[{"PutRequest":{"Item":{"id":{"S":"2"},"title":{"S":"Children of Dune"}}}},{"DeleteRequest":{"Key":{"id":{"S":"1"}}}}]}`,
		"--output", "json",
	)

	batchGetOut := runAWS(t, h.baseURL,
		"dynamodb", "batch-get-item",
		"--request-items", `{"books":{"Keys":[{"id":{"S":"2"}}]}}`,
		"--output", "json",
	)
	if !strings.Contains(string(batchGetOut), `"Children of Dune"`) {
		t.Fatalf("unexpected batch-get-item output: %s", batchGetOut)
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

func TestAWSCLIKMS(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	createOut := runAWS(t, h.baseURL, "kms", "create-key", "--description", "contract key", "--output", "json")
	var created struct {
		KeyMetadata struct {
			Arn   string `json:"Arn"`
			KeyID string `json:"KeyId"`
		} `json:"KeyMetadata"`
	}
	if err := json.Unmarshal(createOut, &created); err != nil {
		t.Fatalf("decode create-key output: %v\n%s", err, createOut)
	}
	if created.KeyMetadata.KeyID == "" || created.KeyMetadata.Arn == "" {
		t.Fatalf("expected key metadata in create-key output: %s", createOut)
	}

	listOut := runAWS(t, h.baseURL, "kms", "list-keys", "--output", "json")
	if !strings.Contains(string(listOut), created.KeyMetadata.KeyID) {
		t.Fatalf("expected key in list-keys output: %s", listOut)
	}

	describeOut := runAWS(t, h.baseURL, "kms", "describe-key", "--key-id", created.KeyMetadata.KeyID, "--output", "json")
	if !strings.Contains(string(describeOut), `"Description": "contract key"`) && !strings.Contains(string(describeOut), `"Description":"contract key"`) {
		t.Fatalf("expected description in describe-key output: %s", describeOut)
	}

	runAWS(t, h.baseURL, "kms", "create-alias", "--alias-name", "alias/contract-key", "--target-key-id", created.KeyMetadata.KeyID)

	aliasesOut := runAWS(t, h.baseURL, "kms", "list-aliases", "--output", "json")
	if !strings.Contains(string(aliasesOut), `"alias/contract-key"`) {
		t.Fatalf("expected alias in list-aliases output: %s", aliasesOut)
	}

	plaintextPath := filepath.Join(t.TempDir(), "plaintext.txt")
	if err := os.WriteFile(plaintextPath, []byte("hello kms"), 0o644); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}
	encryptOut := runAWS(t, h.baseURL, "kms", "encrypt", "--key-id", "alias/contract-key", "--plaintext", "fileb://"+plaintextPath, "--output", "json")
	var encrypted struct {
		CiphertextBlob string `json:"CiphertextBlob"`
	}
	if err := json.Unmarshal(encryptOut, &encrypted); err != nil {
		t.Fatalf("decode encrypt output: %v\n%s", err, encryptOut)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted.CiphertextBlob)
	if err != nil {
		t.Fatalf("decode ciphertext blob: %v", err)
	}
	ciphertextPath := filepath.Join(t.TempDir(), "ciphertext.bin")
	if err := os.WriteFile(ciphertextPath, ciphertext, 0o644); err != nil {
		t.Fatalf("write ciphertext: %v", err)
	}

	decryptOut := runAWS(t, h.baseURL, "kms", "decrypt", "--ciphertext-blob", "fileb://"+ciphertextPath, "--output", "json")
	var decrypted struct {
		Plaintext string `json:"Plaintext"`
	}
	if err := json.Unmarshal(decryptOut, &decrypted); err != nil {
		t.Fatalf("decode decrypt output: %v\n%s", err, decryptOut)
	}
	decodedPlaintext, err := base64.StdEncoding.DecodeString(decrypted.Plaintext)
	if err != nil {
		t.Fatalf("decode decrypt plaintext: %v", err)
	}
	if string(decodedPlaintext) != "hello kms" {
		t.Fatalf("unexpected decrypt plaintext: %q", decodedPlaintext)
	}
}

func TestAWSCLIAPIGatewayV2HTTPAPI(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	functionName := "http-api-function"
	zipPath := filepath.Join(t.TempDir(), "http-api.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "def main(event, context):\n    return {'statusCode': 200, 'headers': {'content-type': 'text/plain'}, 'body': 'hello from api'}\n",
	}); err != nil {
		t.Fatalf("write lambda zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", functionName,
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/http-api-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
		"--output", "json",
	)

	createAPIOut := runAWS(t, h.baseURL, "apigatewayv2", "create-api", "--name", "contract-http-api", "--protocol-type", "HTTP", "--output", "json")
	var apiPayload struct {
		APIEndpoint string `json:"ApiEndpoint"`
		APIID       string `json:"ApiId"`
	}
	if err := json.Unmarshal(createAPIOut, &apiPayload); err != nil {
		t.Fatalf("decode create-api output: %v\n%s", err, createAPIOut)
	}
	if apiPayload.APIID == "" || apiPayload.APIEndpoint == "" {
		t.Fatalf("expected api metadata in create-api output: %s", createAPIOut)
	}

	uri := "arn:aws:lambda:us-east-1:000000000000:function:" + functionName
	createIntegrationOut := runAWS(t, h.baseURL,
		"apigatewayv2", "create-integration",
		"--api-id", apiPayload.APIID,
		"--integration-type", "AWS_PROXY",
		"--integration-uri", uri,
		"--payload-format-version", "2.0",
		"--output", "json",
	)
	var integrationPayload struct {
		IntegrationID string `json:"IntegrationId"`
	}
	if err := json.Unmarshal(createIntegrationOut, &integrationPayload); err != nil {
		t.Fatalf("decode create-integration output: %v\n%s", err, createIntegrationOut)
	}
	if integrationPayload.IntegrationID == "" {
		t.Fatalf("expected integration id in output: %s", createIntegrationOut)
	}

	runAWS(t, h.baseURL,
		"apigatewayv2", "create-route",
		"--api-id", apiPayload.APIID,
		"--route-key", "GET /hello",
		"--target", "integrations/"+integrationPayload.IntegrationID,
		"--output", "json",
	)
	runAWS(t, h.baseURL,
		"apigatewayv2", "create-stage",
		"--api-id", apiPayload.APIID,
		"--stage-name", "$default",
		"--auto-deploy",
		"--output", "json",
	)

	getAPIOut := runAWS(t, h.baseURL, "apigatewayv2", "get-api", "--api-id", apiPayload.APIID, "--output", "json")
	if !strings.Contains(string(getAPIOut), `"contract-http-api"`) {
		t.Fatalf("expected api name in get-api output: %s", getAPIOut)
	}
	getAPIsOut := runAWS(t, h.baseURL, "apigatewayv2", "get-apis", "--output", "json")
	if !strings.Contains(string(getAPIsOut), apiPayload.APIID) {
		t.Fatalf("expected api id in get-apis output: %s", getAPIsOut)
	}

	resp, err := http.Get(apiPayload.APIEndpoint + "/hello")
	if err != nil {
		t.Fatalf("http api invoke: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected api invoke status %d: %s", resp.StatusCode, body)
	}
	if string(body) != "hello from api" {
		t.Fatalf("unexpected api invoke body: %s", body)
	}
}

func TestAWSCLINodeLambdaHTTPAPIToDynamoDB(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runAWS(t, h.baseURL,
		"dynamodb", "create-table",
		"--table-name", "http-items",
		"--attribute-definitions", "AttributeName=id,AttributeType=S",
		"--key-schema", "AttributeName=id,KeyType=HASH",
		"--billing-mode", "PAY_PER_REQUEST",
		"--output", "json",
	)

	zipPath := filepath.Join(t.TempDir(), "node-http-ddb.zip")
	if err := writeZip(zipPath, map[string]string{
		"index.js": `
exports.handler = async (event) => {
  const payload = JSON.parse(event.body || "{}");
  const response = await fetch(process.env.STRATUS_ENDPOINT, {
    method: "POST",
    headers: {
      "content-type": "application/x-amz-json-1.0",
      "x-amz-target": "DynamoDB_20120810.PutItem"
    },
    body: JSON.stringify({
      TableName: process.env.TABLE_NAME,
      Item: {
        id: { S: payload.id },
        status: { S: "stored" }
      }
    })
  });
  if (!response.ok) {
    throw new Error(await response.text());
  }
  return {
    statusCode: 202,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ id: payload.id, status: "stored" })
  };
};
`,
	}); err != nil {
		t.Fatalf("write node lambda zip: %v", err)
	}

	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "node-http-ddb",
		"--runtime", "nodejs20.x",
		"--role", "arn:aws:iam::000000000000:role/node-http-ddb-role",
		"--handler", "index.handler",
		"--zip-file", "fileb://"+zipPath,
		"--environment", fmt.Sprintf("Variables={TABLE_NAME=http-items,STRATUS_ENDPOINT=%s}", dockerReachableEndpoint(h.baseURL)),
		"--output", "json",
	)

	createAPIOut := runAWS(t, h.baseURL, "apigatewayv2", "create-api", "--name", "node-http-api", "--protocol-type", "HTTP", "--output", "json")
	var apiPayload struct {
		APIEndpoint string `json:"ApiEndpoint"`
		APIID       string `json:"ApiId"`
	}
	if err := json.Unmarshal(createAPIOut, &apiPayload); err != nil {
		t.Fatalf("decode create-api output: %v\n%s", err, createAPIOut)
	}

	uri := "arn:aws:lambda:us-east-1:000000000000:function:node-http-ddb"
	createIntegrationOut := runAWS(t, h.baseURL,
		"apigatewayv2", "create-integration",
		"--api-id", apiPayload.APIID,
		"--integration-type", "AWS_PROXY",
		"--integration-uri", uri,
		"--payload-format-version", "2.0",
		"--output", "json",
	)
	var integrationPayload struct {
		IntegrationID string `json:"IntegrationId"`
	}
	if err := json.Unmarshal(createIntegrationOut, &integrationPayload); err != nil {
		t.Fatalf("decode create-integration output: %v\n%s", err, createIntegrationOut)
	}

	runAWS(t, h.baseURL,
		"apigatewayv2", "create-route",
		"--api-id", apiPayload.APIID,
		"--route-key", "POST /items",
		"--target", "integrations/"+integrationPayload.IntegrationID,
		"--output", "json",
	)
	runAWS(t, h.baseURL,
		"apigatewayv2", "create-stage",
		"--api-id", apiPayload.APIID,
		"--stage-name", "$default",
		"--auto-deploy",
		"--output", "json",
	)

	req, err := http.NewRequest(http.MethodPost, apiPayload.APIEndpoint+"/items", strings.NewReader(`{"id":"item-123"}`))
	if err != nil {
		t.Fatalf("new http api request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("invoke node http api: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("unexpected node api status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"item-123"`) {
		t.Fatalf("unexpected node api body: %s", body)
	}

	getOut := runAWS(t, h.baseURL,
		"dynamodb", "get-item",
		"--table-name", "http-items",
		"--key", `{"id":{"S":"item-123"}}`,
		"--output", "json",
	)
	if !strings.Contains(string(getOut), `"stored"`) {
		t.Fatalf("expected item written by node lambda, got: %s", getOut)
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
		"sqs", "change-message-visibility",
		"--queue-url", urlPayload.QueueURL,
		"--receipt-handle", receivePayload.Messages[0].ReceiptHandle,
		"--visibility-timeout", "0",
	)

	reappearedOut := runAWS(t, h.baseURL,
		"sqs", "receive-message",
		"--queue-url", urlPayload.QueueURL,
		"--attribute-names", "All",
		"--max-number-of-messages", "1",
		"--output", "json",
	)
	var reappeared struct {
		Messages []struct {
			ReceiptHandle string `json:"ReceiptHandle"`
		} `json:"Messages"`
	}
	if err := json.Unmarshal(reappearedOut, &reappeared); err != nil {
		t.Fatalf("decode visibility receive output: %v\n%s", err, reappearedOut)
	}
	if len(reappeared.Messages) != 1 || reappeared.Messages[0].ReceiptHandle == "" {
		t.Fatalf("expected message to reappear after change-message-visibility: %s", reappearedOut)
	}

	runAWS(t, h.baseURL,
		"sqs", "delete-message",
		"--queue-url", urlPayload.QueueURL,
		"--receipt-handle", reappeared.Messages[0].ReceiptHandle,
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

func TestAWSCLISQSDepthDLQAttributesAndLongPolling(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runAWS(t, h.baseURL, "sqs", "create-queue", "--queue-name", "jobs-dlq")
	dlqURLRaw := runAWS(t, h.baseURL, "sqs", "get-queue-url", "--queue-name", "jobs-dlq", "--output", "json")
	var dlqURL struct {
		QueueURL string `json:"QueueUrl"`
	}
	if err := json.Unmarshal(dlqURLRaw, &dlqURL); err != nil {
		t.Fatalf("decode dlq queue url: %v\n%s", err, dlqURLRaw)
	}
	dlqAttrs := runAWS(t, h.baseURL, "sqs", "get-queue-attributes", "--queue-url", dlqURL.QueueURL, "--attribute-names", "QueueArn", "--output", "json")
	var attrs struct {
		Attributes map[string]string `json:"Attributes"`
	}
	if err := json.Unmarshal(dlqAttrs, &attrs); err != nil {
		t.Fatalf("decode dlq attributes: %v\n%s", err, dlqAttrs)
	}

	redrive := fmt.Sprintf(`{"deadLetterTargetArn":"%s","maxReceiveCount":"2"}`, attrs.Attributes["QueueArn"])
	runAWS(t, h.baseURL,
		"sqs", "create-queue",
		"--queue-name", "jobs-depth",
		"--attributes", fmt.Sprintf(`{"ReceiveMessageWaitTimeSeconds":"1","RedrivePolicy":%q}`, redrive),
		"--output", "json",
	)
	queueURLRaw := runAWS(t, h.baseURL, "sqs", "get-queue-url", "--queue-name", "jobs-depth", "--output", "json")
	var queueURL struct {
		QueueURL string `json:"QueueUrl"`
	}
	if err := json.Unmarshal(queueURLRaw, &queueURL); err != nil {
		t.Fatalf("decode queue url: %v\n%s", err, queueURLRaw)
	}

	runAWS(t, h.baseURL, "sqs", "set-queue-attributes", "--queue-url", queueURL.QueueURL, "--attributes", "VisibilityTimeout=0")
	queueAttrs := runAWS(t, h.baseURL, "sqs", "get-queue-attributes", "--queue-url", queueURL.QueueURL, "--attribute-names", "All", "--output", "json")
	if !strings.Contains(string(queueAttrs), `"ReceiveMessageWaitTimeSeconds": "1"`) && !strings.Contains(string(queueAttrs), `"ReceiveMessageWaitTimeSeconds":"1"`) {
		t.Fatalf("expected receive wait attribute in queue attrs output: %s", queueAttrs)
	}
	if !strings.Contains(string(queueAttrs), `"VisibilityTimeout": "0"`) && !strings.Contains(string(queueAttrs), `"VisibilityTimeout":"0"`) {
		t.Fatalf("expected updated visibility timeout in queue attrs output: %s", queueAttrs)
	}

	runAWS(t, h.baseURL, "sqs", "send-message", "--queue-url", queueURL.QueueURL, "--message-body", "poison pill")
	for range 3 {
		runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queueURL.QueueURL, "--max-number-of-messages", "1", "--visibility-timeout", "0", "--output", "json")
	}

	sourceEmpty := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queueURL.QueueURL, "--max-number-of-messages", "1", "--wait-time-seconds", "0", "--output", "json")
	if strings.Contains(string(sourceEmpty), `"Body"`) {
		t.Fatalf("expected source queue to be empty after redrive: %s", sourceEmpty)
	}

	dlqReceive := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", dlqURL.QueueURL, "--max-number-of-messages", "1", "--output", "json")
	if !strings.Contains(string(dlqReceive), `"poison pill"`) {
		t.Fatalf("expected message in DLQ: %s", dlqReceive)
	}

	start := time.Now()
	_ = runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queueURL.QueueURL, "--max-number-of-messages", "1", "--output", "json")
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("expected long-poll receive-message to wait for queue default")
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

func TestAWSCLICloudFormationExecutesLambdaAndHTTPAPI(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	templatePath := filepath.Join(t.TempDir(), "http-api-template.json")
	templateBody := `{
  "AWSTemplateFormatVersion": "2010-09-09",
  "Resources": {
    "InlineRole": {
      "Type": "AWS::IAM::Role",
      "Properties": {
        "RoleName": "cfn-http-role",
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
        }
      }
    },
    "InlineFunction": {
      "Type": "AWS::Lambda::Function",
      "Properties": {
        "FunctionName": "cfn-http-inline",
        "Handler": "index.main",
        "Runtime": "python3.11",
        "Role": {
          "Fn::GetAtt": [
            "InlineRole",
            "Arn"
          ]
        },
        "Code": {
          "ZipFile": "def main(event, context):\n    return {'statusCode': 200, 'headers': {'content-type': 'text/plain'}, 'body': 'hello from cfn'}\n"
        },
        "Timeout": 10
      }
    },
    "HttpApi": {
      "Type": "AWS::ApiGatewayV2::Api",
      "Properties": {
        "Name": "cfn-http-api",
        "ProtocolType": "HTTP"
      }
    },
    "HttpIntegration": {
      "Type": "AWS::ApiGatewayV2::Integration",
      "Properties": {
        "ApiId": {
          "Ref": "HttpApi"
        },
        "IntegrationType": "AWS_PROXY",
        "IntegrationUri": {
          "Fn::GetAtt": [
            "InlineFunction",
            "Arn"
          ]
        },
        "PayloadFormatVersion": "2.0"
      }
    },
    "HelloRoute": {
      "Type": "AWS::ApiGatewayV2::Route",
      "Properties": {
        "ApiId": {
          "Ref": "HttpApi"
        },
        "RouteKey": "GET /hello",
        "Target": {
          "Fn::Join": [
            "",
            [
              "integrations/",
              {
                "Ref": "HttpIntegration"
              }
            ]
          ]
        }
      }
    },
    "DefaultStage": {
      "Type": "AWS::ApiGatewayV2::Stage",
      "Properties": {
        "ApiId": {
          "Ref": "HttpApi"
        },
        "StageName": "$default",
        "AutoDeploy": true
      }
    },
    "AllowInvoke": {
      "Type": "AWS::Lambda::Permission",
      "Properties": {
        "Action": "lambda:InvokeFunction",
        "FunctionName": {
          "Ref": "InlineFunction"
        },
        "Principal": "apigateway.amazonaws.com"
      }
    }
  }
}`
	if err := os.WriteFile(templatePath, []byte(templateBody), 0o644); err != nil {
		t.Fatalf("write http api template: %v", err)
	}

	runAWS(t, h.baseURL,
		"cloudformation", "create-stack",
		"--stack-name", "http-api-stack",
		"--template-body", "file://"+templatePath,
		"--capabilities", "CAPABILITY_NAMED_IAM",
	)

	functionOut := runAWS(t, h.baseURL, "lambda", "get-function", "--function-name", "cfn-http-inline", "--output", "json")
	if !strings.Contains(string(functionOut), `"FunctionName": "cfn-http-inline"`) && !strings.Contains(string(functionOut), `"FunctionName":"cfn-http-inline"`) {
		t.Fatalf("expected lambda function created by stack: %s", functionOut)
	}

	apisOut := runAWS(t, h.baseURL, "apigatewayv2", "get-apis", "--output", "json")
	var apiList struct {
		Items []struct {
			APIID       string `json:"ApiId"`
			APIEndpoint string `json:"ApiEndpoint"`
			Name        string `json:"Name"`
		} `json:"Items"`
	}
	if err := json.Unmarshal(apisOut, &apiList); err != nil {
		t.Fatalf("decode get-apis output: %v\n%s", err, apisOut)
	}
	apiEndpoint := ""
	apiID := ""
	for _, item := range apiList.Items {
		if item.Name == "cfn-http-api" {
			apiEndpoint = item.APIEndpoint
			apiID = item.APIID
			break
		}
	}
	if apiEndpoint == "" || apiID == "" {
		t.Fatalf("expected http api created by stack: %s", apisOut)
	}

	resp, err := http.Get(apiEndpoint + "/hello")
	if err != nil {
		t.Fatalf("invoke cfn-created http api: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected cfn http api status %d: %s", resp.StatusCode, body)
	}
	if string(body) != "hello from cfn" {
		t.Fatalf("unexpected cfn http api body: %s", body)
	}

	runAWS(t, h.baseURL, "cloudformation", "delete-stack", "--stack-name", "http-api-stack")

	deletedAPICmd := exec.Command("aws", "--endpoint-url", h.baseURL, "apigatewayv2", "get-api", "--api-id", apiID)
	deletedAPICmd.Env = awsEnv()
	deletedAPIOut, err := deletedAPICmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected cfn-created api to be deleted")
	}
	if !strings.Contains(string(deletedAPIOut), "NotFoundException") {
		t.Fatalf("expected deleted api lookup to fail: %s", deletedAPIOut)
	}

	deletedFunctionCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "lambda", "get-function", "--function-name", "cfn-http-inline")
	deletedFunctionCmd.Env = awsEnv()
	deletedFunctionOut, err := deletedFunctionCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected cfn-created lambda function to be deleted")
	}
	if !strings.Contains(string(deletedFunctionOut), "ResourceNotFoundException") {
		t.Fatalf("expected deleted lambda lookup to fail: %s", deletedFunctionOut)
	}
}

func TestAWSCLICloudFormationExecutesAdditionalControlPlaneResources(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	templatePath := filepath.Join(t.TempDir(), "control-plane-template.json")
	templateBody := `{
  "AWSTemplateFormatVersion": "2010-09-09",
  "Resources": {
    "LocalKey": {
      "Type": "AWS::KMS::Key",
      "Properties": {
        "Description": "local cfn key"
      }
    },
    "LocalAlias": {
      "Type": "AWS::KMS::Alias",
      "Properties": {
        "AliasName": "alias/local-cfn",
        "TargetKeyId": {
          "Ref": "LocalKey"
        }
      }
    },
    "LocalParam": {
      "Type": "AWS::SSM::Parameter",
      "Properties": {
        "Name": "/stratus/cfn/control",
        "Type": "String",
        "Value": "mission"
      }
    },
    "LocalTopic": {
      "Type": "AWS::SNS::Topic",
      "Properties": {
        "TopicName": "cfn-control-topic",
        "DisplayName": "CFN Control Topic"
      }
    },
    "LocalSecret": {
      "Type": "AWS::SecretsManager::Secret",
      "Properties": {
        "Name": "cfn/control/secret",
        "Description": "control secret",
        "SecretString": "{\"hello\":\"world\"}"
      }
    },
    "LocalStream": {
      "Type": "AWS::Kinesis::Stream",
      "Properties": {
        "Name": "cfn-control-stream",
        "ShardCount": 1
      }
    },
    "LocalPool": {
      "Type": "AWS::Cognito::UserPool",
      "Properties": {
        "UserPoolName": "cfn-control-pool"
      }
    },
    "LocalClient": {
      "Type": "AWS::Cognito::UserPoolClient",
      "Properties": {
        "ClientName": "cfn-control-client",
        "UserPoolId": {
          "Ref": "LocalPool"
        },
        "ExplicitAuthFlows": [
          "USER_PASSWORD_AUTH"
        ]
      }
    }
  }
}`
	if err := os.WriteFile(templatePath, []byte(templateBody), 0o644); err != nil {
		t.Fatalf("write control plane template: %v", err)
	}

	runAWS(t, h.baseURL, "cloudformation", "create-stack", "--stack-name", "control-plane-stack", "--template-body", "file://"+templatePath)

	paramOut := runAWS(t, h.baseURL, "ssm", "get-parameter", "--name", "/stratus/cfn/control", "--output", "json")
	if !strings.Contains(string(paramOut), `"mission"`) {
		t.Fatalf("expected parameter value: %s", paramOut)
	}

	topicARN := "arn:aws:sns:us-east-1:000000000000:cfn-control-topic"
	topicOut := runAWS(t, h.baseURL, "sns", "get-topic-attributes", "--topic-arn", topicARN, "--output", "json")
	if !strings.Contains(string(topicOut), `"CFN Control Topic"`) {
		t.Fatalf("expected topic display name: %s", topicOut)
	}

	secretOut := runAWS(t, h.baseURL, "secretsmanager", "get-secret-value", "--secret-id", "cfn/control/secret", "--output", "json")
	if !strings.Contains(string(secretOut), `world`) {
		t.Fatalf("expected secret payload: %s", secretOut)
	}

	streamOut := runAWS(t, h.baseURL, "kinesis", "describe-stream-summary", "--stream-name", "cfn-control-stream", "--output", "json")
	if !strings.Contains(string(streamOut), `"ACTIVE"`) {
		t.Fatalf("expected active stream: %s", streamOut)
	}

	aliasOut := runAWS(t, h.baseURL, "kms", "list-aliases", "--output", "json")
	if !strings.Contains(string(aliasOut), `"alias/local-cfn"`) {
		t.Fatalf("expected alias in list-aliases output: %s", aliasOut)
	}

	poolsOut := runAWS(t, h.baseURL, "cognito-idp", "list-user-pools", "--max-results", "10", "--output", "json")
	if !strings.Contains(string(poolsOut), `"cfn-control-pool"`) {
		t.Fatalf("expected user pool in list-user-pools output: %s", poolsOut)
	}

	runAWS(t, h.baseURL, "cloudformation", "delete-stack", "--stack-name", "control-plane-stack")

	deletedSecretCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "secretsmanager", "get-secret-value", "--secret-id", "cfn/control/secret")
	deletedSecretCmd.Env = awsEnv()
	deletedSecretOut, err := deletedSecretCmd.CombinedOutput()
	if err == nil || !strings.Contains(string(deletedSecretOut), "ResourceNotFoundException") {
		t.Fatalf("expected deleted secret lookup to fail: %s", deletedSecretOut)
	}

	deletedParamCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "ssm", "get-parameter", "--name", "/stratus/cfn/control")
	deletedParamCmd.Env = awsEnv()
	deletedParamOut, err := deletedParamCmd.CombinedOutput()
	if err == nil || !strings.Contains(string(deletedParamOut), "ParameterNotFound") {
		t.Fatalf("expected deleted parameter lookup to fail: %s", deletedParamOut)
	}
}

func TestAWSCLICloudFormationExecutesEventRuleTargets(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	templatePath := filepath.Join(t.TempDir(), "events-template.json")
	templateBody := `{
  "AWSTemplateFormatVersion": "2010-09-09",
  "Resources": {
    "EventRole": {
      "Type": "AWS::IAM::Role",
      "Properties": {
        "RoleName": "cfn-events-role",
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
        }
      }
    },
    "EventFunction": {
      "Type": "AWS::Lambda::Function",
      "Properties": {
        "FunctionName": "cfn-events-target",
        "Handler": "index.main",
        "Runtime": "python3.11",
        "Role": {
          "Fn::GetAtt": [
            "EventRole",
            "Arn"
          ]
        },
        "Code": {
          "ZipFile": "deliveries = []\n\ndef main(event, context):\n    global deliveries\n    if isinstance(event, dict) and event.get('mode') == 'snapshot':\n        return {'deliveries': deliveries}\n    deliveries.append(event)\n    return {'accepted': True}\n"
        }
      }
    },
    "EventBus": {
      "Type": "AWS::Events::EventBus",
      "Properties": {
        "Name": "cfn-event-bus"
      }
    },
    "EventRule": {
      "Type": "AWS::Events::Rule",
      "Properties": {
        "Name": "cfn-event-rule",
        "EventBusName": {
          "Ref": "EventBus"
        },
        "EventPattern": {
          "source": [
            "stratus.cfn"
          ],
          "detail": {
            "kind": [
              "demo"
            ]
          }
        },
        "Targets": [
          {
            "Arn": {
              "Fn::GetAtt": [
                "EventFunction",
                "Arn"
              ]
            },
            "Id": "lambda-target"
          }
        ]
      }
    }
  }
}`
	if err := os.WriteFile(templatePath, []byte(templateBody), 0o644); err != nil {
		t.Fatalf("write events template: %v", err)
	}

	runAWS(t, h.baseURL, "cloudformation", "create-stack", "--stack-name", "events-stack", "--template-body", "file://"+templatePath, "--capabilities", "CAPABILITY_NAMED_IAM")

	entriesPath := filepath.Join(t.TempDir(), "event-entries.json")
	entriesJSON := `[{"EventBusName":"cfn-event-bus","Source":"stratus.cfn","DetailType":"demo","Detail":"{\"kind\":\"demo\",\"message\":\"hello events\"}"}]`
	if err := os.WriteFile(entriesPath, []byte(entriesJSON), 0o644); err != nil {
		t.Fatalf("write event entries: %v", err)
	}
	runAWS(t, h.baseURL, "events", "put-events", "--entries", "file://"+entriesPath, "--output", "json")

	snapshotPath := filepath.Join(t.TempDir(), "events-snapshot.json")
	_ = runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", "cfn-events-target",
		"--payload", `{"mode":"snapshot"}`,
		snapshotPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	snapshotBody, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read events snapshot: %v", err)
	}
	if !strings.Contains(string(snapshotBody), `hello events`) {
		t.Fatalf("expected event delivery in lambda snapshot: %s", snapshotBody)
	}

	runAWS(t, h.baseURL, "cloudformation", "delete-stack", "--stack-name", "events-stack")
}

func TestAWSCLICloudFormationExecutesRestAPIAndStepFunctions(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	templatePath := filepath.Join(t.TempDir(), "rest-step-template.json")
	templateBody := `{
  "AWSTemplateFormatVersion": "2010-09-09",
  "Resources": {
    "ExecRole": {
      "Type": "AWS::IAM::Role",
      "Properties": {
        "RoleName": "cfn-rest-steps-role",
        "AssumeRolePolicyDocument": {
          "Version": "2012-10-17",
          "Statement": [
            {
              "Effect": "Allow",
              "Principal": {
                "Service": [
                  "lambda.amazonaws.com",
                  "states.amazonaws.com"
                ]
              },
              "Action": "sts:AssumeRole"
            }
          ]
        }
      }
    },
    "WorkerFunction": {
      "Type": "AWS::Lambda::Function",
      "Properties": {
        "FunctionName": "cfn-rest-steps-fn",
        "Handler": "index.main",
        "Runtime": "python3.11",
        "Role": {
          "Fn::GetAtt": [
            "ExecRole",
            "Arn"
          ]
        },
        "Code": {
          "ZipFile": "def main(event, context):\n    if isinstance(event, dict) and event.get('httpMethod'):\n        return {'statusCode': 200, 'headers': {'content-type': 'text/plain'}, 'body': 'hello rest cfn'}\n    return {'handled': event.get('message', 'missing')}\n"
        }
      }
    },
    "RestApi": {
      "Type": "AWS::ApiGateway::RestApi",
      "Properties": {
        "Name": "cfn-rest-api"
      }
    },
    "HelloResource": {
      "Type": "AWS::ApiGateway::Resource",
      "Properties": {
        "RestApiId": {
          "Ref": "RestApi"
        },
        "ParentId": {
          "Fn::GetAtt": [
            "RestApi",
            "RootResourceId"
          ]
        },
        "PathPart": "hello"
      }
    },
    "HelloMethod": {
      "Type": "AWS::ApiGateway::Method",
      "Properties": {
        "RestApiId": {
          "Ref": "RestApi"
        },
        "ResourceId": {
          "Ref": "HelloResource"
        },
        "HttpMethod": "GET",
        "AuthorizationType": "NONE",
        "Integration": {
          "Type": "AWS_PROXY",
          "IntegrationHttpMethod": "POST",
          "Uri": {
            "Fn::GetAtt": [
              "WorkerFunction",
              "Arn"
            ]
          }
        }
      }
    },
    "Deployment": {
      "Type": "AWS::ApiGateway::Deployment",
      "Properties": {
        "RestApiId": {
          "Ref": "RestApi"
        },
        "Description": "cfn deployment"
      }
    },
    "Stage": {
      "Type": "AWS::ApiGateway::Stage",
      "Properties": {
        "RestApiId": {
          "Ref": "RestApi"
        },
        "DeploymentId": {
          "Ref": "Deployment"
        },
        "StageName": "prod"
      }
    },
    "StateMachine": {
      "Type": "AWS::StepFunctions::StateMachine",
      "Properties": {
        "StateMachineName": "cfn-state-machine",
        "RoleArn": {
          "Fn::GetAtt": [
            "ExecRole",
            "Arn"
          ]
        },
        "Definition": {
          "StartAt": "CallWorker",
          "States": {
            "CallWorker": {
              "Type": "Task",
              "Resource": {
                "Fn::GetAtt": [
                  "WorkerFunction",
                  "Arn"
                ]
              },
              "End": true
            }
          }
        }
      }
    }
  }
}`
	if err := os.WriteFile(templatePath, []byte(templateBody), 0o644); err != nil {
		t.Fatalf("write rest + stepfunctions template: %v", err)
	}

	runAWS(t, h.baseURL, "cloudformation", "create-stack", "--stack-name", "rest-step-stack", "--template-body", "file://"+templatePath, "--capabilities", "CAPABILITY_NAMED_IAM")

	apisOut := runAWS(t, h.baseURL, "apigateway", "get-rest-apis", "--output", "json")
	var apis struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(apisOut, &apis); err != nil {
		t.Fatalf("decode get-rest-apis output: %v\n%s", err, apisOut)
	}
	apiID := ""
	for _, item := range apis.Items {
		if item.Name == "cfn-rest-api" {
			apiID = item.ID
			break
		}
	}
	if apiID == "" {
		t.Fatalf("expected cfn rest api in get-rest-apis output: %s", apisOut)
	}

	resp, err := http.Get(h.baseURL + "/_aws/restapis/" + apiID + "/prod/_user_request_/hello")
	if err != nil {
		t.Fatalf("invoke cfn rest api: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello rest cfn" {
		t.Fatalf("unexpected cfn rest api response %d: %s", resp.StatusCode, body)
	}

	machinesOut := runAWS(t, h.baseURL, "stepfunctions", "list-state-machines", "--output", "json")
	var machines struct {
		StateMachines []struct {
			StateMachineArn string `json:"stateMachineArn"`
			Name            string `json:"name"`
		} `json:"stateMachines"`
	}
	if err := json.Unmarshal(machinesOut, &machines); err != nil {
		t.Fatalf("decode list-state-machines output: %v\n%s", err, machinesOut)
	}
	stateMachineArn := ""
	for _, item := range machines.StateMachines {
		if item.Name == "cfn-state-machine" {
			stateMachineArn = item.StateMachineArn
			break
		}
	}
	if stateMachineArn == "" {
		t.Fatalf("expected cfn state machine in list-state-machines output: %s", machinesOut)
	}

	startOut := runAWS(t, h.baseURL,
		"stepfunctions", "start-execution",
		"--state-machine-arn", stateMachineArn,
		"--name", "cfn-run-1",
		"--input", `{"message":"hello step cfn"}`,
		"--output", "json",
	)
	var started struct {
		ExecutionArn string `json:"executionArn"`
	}
	if err := json.Unmarshal(startOut, &started); err != nil {
		t.Fatalf("decode start-execution output: %v\n%s", err, startOut)
	}
	describeOut := runAWS(t, h.baseURL, "stepfunctions", "describe-execution", "--execution-arn", started.ExecutionArn, "--output", "json")
	if !strings.Contains(string(describeOut), `hello step cfn`) || !strings.Contains(string(describeOut), `"SUCCEEDED"`) {
		t.Fatalf("unexpected describe-execution output: %s", describeOut)
	}

	runAWS(t, h.baseURL, "cloudformation", "delete-stack", "--stack-name", "rest-step-stack")
}

func TestAWSCLISNSWorkflow(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	createOut := runAWS(t, h.baseURL, "sns", "create-topic", "--name", "mission-sns", "--output", "json")
	var created struct {
		TopicArn string `json:"TopicArn"`
	}
	if err := json.Unmarshal(createOut, &created); err != nil {
		t.Fatalf("decode create-topic output: %v\n%s", err, createOut)
	}
	if created.TopicArn == "" {
		t.Fatalf("expected topic arn: %s", createOut)
	}

	listOut := runAWS(t, h.baseURL, "sns", "list-topics", "--output", "json")
	if !strings.Contains(string(listOut), created.TopicArn) {
		t.Fatalf("expected topic in list-topics output: %s", listOut)
	}

	runAWS(t, h.baseURL,
		"sns", "set-topic-attributes",
		"--topic-arn", created.TopicArn,
		"--attribute-name", "DisplayName",
		"--attribute-value", "Mission SNS",
	)

	attrsOut := runAWS(t, h.baseURL, "sns", "get-topic-attributes", "--topic-arn", created.TopicArn, "--output", "json")
	if !strings.Contains(string(attrsOut), `"DisplayName": "Mission SNS"`) && !strings.Contains(string(attrsOut), `"DisplayName":"Mission SNS"`) {
		t.Fatalf("expected display name in get-topic-attributes output: %s", attrsOut)
	}

	publishOut := runAWS(t, h.baseURL,
		"sns", "publish",
		"--topic-arn", created.TopicArn,
		"--message", "hello from sns",
		"--output", "json",
	)
	if !strings.Contains(string(publishOut), "MessageId") {
		t.Fatalf("expected publish message id: %s", publishOut)
	}

	runAWS(t, h.baseURL, "sns", "delete-topic", "--topic-arn", created.TopicArn)

	deletedCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "sns", "get-topic-attributes", "--topic-arn", created.TopicArn)
	deletedCmd.Env = awsEnv()
	deletedOut, err := deletedCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected deleted topic lookup to fail")
	}
	if !strings.Contains(string(deletedOut), "NotFound") {
		t.Fatalf("expected NotFound after delete, got: %s", deletedOut)
	}
}

func TestAWSCLIEventBridgeWorkflowAndLambdaDelivery(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	functionName := "eventbridge-target"
	zipPath := filepath.Join(t.TempDir(), "eventbridge-target.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "deliveries = []\n\ndef main(event, context):\n    global deliveries\n    if isinstance(event, dict) and event.get('mode') == 'snapshot':\n        return {'deliveries': deliveries}\n    deliveries.append(event)\n    return {'accepted': True}\n",
	}); err != nil {
		t.Fatalf("write eventbridge lambda zip: %v", err)
	}

	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", functionName,
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
	)

	createBusOut := runAWS(t, h.baseURL, "events", "create-event-bus", "--name", "mission-bus", "--output", "json")
	if !strings.Contains(string(createBusOut), "mission-bus") && !strings.Contains(string(createBusOut), "EventBusArn") {
		t.Fatalf("unexpected create-event-bus output: %s", createBusOut)
	}

	listBusesOut := runAWS(t, h.baseURL, "events", "list-event-buses", "--output", "json")
	if !strings.Contains(string(listBusesOut), `"default"`) || !strings.Contains(string(listBusesOut), `"mission-bus"`) {
		t.Fatalf("expected default and custom buses in list-event-buses output: %s", listBusesOut)
	}

	pattern := `{"source":["stratus.test"],"detail":{"kind":["demo"]}}`
	putRuleOut := runAWS(t, h.baseURL,
		"events", "put-rule",
		"--name", "mission-rule",
		"--event-bus-name", "mission-bus",
		"--event-pattern", pattern,
		"--output", "json",
	)
	if !strings.Contains(string(putRuleOut), "RuleArn") {
		t.Fatalf("unexpected put-rule output: %s", putRuleOut)
	}

	listRulesOut := runAWS(t, h.baseURL, "events", "list-rules", "--event-bus-name", "mission-bus", "--output", "json")
	if !strings.Contains(string(listRulesOut), `"mission-rule"`) {
		t.Fatalf("expected rule in list-rules output: %s", listRulesOut)
	}

	targetsPath := filepath.Join(t.TempDir(), "targets.json")
	targetsJSON := fmt.Sprintf(`[{"Id":"lambda-target","Arn":"arn:aws:lambda:us-east-1:000000000000:function:%s"}]`, functionName)
	if err := os.WriteFile(targetsPath, []byte(targetsJSON), 0o644); err != nil {
		t.Fatalf("write targets json: %v", err)
	}
	putTargetsOut := runAWS(t, h.baseURL,
		"events", "put-targets",
		"--rule", "mission-rule",
		"--event-bus-name", "mission-bus",
		"--targets", "file://"+targetsPath,
		"--output", "json",
	)
	if !strings.Contains(string(putTargetsOut), `"FailedEntryCount": 0`) && !strings.Contains(string(putTargetsOut), `"FailedEntryCount":0`) {
		t.Fatalf("unexpected put-targets output: %s", putTargetsOut)
	}

	listTargetsOut := runAWS(t, h.baseURL,
		"events", "list-targets-by-rule",
		"--rule", "mission-rule",
		"--event-bus-name", "mission-bus",
		"--output", "json",
	)
	if !strings.Contains(string(listTargetsOut), `"lambda-target"`) {
		t.Fatalf("expected target in list-targets-by-rule output: %s", listTargetsOut)
	}

	entriesPath := filepath.Join(t.TempDir(), "entries.json")
	entriesJSON := `[{"EventBusName":"mission-bus","Source":"stratus.test","DetailType":"demo","Detail":"{\"kind\":\"demo\",\"message\":\"hello\"}"}]`
	if err := os.WriteFile(entriesPath, []byte(entriesJSON), 0o644); err != nil {
		t.Fatalf("write entries json: %v", err)
	}
	putEventsOut := runAWS(t, h.baseURL,
		"events", "put-events",
		"--entries", "file://"+entriesPath,
		"--output", "json",
	)
	if !strings.Contains(string(putEventsOut), `"FailedEntryCount": 0`) && !strings.Contains(string(putEventsOut), `"FailedEntryCount":0`) {
		t.Fatalf("unexpected put-events output: %s", putEventsOut)
	}
	if !strings.Contains(string(putEventsOut), "EventId") {
		t.Fatalf("expected event id in put-events output: %s", putEventsOut)
	}

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	_ = runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", functionName,
		"--payload", `{"mode":"snapshot"}`,
		snapshotPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	snapshotBody, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot payload: %v", err)
	}
	if !strings.Contains(string(snapshotBody), `"stratus.test"`) || !strings.Contains(string(snapshotBody), `"message": "hello"`) && !strings.Contains(string(snapshotBody), `"message":"hello"`) {
		t.Fatalf("expected EventBridge delivery in lambda snapshot: %s", snapshotBody)
	}

	removeTargetsOut := runAWS(t, h.baseURL,
		"events", "remove-targets",
		"--rule", "mission-rule",
		"--event-bus-name", "mission-bus",
		"--ids", "lambda-target",
		"--output", "json",
	)
	if !strings.Contains(string(removeTargetsOut), `"FailedEntryCount": 0`) && !strings.Contains(string(removeTargetsOut), `"FailedEntryCount":0`) {
		t.Fatalf("unexpected remove-targets output: %s", removeTargetsOut)
	}

	runAWS(t, h.baseURL, "events", "delete-rule", "--name", "mission-rule", "--event-bus-name", "mission-bus")
	runAWS(t, h.baseURL, "events", "delete-event-bus", "--name", "mission-bus")
}

func TestAWSCLIEventBridgeDepthSchedulesArchivesAndSQSTargets(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	queueOut := runAWS(t, h.baseURL, "sqs", "create-queue", "--queue-name", "eventbridge-depth-queue", "--output", "json")
	var queue struct {
		QueueURL string `json:"QueueUrl"`
	}
	if err := json.Unmarshal(queueOut, &queue); err != nil {
		t.Fatalf("decode queue create output: %v\n%s", err, queueOut)
	}
	queueAttrs := runAWS(t, h.baseURL, "sqs", "get-queue-attributes", "--queue-url", queue.QueueURL, "--attribute-names", "QueueArn", "--output", "json")
	var attrPayload struct {
		Attributes map[string]string `json:"Attributes"`
	}
	if err := json.Unmarshal(queueAttrs, &attrPayload); err != nil {
		t.Fatalf("decode queue attrs: %v\n%s", err, queueAttrs)
	}

	runAWS(t, h.baseURL,
		"events", "create-archive",
		"--archive-name", "depth-archive",
		"--event-source-arn", "arn:aws:events:us-east-1:000000000000:event-bus/default",
		"--description", "archive depth",
		"--output", "json",
	)
	archivesOut := runAWS(t, h.baseURL, "events", "list-archives", "--output", "json")
	if !strings.Contains(string(archivesOut), `"depth-archive"`) {
		t.Fatalf("expected archive in list-archives output: %s", archivesOut)
	}
	describeArchiveOut := runAWS(t, h.baseURL, "events", "describe-archive", "--archive-name", "depth-archive", "--output", "json")
	if !strings.Contains(string(describeArchiveOut), `"archive depth"`) {
		t.Fatalf("expected archive description in describe-archive output: %s", describeArchiveOut)
	}

	pattern := `{"source":[{"prefix":"app."}],"detail":{"score":[{"numeric":[">",10]}]}}`
	runAWS(t, h.baseURL,
		"events", "put-rule",
		"--name", "pattern-depth-rule",
		"--event-pattern", pattern,
		"--output", "json",
	)
	targetsPath := filepath.Join(t.TempDir(), "eventbridge-depth-targets.json")
	if err := os.WriteFile(targetsPath, []byte(fmt.Sprintf(`[{"Id":"queue-target","Arn":"%s"}]`, attrPayload.Attributes["QueueArn"])), 0o644); err != nil {
		t.Fatalf("write queue target file: %v", err)
	}
	runAWS(t, h.baseURL, "events", "put-targets", "--rule", "pattern-depth-rule", "--targets", "file://"+targetsPath, "--output", "json")
	entriesPath := filepath.Join(t.TempDir(), "eventbridge-depth-entries.json")
	entries := `[{"Source":"app.demo","DetailType":"score","Detail":"{\"score\":12}"}]`
	if err := os.WriteFile(entriesPath, []byte(entries), 0o644); err != nil {
		t.Fatalf("write entries file: %v", err)
	}
	runAWS(t, h.baseURL, "events", "put-events", "--entries", "file://"+entriesPath, "--output", "json")
	patternMessage := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queue.QueueURL, "--wait-time-seconds", "1", "--output", "json")
	if !strings.Contains(string(patternMessage), `app.demo`) || !strings.Contains(string(patternMessage), `score`) {
		t.Fatalf("expected eventbridge pattern match delivery into SQS: %s", patternMessage)
	}

	runAWS(t, h.baseURL,
		"events", "put-rule",
		"--name", "schedule-depth-rule",
		"--schedule-expression", "rate(1 second)",
		"--output", "json",
	)
	runAWS(t, h.baseURL, "events", "put-targets", "--rule", "schedule-depth-rule", "--targets", "file://"+targetsPath, "--output", "json")
	time.Sleep(1500 * time.Millisecond)
	scheduledMessage := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queue.QueueURL, "--wait-time-seconds", "1", "--output", "json")
	if !strings.Contains(string(scheduledMessage), `aws.events`) || !strings.Contains(string(scheduledMessage), `Scheduled Event`) {
		t.Fatalf("expected scheduled event delivery into SQS: %s", scheduledMessage)
	}
}

func TestAWSCLISecretsManagerWorkflow(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	createOut := runAWS(t, h.baseURL,
		"secretsmanager", "create-secret",
		"--name", "mission/secret",
		"--description", "mission secret",
		"--secret-string", `{"hello":"world"}`,
		"--output", "json",
	)
	var created struct {
		ARN       string `json:"ARN"`
		Name      string `json:"Name"`
		VersionID string `json:"VersionId"`
	}
	if err := json.Unmarshal(createOut, &created); err != nil {
		t.Fatalf("decode create-secret output: %v\n%s", err, createOut)
	}
	if created.ARN == "" || created.Name != "mission/secret" || created.VersionID == "" {
		t.Fatalf("unexpected create-secret output: %+v", created)
	}

	listOut := runAWS(t, h.baseURL, "secretsmanager", "list-secrets", "--output", "json")
	if !strings.Contains(string(listOut), `"mission/secret"`) {
		t.Fatalf("expected secret in list-secrets output: %s", listOut)
	}

	describeOut := runAWS(t, h.baseURL, "secretsmanager", "describe-secret", "--secret-id", created.ARN, "--output", "json")
	if !strings.Contains(string(describeOut), `"mission/secret"`) || !strings.Contains(string(describeOut), `"VersionIdsToStages"`) {
		t.Fatalf("unexpected describe-secret output: %s", describeOut)
	}

	getOut := runAWS(t, h.baseURL, "secretsmanager", "get-secret-value", "--secret-id", "mission/secret", "--output", "json")
	if !strings.Contains(string(getOut), `"SecretString": "{\"hello\":\"world\"}"`) && !strings.Contains(string(getOut), `"SecretString":"{\"hello\":\"world\"}"`) {
		t.Fatalf("unexpected get-secret-value output: %s", getOut)
	}

	updateOut := runAWS(t, h.baseURL,
		"secretsmanager", "update-secret",
		"--secret-id", "mission/secret",
		"--secret-string", `{"hello":"earth"}`,
		"--output", "json",
	)
	if !strings.Contains(string(updateOut), "VersionId") {
		t.Fatalf("unexpected update-secret output: %s", updateOut)
	}

	getUpdatedOut := runAWS(t, h.baseURL, "secretsmanager", "get-secret-value", "--secret-id", created.ARN, "--output", "json")
	if !strings.Contains(string(getUpdatedOut), `earth`) {
		t.Fatalf("expected updated secret value: %s", getUpdatedOut)
	}

	deleteOut := runAWS(t, h.baseURL,
		"secretsmanager", "delete-secret",
		"--secret-id", "mission/secret",
		"--force-delete-without-recovery",
		"--output", "json",
	)
	if !strings.Contains(string(deleteOut), `"DeletionDate"`) {
		t.Fatalf("unexpected delete-secret output: %s", deleteOut)
	}

	deletedCmd := exec.Command("aws", "--endpoint-url", h.baseURL, "secretsmanager", "get-secret-value", "--secret-id", "mission/secret")
	deletedCmd.Env = awsEnv()
	deletedOut, err := deletedCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected deleted secret lookup to fail")
	}
	if !strings.Contains(string(deletedOut), "ResourceNotFoundException") {
		t.Fatalf("expected ResourceNotFoundException after delete, got: %s", deletedOut)
	}
}

func TestAWSCLIAPIGatewayRESTWorkflow(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	functionName := "rest-api-target"
	zipPath := filepath.Join(t.TempDir(), "rest-api-target.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "def main(event, context):\n    return {'statusCode': 200, 'headers': {'content-type': 'text/plain'}, 'body': 'hello rest'}\n",
	}); err != nil {
		t.Fatalf("write api gateway rest lambda zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", functionName,
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
	)

	createOut := runAWS(t, h.baseURL, "apigateway", "create-rest-api", "--name", "mission-rest-api", "--output", "json")
	var api struct {
		ID             string `json:"id"`
		RootResourceID string `json:"rootResourceId"`
	}
	if err := json.Unmarshal(createOut, &api); err != nil {
		t.Fatalf("decode create-rest-api output: %v\n%s", err, createOut)
	}
	if api.ID == "" || api.RootResourceID == "" {
		t.Fatalf("unexpected create-rest-api output: %s", createOut)
	}

	getAPIsOut := runAWS(t, h.baseURL, "apigateway", "get-rest-apis", "--output", "json")
	if !strings.Contains(string(getAPIsOut), `"mission-rest-api"`) {
		t.Fatalf("expected api in get-rest-apis output: %s", getAPIsOut)
	}

	resourceOut := runAWS(t, h.baseURL,
		"apigateway", "create-resource",
		"--rest-api-id", api.ID,
		"--parent-id", api.RootResourceID,
		"--path-part", "hello",
		"--output", "json",
	)
	var resource struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(resourceOut, &resource); err != nil {
		t.Fatalf("decode create-resource output: %v\n%s", err, resourceOut)
	}
	if resource.ID == "" || resource.Path != "/hello" {
		t.Fatalf("unexpected create-resource output: %+v", resource)
	}

	runAWS(t, h.baseURL,
		"apigateway", "put-method",
		"--rest-api-id", api.ID,
		"--resource-id", resource.ID,
		"--http-method", "GET",
		"--authorization-type", "NONE",
	)

	runAWS(t, h.baseURL,
		"apigateway", "put-integration",
		"--rest-api-id", api.ID,
		"--resource-id", resource.ID,
		"--http-method", "GET",
		"--type", "AWS_PROXY",
		"--integration-http-method", "POST",
		"--uri", fmt.Sprintf("arn:aws:lambda:us-east-1:000000000000:function:%s", functionName),
	)

	deployOut := runAWS(t, h.baseURL,
		"apigateway", "create-deployment",
		"--rest-api-id", api.ID,
		"--stage-name", "prod",
		"--output", "json",
	)
	if !strings.Contains(string(deployOut), `"id"`) {
		t.Fatalf("unexpected create-deployment output: %s", deployOut)
	}

	stagesOut := runAWS(t, h.baseURL, "apigateway", "get-stages", "--rest-api-id", api.ID, "--output", "json")
	if !strings.Contains(string(stagesOut), `"prod"`) {
		t.Fatalf("expected stage in get-stages output: %s", stagesOut)
	}

	resp, err := http.Get(h.baseURL + "/_aws/restapis/" + api.ID + "/prod/_user_request_/hello")
	if err != nil {
		t.Fatalf("invoke rest api: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hello rest" {
		t.Fatalf("unexpected rest api response %d: %s", resp.StatusCode, body)
	}
}

func TestAWSCLIKinesisWorkflow(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	runAWS(t, h.baseURL, "kinesis", "create-stream", "--stream-name", "mission-stream", "--shard-count", "1")

	describeOut := runAWS(t, h.baseURL, "kinesis", "describe-stream-summary", "--stream-name", "mission-stream", "--output", "json")
	if !strings.Contains(string(describeOut), `"StreamStatus": "ACTIVE"`) && !strings.Contains(string(describeOut), `"StreamStatus":"ACTIVE"`) {
		t.Fatalf("unexpected describe-stream-summary output: %s", describeOut)
	}

	listOut := runAWS(t, h.baseURL, "kinesis", "list-streams", "--output", "json")
	if !strings.Contains(string(listOut), `"mission-stream"`) {
		t.Fatalf("expected stream in list-streams output: %s", listOut)
	}

	shardsOut := runAWS(t, h.baseURL, "kinesis", "list-shards", "--stream-name", "mission-stream", "--output", "json")
	var shards struct {
		Shards []struct {
			ShardID string `json:"ShardId"`
		} `json:"Shards"`
	}
	if err := json.Unmarshal(shardsOut, &shards); err != nil {
		t.Fatalf("decode list-shards output: %v\n%s", err, shardsOut)
	}
	if len(shards.Shards) != 1 || shards.Shards[0].ShardID == "" {
		t.Fatalf("unexpected list-shards output: %+v", shards)
	}

	putOut := runAWS(t, h.baseURL,
		"kinesis", "put-record",
		"--stream-name", "mission-stream",
		"--partition-key", "pk1",
		"--data", "aGVsbG8=",
		"--output", "json",
	)
	if !strings.Contains(string(putOut), "SequenceNumber") {
		t.Fatalf("unexpected put-record output: %s", putOut)
	}

	iterOut := runAWS(t, h.baseURL,
		"kinesis", "get-shard-iterator",
		"--stream-name", "mission-stream",
		"--shard-id", shards.Shards[0].ShardID,
		"--shard-iterator-type", "TRIM_HORIZON",
		"--output", "json",
	)
	var iter struct {
		ShardIterator string `json:"ShardIterator"`
	}
	if err := json.Unmarshal(iterOut, &iter); err != nil {
		t.Fatalf("decode get-shard-iterator output: %v\n%s", err, iterOut)
	}
	if iter.ShardIterator == "" {
		t.Fatalf("expected shard iterator: %s", iterOut)
	}

	recordsOut := runAWS(t, h.baseURL, "kinesis", "get-records", "--shard-iterator", iter.ShardIterator, "--output", "json")
	if !strings.Contains(string(recordsOut), `"pk1"`) {
		t.Fatalf("expected partition key in get-records output: %s", recordsOut)
	}
	if !strings.Contains(string(recordsOut), `"aGVsbG8="`) {
		t.Fatalf("expected base64 encoded payload in get-records output: %s", recordsOut)
	}

	runAWS(t, h.baseURL, "kinesis", "delete-stream", "--stream-name", "mission-stream")
}

func TestAWSCLIKinesisDepthPutRecordsDescribeAndEventSourceMapping(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	runAWS(t, h.baseURL, "kinesis", "create-stream", "--stream-name", "depth-stream", "--shard-count", "2")
	describeOut := runAWS(t, h.baseURL, "kinesis", "describe-stream", "--stream-name", "depth-stream", "--output", "json")
	if !strings.Contains(string(describeOut), `"Shards"`) || !strings.Contains(string(describeOut), `"depth-stream"`) {
		t.Fatalf("unexpected describe-stream output: %s", describeOut)
	}

	putRecordsPath := filepath.Join(t.TempDir(), "kinesis-records.json")
	if err := os.WriteFile(putRecordsPath, []byte(`[{"Data":"Zmlyc3Q=","PartitionKey":"a"},{"Data":"c2Vjb25k","PartitionKey":"b"}]`), 0o644); err != nil {
		t.Fatalf("write kinesis put-records file: %v", err)
	}
	putRecordsOut := runAWS(t, h.baseURL, "kinesis", "put-records", "--stream-name", "depth-stream", "--records", "file://"+putRecordsPath, "--output", "json")
	if !strings.Contains(string(putRecordsOut), `"FailedRecordCount": 0`) && !strings.Contains(string(putRecordsOut), `"FailedRecordCount":0`) {
		t.Fatalf("unexpected put-records output: %s", putRecordsOut)
	}

	shardsOut := runAWS(t, h.baseURL, "kinesis", "list-shards", "--stream-name", "depth-stream", "--output", "json")
	var shards struct {
		Shards []struct {
			ShardID string `json:"ShardId"`
		} `json:"Shards"`
	}
	if err := json.Unmarshal(shardsOut, &shards); err != nil {
		t.Fatalf("decode shards output: %v\n%s", err, shardsOut)
	}
	iterOut := runAWS(t, h.baseURL,
		"kinesis", "get-shard-iterator",
		"--stream-name", "depth-stream",
		"--shard-id", shards.Shards[0].ShardID,
		"--shard-iterator-type", "TRIM_HORIZON",
		"--output", "json",
	)
	var iter struct {
		ShardIterator string `json:"ShardIterator"`
	}
	if err := json.Unmarshal(iterOut, &iter); err != nil {
		t.Fatalf("decode at-timestamp iterator: %v\n%s", err, iterOut)
	}
	recordsOut := runAWS(t, h.baseURL, "kinesis", "get-records", "--shard-iterator", iter.ShardIterator, "--output", "json")
	if !strings.Contains(string(recordsOut), `"Zmlyc3Q="`) && !strings.Contains(string(recordsOut), `"c2Vjb25k"`) {
		t.Fatalf("expected data in get-records output: %s", recordsOut)
	}

	mapZip := filepath.Join(t.TempDir(), "kinesis-map.zip")
	if err := writeZip(mapZip, map[string]string{
		"handler.py": "events = []\n\ndef main(event, context):\n    global events\n    if isinstance(event, dict) and event.get('mode') == 'snapshot':\n        return {'events': events}\n    events.append(event)\n    return {'ok': True}\n",
	}); err != nil {
		t.Fatalf("write kinesis mapping zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", "kinesis-mapped",
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+mapZip,
	)
	mappingOut := runAWS(t, h.baseURL,
		"lambda", "create-event-source-mapping",
		"--function-name", "kinesis-mapped",
		"--event-source-arn", "arn:aws:kinesis:us-east-1:000000000000:stream/depth-stream",
		"--starting-position", "LATEST",
		"--output", "json",
	)
	if !strings.Contains(string(mappingOut), `"UUID"`) {
		t.Fatalf("expected mapping uuid: %s", mappingOut)
	}
	runAWS(t, h.baseURL, "kinesis", "put-record", "--stream-name", "depth-stream", "--partition-key", "mapped", "--data", "bWFwcGVk", "--output", "json")
	time.Sleep(750 * time.Millisecond)
	snapshotPath := filepath.Join(t.TempDir(), "kinesis-map-snapshot.json")
	_ = runAWS(t, h.baseURL,
		"lambda", "invoke",
		"--function-name", "kinesis-mapped",
		"--payload", `{"mode":"snapshot"}`,
		snapshotPath,
		"--cli-binary-format", "raw-in-base64-out",
		"--output", "json",
	)
	snapshotBody, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read kinesis mapping snapshot: %v", err)
	}
	if !strings.Contains(string(snapshotBody), `"aws:kinesis"`) || !strings.Contains(string(snapshotBody), `"bWFwcGVk"`) {
		t.Fatalf("expected kinesis event source mapping delivery: %s", snapshotBody)
	}
}

func TestAWSCLICloudWatchMetricsWorkflow(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runAWS(t, h.baseURL,
		"cloudwatch", "put-metric-data",
		"--namespace", "Stratus/Test",
		"--metric-name", "Requests",
		"--value", "42",
		"--unit", "Count",
		"--dimensions", "Service=API",
	)

	listOut := runAWS(t, h.baseURL,
		"cloudwatch", "list-metrics",
		"--namespace", "Stratus/Test",
		"--metric-name", "Requests",
		"--output", "json",
	)
	if !strings.Contains(string(listOut), `"Requests"`) || !strings.Contains(string(listOut), `"Stratus/Test"`) {
		t.Fatalf("unexpected list-metrics output: %s", listOut)
	}

	start := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	end := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	statsOut := runAWS(t, h.baseURL,
		"cloudwatch", "get-metric-statistics",
		"--namespace", "Stratus/Test",
		"--metric-name", "Requests",
		"--dimensions", "Name=Service,Value=API",
		"--start-time", start,
		"--end-time", end,
		"--period", "60",
		"--statistics", "Average",
		"--output", "json",
	)
	if !strings.Contains(string(statsOut), `"Average": 42`) && !strings.Contains(string(statsOut), `"Average":42`) {
		t.Fatalf("unexpected get-metric-statistics output: %s", statsOut)
	}
}

func TestAWSCLICognitoIDPWorkflow(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	poolOut := runAWS(t, h.baseURL, "cognito-idp", "create-user-pool", "--pool-name", "mission-pool", "--output", "json")
	var pool struct {
		UserPool struct {
			ID string `json:"Id"`
		} `json:"UserPool"`
	}
	if err := json.Unmarshal(poolOut, &pool); err != nil {
		t.Fatalf("decode create-user-pool output: %v\n%s", err, poolOut)
	}
	if pool.UserPool.ID == "" {
		t.Fatalf("expected user pool id: %s", poolOut)
	}

	clientOut := runAWS(t, h.baseURL,
		"cognito-idp", "create-user-pool-client",
		"--user-pool-id", pool.UserPool.ID,
		"--client-name", "mission-client",
		"--explicit-auth-flows", "USER_PASSWORD_AUTH",
		"--output", "json",
	)
	var client struct {
		UserPoolClient struct {
			ClientID string `json:"ClientId"`
		} `json:"UserPoolClient"`
	}
	if err := json.Unmarshal(clientOut, &client); err != nil {
		t.Fatalf("decode create-user-pool-client output: %v\n%s", err, clientOut)
	}
	if client.UserPoolClient.ClientID == "" {
		t.Fatalf("expected client id: %s", clientOut)
	}

	signUpOut := runAWS(t, h.baseURL,
		"cognito-idp", "sign-up",
		"--client-id", client.UserPoolClient.ClientID,
		"--username", "alice",
		"--password", "Secret123!",
		"--output", "json",
	)
	if !strings.Contains(string(signUpOut), `"UserConfirmed": false`) && !strings.Contains(string(signUpOut), `"UserConfirmed":false`) {
		t.Fatalf("unexpected sign-up output: %s", signUpOut)
	}

	runAWS(t, h.baseURL, "cognito-idp", "admin-confirm-sign-up", "--user-pool-id", pool.UserPool.ID, "--username", "alice")

	authOut := runAWS(t, h.baseURL,
		"cognito-idp", "initiate-auth",
		"--client-id", client.UserPoolClient.ClientID,
		"--auth-flow", "USER_PASSWORD_AUTH",
		"--auth-parameters", "USERNAME=alice,PASSWORD=Secret123!",
		"--output", "json",
	)
	if !strings.Contains(string(authOut), "AccessToken") || !strings.Contains(string(authOut), "IdToken") {
		t.Fatalf("unexpected initiate-auth output: %s", authOut)
	}

	listOut := runAWS(t, h.baseURL, "cognito-idp", "list-user-pools", "--max-results", "10", "--output", "json")
	if !strings.Contains(string(listOut), `"mission-pool"`) {
		t.Fatalf("expected pool in list-user-pools output: %s", listOut)
	}
}

func TestAWSCLICognitoIDPDepthAdminUsersAndGroups(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	poolOut := runAWS(t, h.baseURL, "cognito-idp", "create-user-pool", "--pool-name", "depth-pool", "--output", "json")
	var pool struct {
		UserPool struct {
			ID string `json:"Id"`
		} `json:"UserPool"`
	}
	if err := json.Unmarshal(poolOut, &pool); err != nil {
		t.Fatalf("decode pool output: %v\n%s", err, poolOut)
	}

	clientOut := runAWS(t, h.baseURL,
		"cognito-idp", "create-user-pool-client",
		"--user-pool-id", pool.UserPool.ID,
		"--client-name", "depth-client",
		"--explicit-auth-flows", "USER_PASSWORD_AUTH",
		"--output", "json",
	)
	var client struct {
		UserPoolClient struct {
			ClientID string `json:"ClientId"`
		} `json:"UserPoolClient"`
	}
	if err := json.Unmarshal(clientOut, &client); err != nil {
		t.Fatalf("decode client output: %v\n%s", err, clientOut)
	}

	runAWS(t, h.baseURL,
		"cognito-idp", "admin-create-user",
		"--user-pool-id", pool.UserPool.ID,
		"--username", "bob",
		"--temporary-password", "Temp123!",
		"--message-action", "SUPPRESS",
		"--user-attributes", "Name=email,Value=bob@example.com",
		"--output", "json",
	)
	runAWS(t, h.baseURL,
		"cognito-idp", "admin-set-user-password",
		"--user-pool-id", pool.UserPool.ID,
		"--username", "bob",
		"--password", "Secret123!",
		"--permanent",
	)
	listUsersOut := runAWS(t, h.baseURL, "cognito-idp", "list-users", "--user-pool-id", pool.UserPool.ID, "--output", "json")
	if !strings.Contains(string(listUsersOut), `"bob"`) || !strings.Contains(string(listUsersOut), `bob@example.com`) {
		t.Fatalf("expected admin-created user in list-users output: %s", listUsersOut)
	}

	runAWS(t, h.baseURL, "cognito-idp", "create-group", "--user-pool-id", pool.UserPool.ID, "--group-name", "admins", "--description", "administrators", "--output", "json")
	runAWS(t, h.baseURL, "cognito-idp", "admin-add-user-to-group", "--user-pool-id", pool.UserPool.ID, "--username", "bob", "--group-name", "admins")
	listGroupsOut := runAWS(t, h.baseURL, "cognito-idp", "list-groups", "--user-pool-id", pool.UserPool.ID, "--output", "json")
	if !strings.Contains(string(listGroupsOut), `"admins"`) {
		t.Fatalf("expected group in list-groups output: %s", listGroupsOut)
	}
	userGroupsOut := runAWS(t, h.baseURL, "cognito-idp", "admin-list-groups-for-user", "--user-pool-id", pool.UserPool.ID, "--username", "bob", "--output", "json")
	if !strings.Contains(string(userGroupsOut), `"admins"`) {
		t.Fatalf("expected user group membership in admin-list-groups-for-user output: %s", userGroupsOut)
	}

	authOut := runAWS(t, h.baseURL,
		"cognito-idp", "initiate-auth",
		"--client-id", client.UserPoolClient.ClientID,
		"--auth-flow", "USER_PASSWORD_AUTH",
		"--auth-parameters", "USERNAME=bob,PASSWORD=Secret123!",
		"--output", "json",
	)
	if !strings.Contains(string(authOut), "AccessToken") {
		t.Fatalf("expected auth success for admin-created user: %s", authOut)
	}
}

func TestAWSCLIStepFunctionsWorkflow(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	functionName := "step-functions-target"
	zipPath := filepath.Join(t.TempDir(), "step-functions-target.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "def main(event, context):\n    return {'handled': event.get('message', 'missing')}\n",
	}); err != nil {
		t.Fatalf("write step functions lambda zip: %v", err)
	}
	runAWS(t, h.baseURL,
		"lambda", "create-function",
		"--function-name", functionName,
		"--runtime", "python3.11",
		"--role", "arn:aws:iam::000000000000:role/stratus-lambda-role",
		"--handler", "handler.main",
		"--zip-file", "fileb://"+zipPath,
	)

	definitionPath := filepath.Join(t.TempDir(), "state-machine.json")
	definition := fmt.Sprintf(`{
  "StartAt": "CallLambda",
  "States": {
    "CallLambda": {
      "Type": "Task",
      "Resource": "arn:aws:lambda:us-east-1:000000000000:function:%s",
      "End": true
    }
  }
}`, functionName)
	if err := os.WriteFile(definitionPath, []byte(definition), 0o644); err != nil {
		t.Fatalf("write state machine definition: %v", err)
	}

	createOut := runAWS(t, h.baseURL,
		"stepfunctions", "create-state-machine",
		"--name", "mission-machine",
		"--definition", "file://"+definitionPath,
		"--role-arn", "arn:aws:iam::000000000000:role/stratus-stepfunctions-role",
		"--output", "json",
	)
	var created struct {
		StateMachineArn string `json:"stateMachineArn"`
	}
	if err := json.Unmarshal(createOut, &created); err != nil {
		t.Fatalf("decode create-state-machine output: %v\n%s", err, createOut)
	}
	if created.StateMachineArn == "" {
		t.Fatalf("expected state machine arn: %s", createOut)
	}

	listOut := runAWS(t, h.baseURL, "stepfunctions", "list-state-machines", "--output", "json")
	if !strings.Contains(string(listOut), `"mission-machine"`) {
		t.Fatalf("expected state machine in list-state-machines output: %s", listOut)
	}

	startOut := runAWS(t, h.baseURL,
		"stepfunctions", "start-execution",
		"--state-machine-arn", created.StateMachineArn,
		"--name", "run-1",
		"--input", `{"message":"hello steps"}`,
		"--output", "json",
	)
	var started struct {
		ExecutionArn string `json:"executionArn"`
	}
	if err := json.Unmarshal(startOut, &started); err != nil {
		t.Fatalf("decode start-execution output: %v\n%s", err, startOut)
	}
	if started.ExecutionArn == "" {
		t.Fatalf("expected execution arn: %s", startOut)
	}

	describeOut := runAWS(t, h.baseURL, "stepfunctions", "describe-execution", "--execution-arn", started.ExecutionArn, "--output", "json")
	if !strings.Contains(string(describeOut), `"SUCCEEDED"`) || !strings.Contains(string(describeOut), `hello steps`) {
		t.Fatalf("unexpected describe-execution output: %s", describeOut)
	}

	runAWS(t, h.baseURL, "stepfunctions", "delete-state-machine", "--state-machine-arn", created.StateMachineArn)
}

func TestAWSCLIStepFunctionsDepthChoiceRetryCatchMapAndParallel(t *testing.T) {
	h := startHarnessWithDataDir(t, t.TempDir())
	defer h.Close()

	retryZip := filepath.Join(t.TempDir(), "step-retry.zip")
	if err := writeZip(retryZip, map[string]string{
		"handler.py": "count = 0\n\ndef main(event, context):\n    global count\n    count += 1\n    if count == 1:\n        raise Exception('retry me')\n    return {'retried': count}\n",
	}); err != nil {
		t.Fatalf("write retry lambda zip: %v", err)
	}
	catchZip := filepath.Join(t.TempDir(), "step-catch.zip")
	if err := writeZip(catchZip, map[string]string{
		"handler.py": "def main(event, context):\n    raise Exception('always boom')\n",
	}); err != nil {
		t.Fatalf("write catch lambda zip: %v", err)
	}
	mapZip := filepath.Join(t.TempDir(), "step-map.zip")
	if err := writeZip(mapZip, map[string]string{
		"handler.py": "def main(event, context):\n    return {'seen': event.get('value', 0) * 2}\n",
	}); err != nil {
		t.Fatalf("write map lambda zip: %v", err)
	}
	runAWS(t, h.baseURL, "lambda", "create-function", "--function-name", "step-retry", "--runtime", "python3.11", "--role", "arn:aws:iam::000000000000:role/stratus-lambda-role", "--handler", "handler.main", "--zip-file", "fileb://"+retryZip)
	runAWS(t, h.baseURL, "lambda", "create-function", "--function-name", "step-catch", "--runtime", "python3.11", "--role", "arn:aws:iam::000000000000:role/stratus-lambda-role", "--handler", "handler.main", "--zip-file", "fileb://"+catchZip)
	runAWS(t, h.baseURL, "lambda", "create-function", "--function-name", "step-map", "--runtime", "python3.11", "--role", "arn:aws:iam::000000000000:role/stratus-lambda-role", "--handler", "handler.main", "--zip-file", "fileb://"+mapZip)

	definitionPath := filepath.Join(t.TempDir(), "step-depth.json")
	definition := `{
  "StartAt": "Decide",
  "States": {
    "Decide": {
      "Type": "Choice",
      "Choices": [
        {"Variable":"$.mode","StringEquals":"run","Next":"Seed"}
      ],
      "Default":"Failed"
    },
    "Seed": {
      "Type":"Pass",
      "Result":{"items":[{"value":1},{"value":2}]},
      "ResultPath":"$.work",
      "Next":"RetryTask"
    },
    "RetryTask": {
      "Type":"Task",
      "Resource":"arn:aws:lambda:us-east-1:000000000000:function:step-retry",
      "Retry":[{"ErrorEquals":["States.ALL"],"MaxAttempts":2}],
      "ResultPath":"$.retry",
      "Next":"CatchTask"
    },
    "CatchTask": {
      "Type":"Task",
      "Resource":"arn:aws:lambda:us-east-1:000000000000:function:step-catch",
      "Catch":[{"ErrorEquals":["States.ALL"],"Next":"MapState","ResultPath":"$.caught"}]
    },
    "MapState": {
      "Type":"Map",
      "ItemsPath":"$.work.items",
      "ResultPath":"$.mapped",
      "Iterator":{
        "StartAt":"MapTask",
        "States":{
          "MapTask":{
            "Type":"Task",
            "Resource":"arn:aws:lambda:us-east-1:000000000000:function:step-map",
            "End":true
          }
        }
      },
      "Next":"ParallelState"
    },
    "ParallelState": {
      "Type":"Parallel",
      "ResultPath":"$.parallel",
      "Branches":[
        {"StartAt":"Left","States":{"Left":{"Type":"Pass","Result":{"branch":"left"},"End":true}}},
        {"StartAt":"Right","States":{"Right":{"Type":"Pass","Result":{"branch":"right"},"End":true}}}
      ],
      "End":true
    },
    "Failed":{"Type":"Fail"}
  }
}`
	if err := os.WriteFile(definitionPath, []byte(definition), 0o644); err != nil {
		t.Fatalf("write step depth definition: %v", err)
	}
	createOut := runAWS(t, h.baseURL, "stepfunctions", "create-state-machine", "--name", "depth-machine", "--definition", "file://"+definitionPath, "--role-arn", "arn:aws:iam::000000000000:role/stratus-stepfunctions-role", "--output", "json")
	var created struct {
		StateMachineArn string `json:"stateMachineArn"`
	}
	if err := json.Unmarshal(createOut, &created); err != nil {
		t.Fatalf("decode depth state machine output: %v\n%s", err, createOut)
	}
	startOut := runAWS(t, h.baseURL, "stepfunctions", "start-execution", "--state-machine-arn", created.StateMachineArn, "--input", `{"mode":"run"}`, "--output", "json")
	var started struct {
		ExecutionArn string `json:"executionArn"`
	}
	if err := json.Unmarshal(startOut, &started); err != nil {
		t.Fatalf("decode start execution output: %v\n%s", err, startOut)
	}
	describeOut := runAWS(t, h.baseURL, "stepfunctions", "describe-execution", "--execution-arn", started.ExecutionArn, "--output", "json")
	if !strings.Contains(string(describeOut), `"SUCCEEDED"`) || !strings.Contains(string(describeOut), `retried`) || !strings.Contains(string(describeOut), `parallel`) || !strings.Contains(string(describeOut), `left`) || !strings.Contains(string(describeOut), `seen`) {
		t.Fatalf("unexpected stepfunctions depth output: %s", describeOut)
	}
}

func TestAWSCLIDynamoDBStreamsAndLambdaMapping(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	runAWS(t, h.baseURL,
		"dynamodb", "create-table",
		"--table-name", "stream-books",
		"--attribute-definitions", "AttributeName=id,AttributeType=S",
		"--key-schema", "AttributeName=id,KeyType=HASH",
		"--billing-mode", "PAY_PER_REQUEST",
		"--stream-specification", "StreamEnabled=true,StreamViewType=NEW_AND_OLD_IMAGES",
		"--output", "json",
	)

	streamsOut := runAWS(t, h.baseURL, "dynamodbstreams", "list-streams", "--table-name", "stream-books", "--output", "json")
	var streams struct {
		Streams []struct {
			StreamArn string `json:"StreamArn"`
		} `json:"Streams"`
	}
	if err := json.Unmarshal(streamsOut, &streams); err != nil {
		t.Fatalf("decode list-streams output: %v\n%s", err, streamsOut)
	}
	if len(streams.Streams) != 1 || streams.Streams[0].StreamArn == "" {
		t.Fatalf("expected one stream in list-streams output: %s", streamsOut)
	}
	streamArn := streams.Streams[0].StreamArn

	describeStreamOut := runAWS(t, h.baseURL, "dynamodbstreams", "describe-stream", "--stream-arn", streamArn, "--output", "json")
	if !strings.Contains(string(describeStreamOut), `"NEW_AND_OLD_IMAGES"`) {
		t.Fatalf("expected stream view type in describe-stream output: %s", describeStreamOut)
	}
	var described struct {
		StreamDescription struct {
			Shards []struct {
				ShardID string `json:"ShardId"`
			} `json:"Shards"`
		} `json:"StreamDescription"`
	}
	if err := json.Unmarshal(describeStreamOut, &described); err != nil {
		t.Fatalf("decode describe-stream output: %v\n%s", err, describeStreamOut)
	}

	runAWS(t, h.baseURL,
		"dynamodb", "put-item",
		"--table-name", "stream-books",
		"--item", `{"id":{"S":"1"},"title":{"S":"First"}}`,
		"--output", "json",
	)

	iteratorOut := runAWS(t, h.baseURL, "dynamodbstreams", "get-shard-iterator", "--stream-arn", streamArn, "--shard-id", described.StreamDescription.Shards[0].ShardID, "--shard-iterator-type", "TRIM_HORIZON", "--output", "json")
	var iterator struct {
		ShardIterator string `json:"ShardIterator"`
	}
	if err := json.Unmarshal(iteratorOut, &iterator); err != nil {
		t.Fatalf("decode get-shard-iterator output: %v\n%s", err, iteratorOut)
	}
	recordsOut := runAWS(t, h.baseURL, "dynamodbstreams", "get-records", "--shard-iterator", iterator.ShardIterator, "--output", "json")
	if !strings.Contains(string(recordsOut), `"INSERT"`) || !strings.Contains(string(recordsOut), `"aws:dynamodb"`) {
		t.Fatalf("expected dynamodb stream record in get-records output: %s", recordsOut)
	}

	zipPath := filepath.Join(t.TempDir(), "ddb-stream.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "events = []\n\ndef main(event, context):\n    global events\n    if isinstance(event, dict) and event.get('mode') == 'snapshot':\n        return {'events': events}\n    events.append(event)\n    return {'ok': True}\n",
	}); err != nil {
		t.Fatalf("write stream lambda zip: %v", err)
	}
	runAWS(t, h.baseURL, "lambda", "create-function", "--function-name", "ddb-stream-consumer", "--runtime", "python3.11", "--role", "arn:aws:iam::000000000000:role/stratus-lambda-role", "--handler", "handler.main", "--zip-file", "fileb://"+zipPath)
	runAWS(t, h.baseURL, "lambda", "create-event-source-mapping", "--function-name", "ddb-stream-consumer", "--event-source-arn", streamArn, "--starting-position", "LATEST", "--output", "json")

	runAWS(t, h.baseURL,
		"dynamodb", "put-item",
		"--table-name", "stream-books",
		"--item", `{"id":{"S":"2"},"title":{"S":"Second"}}`,
		"--output", "json",
	)
	time.Sleep(800 * time.Millisecond)
	snapshotPath := filepath.Join(t.TempDir(), "ddb-stream-snapshot.json")
	snapshotPayloadPath := filepath.Join(t.TempDir(), "ddb-stream-snapshot-payload.json")
	if err := os.WriteFile(snapshotPayloadPath, []byte(`{"mode":"snapshot"}`), 0o644); err != nil {
		t.Fatalf("write ddb snapshot payload: %v", err)
	}
	_ = runAWS(t, h.baseURL, "lambda", "invoke", "--function-name", "ddb-stream-consumer", "--payload", "fileb://"+snapshotPayloadPath, snapshotPath, "--output", "json")
	snapshotBody, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read ddb stream snapshot: %v", err)
	}
	if !strings.Contains(string(snapshotBody), `"aws:dynamodb"`) || !strings.Contains(string(snapshotBody), `"INSERT"`) {
		t.Fatalf("expected dynamodb stream event source mapping delivery: %s", snapshotBody)
	}
}

func TestAWSCLISNSSubscriptionsAndEventBridgeCustomBus(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	queueOut := runAWS(t, h.baseURL, "sqs", "create-queue", "--queue-name", "sns-depth-queue", "--output", "json")
	var queue struct {
		QueueURL string `json:"QueueUrl"`
	}
	if err := json.Unmarshal(queueOut, &queue); err != nil {
		t.Fatalf("decode create-queue output: %v\n%s", err, queueOut)
	}
	queueAttrs := runAWS(t, h.baseURL, "sqs", "get-queue-attributes", "--queue-url", queue.QueueURL, "--attribute-names", "QueueArn", "--output", "json")
	var attrs struct {
		Attributes map[string]string `json:"Attributes"`
	}
	if err := json.Unmarshal(queueAttrs, &attrs); err != nil {
		t.Fatalf("decode queue attributes: %v\n%s", err, queueAttrs)
	}
	topicOut := runAWS(t, h.baseURL, "sns", "create-topic", "--name", "depth-topic", "--output", "json")
	var topic struct {
		TopicArn string `json:"TopicArn"`
	}
	if err := json.Unmarshal(topicOut, &topic); err != nil {
		t.Fatalf("decode create-topic output: %v\n%s", err, topicOut)
	}
	subOut := runAWS(t, h.baseURL, "sns", "subscribe", "--topic-arn", topic.TopicArn, "--protocol", "sqs", "--notification-endpoint", attrs.Attributes["QueueArn"], "--output", "json")
	var sub struct {
		SubscriptionArn string `json:"SubscriptionArn"`
	}
	if err := json.Unmarshal(subOut, &sub); err != nil {
		t.Fatalf("decode subscribe output: %v\n%s", err, subOut)
	}
	if sub.SubscriptionArn == "" {
		t.Fatalf("expected subscription arn in subscribe output: %s", subOut)
	}
	runAWS(t, h.baseURL, "sns", "set-subscription-attributes", "--subscription-arn", sub.SubscriptionArn, "--attribute-name", "FilterPolicy", "--attribute-value", `{"kind":["order"]}`)
	listSubsOut := runAWS(t, h.baseURL, "sns", "list-subscriptions-by-topic", "--topic-arn", topic.TopicArn, "--output", "json")
	if !strings.Contains(string(listSubsOut), sub.SubscriptionArn) {
		t.Fatalf("expected subscription in list-subscriptions-by-topic output: %s", listSubsOut)
	}
	runAWS(t, h.baseURL, "sns", "publish", "--topic-arn", topic.TopicArn, "--message", "skip me", "--message-attributes", "kind={DataType=String,StringValue=ignored}", "--output", "json")
	emptyReceive := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queue.QueueURL, "--wait-time-seconds", "0", "--output", "json")
	if trimmed := strings.TrimSpace(string(emptyReceive)); trimmed != "" && strings.Contains(trimmed, "Messages") {
		var empty struct {
			Messages []json.RawMessage `json:"Messages"`
		}
		if err := json.Unmarshal(emptyReceive, &empty); err != nil {
			t.Fatalf("decode empty receive output: %v\n%s", err, emptyReceive)
		}
		if len(empty.Messages) != 0 {
			t.Fatalf("expected filter policy to suppress non-matching sns publish: %s", emptyReceive)
		}
	}
	runAWS(t, h.baseURL, "sns", "publish", "--topic-arn", topic.TopicArn, "--message", "deliver me", "--message-attributes", "kind={DataType=String,StringValue=order}", "--output", "json")
	delivered := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queue.QueueURL, "--wait-time-seconds", "1", "--output", "json")
	if !strings.Contains(string(delivered), "deliver me") {
		t.Fatalf("expected sns delivery into sqs: %s", delivered)
	}
	runAWS(t, h.baseURL, "sns", "set-subscription-attributes", "--subscription-arn", sub.SubscriptionArn, "--attribute-name", "FilterPolicy", "--attribute-value", `{}`)

	runAWS(t, h.baseURL, "events", "create-event-bus", "--name", "depth-custom-bus", "--output", "json")
	runAWS(t, h.baseURL, "events", "put-rule", "--event-bus-name", "depth-custom-bus", "--name", "forward-orders", "--event-pattern", `{"source":["app.orders"]}`, "--output", "json")
	targetsPath := filepath.Join(t.TempDir(), "sns-targets.json")
	if err := os.WriteFile(targetsPath, []byte(fmt.Sprintf(`[{"Id":"topic-target","Arn":"%s"}]`, topic.TopicArn)), 0o644); err != nil {
		t.Fatalf("write sns targets: %v", err)
	}
	runAWS(t, h.baseURL, "events", "put-targets", "--event-bus-name", "depth-custom-bus", "--rule", "forward-orders", "--targets", "file://"+targetsPath, "--output", "json")
	entriesPath := filepath.Join(t.TempDir(), "custom-bus-events.json")
	if err := os.WriteFile(entriesPath, []byte(`[{"EventBusName":"depth-custom-bus","Source":"app.orders","DetailType":"created","Detail":"{\"id\":\"ord-1\"}"}]`), 0o644); err != nil {
		t.Fatalf("write custom bus entries: %v", err)
	}
	runAWS(t, h.baseURL, "events", "put-events", "--entries", "file://"+entriesPath, "--output", "json")
	busDelivered := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queue.QueueURL, "--wait-time-seconds", "1", "--output", "json")
	if !strings.Contains(string(busDelivered), "ord-1") {
		t.Fatalf("expected eventbridge custom bus sns target delivery: %s", busDelivered)
	}

	runAWS(t, h.baseURL, "events", "put-rule", "--event-bus-name", "depth-custom-bus", "--name", "scheduled-orders", "--schedule-expression", "rate(1 second)", "--output", "json")
	runAWS(t, h.baseURL, "events", "put-targets", "--event-bus-name", "depth-custom-bus", "--rule", "scheduled-orders", "--targets", "file://"+targetsPath, "--output", "json")
	time.Sleep(1500 * time.Millisecond)
	scheduled := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queue.QueueURL, "--wait-time-seconds", "1", "--output", "json")
	if !strings.Contains(string(scheduled), "Scheduled Event") {
		t.Fatalf("expected scheduled custom bus delivery into sns/sqs: %s", scheduled)
	}
}

func TestAWSCLILambdaAsyncDestinations(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	queueOut := runAWS(t, h.baseURL, "sqs", "create-queue", "--queue-name", "lambda-async-success", "--output", "json")
	var queue struct {
		QueueURL string `json:"QueueUrl"`
	}
	if err := json.Unmarshal(queueOut, &queue); err != nil {
		t.Fatalf("decode create queue output: %v\n%s", err, queueOut)
	}
	queueAttrs := runAWS(t, h.baseURL, "sqs", "get-queue-attributes", "--queue-url", queue.QueueURL, "--attribute-names", "QueueArn", "--output", "json")
	var attrs struct {
		Attributes map[string]string `json:"Attributes"`
	}
	if err := json.Unmarshal(queueAttrs, &attrs); err != nil {
		t.Fatalf("decode queue attrs output: %v\n%s", err, queueAttrs)
	}

	zipPath := filepath.Join(t.TempDir(), "async.zip")
	if err := writeZip(zipPath, map[string]string{
		"handler.py": "def main(event, context):\n    return {'ok': True, 'input': event}\n",
	}); err != nil {
		t.Fatalf("write async lambda zip: %v", err)
	}
	runAWS(t, h.baseURL, "lambda", "create-function", "--function-name", "async-func", "--runtime", "python3.11", "--role", "arn:aws:iam::000000000000:role/stratus-lambda-role", "--handler", "handler.main", "--zip-file", "fileb://"+zipPath)
	configOut := runAWS(t, h.baseURL, "lambda", "put-function-event-invoke-config", "--function-name", "async-func", "--destination-config", fmt.Sprintf("OnSuccess={Destination=%s}", attrs.Attributes["QueueArn"]), "--output", "json")
	if !strings.Contains(string(configOut), attrs.Attributes["QueueArn"]) {
		t.Fatalf("expected destination arn in put-function-event-invoke-config output: %s", configOut)
	}
	getConfigOut := runAWS(t, h.baseURL, "lambda", "get-function-event-invoke-config", "--function-name", "async-func", "--output", "json")
	if !strings.Contains(string(getConfigOut), attrs.Attributes["QueueArn"]) {
		t.Fatalf("expected destination arn in get-function-event-invoke-config output: %s", getConfigOut)
	}
	payloadPath := filepath.Join(t.TempDir(), "async-payload.json")
	if err := os.WriteFile(payloadPath, []byte(`{"hello":"async"}`), 0o644); err != nil {
		t.Fatalf("write async payload: %v", err)
	}
	runAWS(t, h.baseURL, "lambda", "invoke", "--function-name", "async-func", "--invocation-type", "Event", "--payload", "fileb://"+payloadPath, filepath.Join(t.TempDir(), "async-meta.json"), "--output", "json")
	delivered := runAWS(t, h.baseURL, "sqs", "receive-message", "--queue-url", queue.QueueURL, "--wait-time-seconds", "1", "--output", "json")
	if !strings.Contains(string(delivered), `condition`) || !strings.Contains(string(delivered), `Success`) {
		t.Fatalf("expected async destination delivery into sqs: %s", delivered)
	}
	runAWS(t, h.baseURL, "lambda", "delete-function-event-invoke-config", "--function-name", "async-func", "--output", "json")
}

func TestAWSCLIECRECSELBV2ControlPlane(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	repoOut := runAWS(t, h.baseURL, "ecr", "create-repository", "--repository-name", "demo/app", "--output", "json")
	if !strings.Contains(string(repoOut), "demo/app") {
		t.Fatalf("expected repository in create-repository output: %s", repoOut)
	}
	describeReposOut := runAWS(t, h.baseURL, "ecr", "describe-repositories", "--repository-names", "demo/app", "--output", "json")
	if !strings.Contains(string(describeReposOut), "repositoryUri") {
		t.Fatalf("expected repositoryUri in describe-repositories output: %s", describeReposOut)
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":2,"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111"},"layers":[]}`), 0o644); err != nil {
		t.Fatalf("write image manifest: %v", err)
	}
	putImageOut := runAWS(t, h.baseURL, "ecr", "put-image", "--repository-name", "demo/app", "--image-tag", "latest", "--image-manifest", "file://"+manifestPath, "--output", "json")
	if !strings.Contains(string(putImageOut), "imageDigest") {
		t.Fatalf("expected imageDigest in put-image output: %s", putImageOut)
	}
	listImagesOut := runAWS(t, h.baseURL, "ecr", "list-images", "--repository-name", "demo/app", "--output", "json")
	if !strings.Contains(string(listImagesOut), "latest") {
		t.Fatalf("expected latest tag in list-images output: %s", listImagesOut)
	}
	batchGetOut := runAWS(t, h.baseURL, "ecr", "batch-get-image", "--repository-name", "demo/app", "--image-ids", "imageTag=latest", "--output", "json")
	if !strings.Contains(string(batchGetOut), "imageManifest") {
		t.Fatalf("expected imageManifest in batch-get-image output: %s", batchGetOut)
	}

	runAWS(t, h.baseURL, "ecs", "create-cluster", "--cluster-name", "demo-cluster", "--output", "json")
	taskDefsPath := filepath.Join(t.TempDir(), "task-defs.json")
	if err := os.WriteFile(taskDefsPath, []byte(`[{"name":"web","image":"000000000000.dkr.ecr.us-east-1.amazonaws.com/demo/app:latest","essential":true,"portMappings":[{"containerPort":8080,"protocol":"tcp"}]}]`), 0o644); err != nil {
		t.Fatalf("write ecs container definitions: %v", err)
	}
	registerOut := runAWS(t, h.baseURL, "ecs", "register-task-definition", "--family", "demo-task", "--container-definitions", "file://"+taskDefsPath, "--requires-compatibilities", "FARGATE", "--network-mode", "awsvpc", "--cpu", "256", "--memory", "512", "--output", "json")
	if !strings.Contains(string(registerOut), "taskDefinitionArn") {
		t.Fatalf("expected taskDefinitionArn in register-task-definition output: %s", registerOut)
	}

	lbOut := runAWS(t, h.baseURL, "elbv2", "create-load-balancer", "--name", "demo-alb", "--subnets", "subnet-a", "subnet-b", "--type", "application", "--output", "json")
	var lb struct {
		LoadBalancers []struct {
			LoadBalancerArn string `json:"LoadBalancerArn"`
		} `json:"LoadBalancers"`
	}
	if err := json.Unmarshal(lbOut, &lb); err != nil {
		t.Fatalf("decode create-load-balancer output: %v\n%s", err, lbOut)
	}
	tgOut := runAWS(t, h.baseURL, "elbv2", "create-target-group", "--name", "demo-tg", "--protocol", "HTTP", "--port", "8080", "--vpc-id", "vpc-12345678", "--target-type", "ip", "--output", "json")
	var tg struct {
		TargetGroups []struct {
			TargetGroupArn string `json:"TargetGroupArn"`
		} `json:"TargetGroups"`
	}
	if err := json.Unmarshal(tgOut, &tg); err != nil {
		t.Fatalf("decode create-target-group output: %v\n%s", err, tgOut)
	}
	runAWS(t, h.baseURL, "elbv2", "register-targets", "--target-group-arn", tg.TargetGroups[0].TargetGroupArn, "--targets", "Id=10.0.0.10,Port=8080", "--output", "json")
	targetHealthOut := runAWS(t, h.baseURL, "elbv2", "describe-target-health", "--target-group-arn", tg.TargetGroups[0].TargetGroupArn, "--output", "json")
	if !strings.Contains(string(targetHealthOut), "healthy") {
		t.Fatalf("expected healthy target in describe-target-health output: %s", targetHealthOut)
	}
	listenerOut := runAWS(t, h.baseURL, "elbv2", "create-listener", "--load-balancer-arn", lb.LoadBalancers[0].LoadBalancerArn, "--protocol", "HTTP", "--port", "80", "--default-actions", "Type=forward,TargetGroupArn="+tg.TargetGroups[0].TargetGroupArn, "--output", "json")
	if !strings.Contains(string(listenerOut), "ListenerArn") {
		t.Fatalf("expected ListenerArn in create-listener output: %s", listenerOut)
	}

	createServiceOut := runAWS(t, h.baseURL, "ecs", "create-service", "--cluster", "demo-cluster", "--service-name", "web", "--task-definition", "demo-task", "--desired-count", "1", "--load-balancers", "targetGroupArn="+tg.TargetGroups[0].TargetGroupArn+",containerName=web,containerPort=8080", "--output", "json")
	if !strings.Contains(string(createServiceOut), "serviceArn") {
		t.Fatalf("expected serviceArn in create-service output: %s", createServiceOut)
	}
	describeServicesOut := runAWS(t, h.baseURL, "ecs", "describe-services", "--cluster", "demo-cluster", "--services", "web", "--output", "json")
	if !strings.Contains(string(describeServicesOut), "demo-task") {
		t.Fatalf("expected task definition in describe-services output: %s", describeServicesOut)
	}
	runTaskOut := runAWS(t, h.baseURL, "ecs", "run-task", "--cluster", "demo-cluster", "--task-definition", "demo-task", "--launch-type", "FARGATE", "--output", "json")
	if !strings.Contains(string(runTaskOut), "taskArn") {
		t.Fatalf("expected taskArn in run-task output: %s", runTaskOut)
	}
}

func TestAWSCLIACMRDSElastiCacheControlPlane(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	requestOut := runAWS(t, h.baseURL, "acm", "request-certificate", "--domain-name", "demo.stratus.local", "--validation-method", "DNS", "--output", "json")
	var cert struct {
		CertificateArn string `json:"CertificateArn"`
	}
	if err := json.Unmarshal(requestOut, &cert); err != nil {
		t.Fatalf("decode request-certificate output: %v\n%s", err, requestOut)
	}
	describeCertOut := runAWS(t, h.baseURL, "acm", "describe-certificate", "--certificate-arn", cert.CertificateArn, "--output", "json")
	if !strings.Contains(string(describeCertOut), "demo.stratus.local") {
		t.Fatalf("expected domain in describe-certificate output: %s", describeCertOut)
	}
	listCertsOut := runAWS(t, h.baseURL, "acm", "list-certificates", "--output", "json")
	if !strings.Contains(string(listCertsOut), cert.CertificateArn) {
		t.Fatalf("expected certificate arn in list-certificates output: %s", listCertsOut)
	}
	runAWS(t, h.baseURL, "rds", "create-db-subnet-group", "--db-subnet-group-name", "main", "--db-subnet-group-description", "main subnet group", "--subnet-ids", "subnet-a", "subnet-b", "--output", "json")
	subnetsOut := runAWS(t, h.baseURL, "rds", "describe-db-subnet-groups", "--db-subnet-group-name", "main", "--output", "json")
	if !strings.Contains(string(subnetsOut), "main") {
		t.Fatalf("expected subnet group in describe-db-subnet-groups output: %s", subnetsOut)
	}
	runAWS(t, h.baseURL, "rds", "create-db-instance", "--db-instance-identifier", "app-db", "--engine", "postgres", "--db-instance-class", "db.t3.micro", "--allocated-storage", "20", "--master-username", "admin", "--master-user-password", "password123", "--db-subnet-group-name", "main", "--output", "json")
	describeDBOut := runAWS(t, h.baseURL, "rds", "describe-db-instances", "--db-instance-identifier", "app-db", "--output", "json")
	if !strings.Contains(string(describeDBOut), "available") {
		t.Fatalf("expected available db instance in describe-db-instances output: %s", describeDBOut)
	}
	runAWS(t, h.baseURL, "elasticache", "create-cache-cluster", "--cache-cluster-id", "cache-main", "--engine", "redis", "--cache-node-type", "cache.t3.micro", "--num-cache-nodes", "1", "--output", "json")
	describeCacheOut := runAWS(t, h.baseURL, "elasticache", "describe-cache-clusters", "--cache-cluster-id", "cache-main", "--output", "json")
	if !strings.Contains(string(describeCacheOut), "cache-main") {
		t.Fatalf("expected cache cluster in describe-cache-clusters output: %s", describeCacheOut)
	}
	runAWS(t, h.baseURL, "elasticache", "delete-cache-cluster", "--cache-cluster-id", "cache-main", "--output", "json")
	runAWS(t, h.baseURL, "rds", "delete-db-instance", "--db-instance-identifier", "app-db", "--skip-final-snapshot", "--output", "json")
	runAWS(t, h.baseURL, "acm", "delete-certificate", "--certificate-arn", cert.CertificateArn, "--output", "json")
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

func runJavaSDKSmoke(t *testing.T, endpoint string, serverOutput *bytes.Buffer) {
	t.Helper()

	if _, err := exec.LookPath("mvn"); err != nil {
		t.Skip("maven not installed")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not installed")
	}

	fixtureDir := filepath.Join(moduleRoot(t), "test", "fixtures", "java-sdk-smoke")
	cmd := exec.Command("mvn", "-q", "-Dstratus.endpoint="+endpoint, "test")
	cmd.Dir = fixtureDir
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("java sdk smoke failed: %v\nmaven:\n%s\nstratus:\n%s", err, out, serverOutput.String())
	}
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
