package lambda

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
	"github.com/stratus/internal/store/fsblob"
)

const (
	functionsBucket = "lambda-functions"
	codeNamespace   = "lambda"
	accountID       = "000000000000"
)

type Options struct {
	Metadata store.Store
	Blobs    *fsblob.Store
	Invoker  Invoker
}

type Service struct {
	metadata store.Store
	blobs    *fsblob.Store
	invoker  Invoker
	now      func() time.Time
}

type Invoker interface {
	Invoke(ctx context.Context, spec FunctionSpec, payload []byte) (InvokeResult, error)
	CleanupFunction(ctx context.Context, functionName string) error
	Close() error
}

type FunctionSpec struct {
	FunctionName string
	Handler      string
	Runtime      string
	Timeout      int
	MemorySize   int
	Environment  map[string]string
}

type InvokeResult struct {
	Payload       []byte
	Logs          string
	FunctionError string
}

type createFunctionInput struct {
	Architectures           []string          `json:"Architectures"`
	Code                    createCodeInput   `json:"Code"`
	CodeSigningConfigArn    string            `json:"CodeSigningConfigArn"`
	DeadLetterConfig        map[string]any    `json:"DeadLetterConfig"`
	Description             string            `json:"Description"`
	Environment             environmentInput  `json:"Environment"`
	EphemeralStorage        map[string]any    `json:"EphemeralStorage"`
	FileSystemConfigs       []any             `json:"FileSystemConfigs"`
	FunctionName            string            `json:"FunctionName"`
	Handler                 string            `json:"Handler"`
	ImageConfig             map[string]any    `json:"ImageConfig"`
	KMSKeyArn               string            `json:"KMSKeyArn"`
	Layers                  []string          `json:"Layers"`
	LoggingConfig           map[string]any    `json:"LoggingConfig"`
	MemorySize              int               `json:"MemorySize"`
	PackageType             string            `json:"PackageType"`
	Publish                 bool              `json:"Publish"`
	Role                    string            `json:"Role"`
	Runtime                 string            `json:"Runtime"`
	RuntimeManagementConfig map[string]any    `json:"RuntimeManagementConfig"`
	SnapStart               map[string]any    `json:"SnapStart"`
	Tags                    map[string]string `json:"Tags"`
	Timeout                 int               `json:"Timeout"`
	TracingConfig           map[string]any    `json:"TracingConfig"`
	VpcConfig               map[string]any    `json:"VpcConfig"`
}

type createCodeInput struct {
	ImageURI        string `json:"ImageUri"`
	S3Bucket        string `json:"S3Bucket"`
	S3Key           string `json:"S3Key"`
	S3ObjectVersion string `json:"S3ObjectVersion"`
	ZipFile         []byte `json:"ZipFile"`
}

type environmentInput struct {
	Variables map[string]string `json:"Variables"`
}

type functionRecord struct {
	FunctionName  string            `json:"function_name"`
	Description   string            `json:"description"`
	Role          string            `json:"role"`
	Runtime       string            `json:"runtime"`
	Handler       string            `json:"handler"`
	Timeout       int               `json:"timeout"`
	MemorySize    int               `json:"memory_size"`
	Environment   map[string]string `json:"environment"`
	CodeSize      int64             `json:"code_size"`
	CodeSHA256    string            `json:"code_sha256"`
	LastModified  string            `json:"last_modified"`
	RevisionID    string            `json:"revision_id"`
	Version       string            `json:"version"`
	Architectures []string          `json:"architectures"`
	PackageType   string            `json:"package_type"`
}

type functionConfigurationResponse struct {
	Architectures []string          `json:"Architectures,omitempty"`
	CodeSha256    string            `json:"CodeSha256"`
	CodeSize      int64             `json:"CodeSize"`
	Description   string            `json:"Description,omitempty"`
	Environment   environmentOutput `json:"Environment,omitempty"`
	FunctionArn   string            `json:"FunctionArn"`
	FunctionName  string            `json:"FunctionName"`
	Handler       string            `json:"Handler"`
	LastModified  string            `json:"LastModified"`
	MemorySize    int               `json:"MemorySize"`
	PackageType   string            `json:"PackageType,omitempty"`
	RevisionID    string            `json:"RevisionId"`
	Role          string            `json:"Role"`
	Runtime       string            `json:"Runtime"`
	State         string            `json:"State,omitempty"`
	Timeout       int               `json:"Timeout"`
	Version       string            `json:"Version"`
}

type environmentOutput struct {
	Variables map[string]string `json:"Variables,omitempty"`
}

type getFunctionResponse struct {
	Code          codeResponse                  `json:"Code"`
	Configuration functionConfigurationResponse `json:"Configuration"`
}

type codeResponse struct {
	Location       string `json:"Location"`
	RepositoryType string `json:"RepositoryType"`
}

func NewService(opts Options) *Service {
	return &Service{
		metadata: opts.Metadata,
		blobs:    opts.Blobs,
		invoker:  opts.Invoker,
		now:      time.Now,
	}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	switch operation {
	case "CreateFunction":
		return s.createFunction(w, r)
	case "GetFunction":
		return s.getFunction(w, r)
	case "GetFunctionConfiguration":
		return s.getFunctionConfiguration(w, r)
	case "DeleteFunction":
		return s.deleteFunction(w, r)
	case "Invoke":
		return s.invokeFunction(w, r)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "lambda operation is not implemented",
		}
	}
}

func (s *Service) createFunction(w http.ResponseWriter, r *http.Request) error {
	var input createFunctionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	if err := validateCreateInput(input); err != nil {
		return err
	}
	if _, err := s.loadRecord(input.FunctionName); err == nil {
		return &apierror.Error{
			StatusCode: http.StatusConflict,
			Code:       "ResourceConflictException",
			Message:    "Function already exist: " + input.FunctionName,
		}
	}

	result, err := s.blobs.Put(codeNamespace+"/"+input.FunctionName, "code.zip", bytes.NewReader(input.Code.ZipFile))
	if err != nil {
		return internal(err)
	}

	now := s.now().UTC()
	record := functionRecord{
		FunctionName:  input.FunctionName,
		Description:   input.Description,
		Role:          input.Role,
		Runtime:       input.Runtime,
		Handler:       input.Handler,
		Timeout:       defaultInt(input.Timeout, 3),
		MemorySize:    defaultInt(input.MemorySize, 128),
		Environment:   cloneMap(input.Environment.Variables),
		CodeSize:      result.Size,
		CodeSHA256:    result.SHA256Base64,
		LastModified:  now.Format("2006-01-02T15:04:05.000-0700"),
		RevisionID:    uuid.NewString(),
		Version:       "$LATEST",
		Architectures: normalizeArchitectures(input.Architectures),
		PackageType:   "Zip",
	}

	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(functionsBucket, input.FunctionName, raw); err != nil {
		return internal(err)
	}
	if err := s.extractSource(input.FunctionName, input.Code.ZipFile); err != nil {
		return internal(err)
	}

	writeJSON(w, http.StatusCreated, s.configurationResponse(record))
	return nil
}

func (s *Service) getFunction(w http.ResponseWriter, r *http.Request) error {
	record, err := s.recordFromRequest(r)
	if err != nil {
		return err
	}

	resp := getFunctionResponse{
		Code: codeResponse{
			Location:       lambdaCodeLocation(r, record.FunctionName),
			RepositoryType: "Local",
		},
		Configuration: s.configurationResponse(record),
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Service) getFunctionConfiguration(w http.ResponseWriter, r *http.Request) error {
	record, err := s.recordFromRequest(r)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, s.configurationResponse(record))
	return nil
}

func (s *Service) deleteFunction(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromPath(r.URL.Path, false)
	if err != nil {
		return err
	}
	if qualifier := r.URL.Query().Get("Qualifier"); qualifier != "" {
		return notImplemented("function qualifiers are not supported")
	}
	if _, err := s.loadRecord(name); err != nil {
		return err
	}
	if err := s.metadata.Delete(functionsBucket, name); err != nil {
		return internal(err)
	}
	if err := s.blobs.DeleteNamespace(codeNamespace + "/" + name); err != nil {
		return internal(err)
	}
	if s.invoker != nil {
		if err := s.invoker.CleanupFunction(r.Context(), name); err != nil {
			return internal(err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) invokeFunction(w http.ResponseWriter, r *http.Request) error {
	if invocationType := r.Header.Get("X-Amz-Invocation-Type"); invocationType != "" && invocationType != "RequestResponse" {
		return notImplemented("only RequestResponse invocation type is supported")
	}
	if qualifier := r.URL.Query().Get("Qualifier"); qualifier != "" {
		return notImplemented("function qualifiers are not supported")
	}
	if s.invoker == nil {
		return &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "lambda runtime is not configured",
		}
	}

	record, err := s.recordFromRequest(r)
	if err != nil {
		return err
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return badRequest("InvalidRequestContentException", "unable to read invocation payload")
	}

	result, err := s.invoker.Invoke(r.Context(), FunctionSpec{
		FunctionName: record.FunctionName,
		Handler:      record.Handler,
		Runtime:      record.Runtime,
		Timeout:      record.Timeout,
		MemorySize:   record.MemorySize,
		Environment:  cloneMap(record.Environment),
	}, payload)
	if err != nil {
		return err
	}

	if strings.EqualFold(r.Header.Get("X-Amz-Log-Type"), "Tail") && result.Logs != "" {
		w.Header().Set("X-Amz-Log-Result", base64.StdEncoding.EncodeToString(tailLog([]byte(result.Logs), 4096)))
	}
	if result.FunctionError != "" {
		w.Header().Set("X-Amz-Function-Error", result.FunctionError)
	}
	w.Header().Set("X-Amz-Executed-Version", record.Version)
	if json.Valid(result.Payload) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Payload)
	return nil
}

func (s *Service) recordFromRequest(r *http.Request) (functionRecord, error) {
	withConfig := strings.HasSuffix(r.URL.Path, "/configuration")
	name, err := functionNameFromPath(r.URL.Path, withConfig)
	if err != nil {
		return functionRecord{}, err
	}
	if qualifier := r.URL.Query().Get("Qualifier"); qualifier != "" {
		return functionRecord{}, notImplemented("function qualifiers are not supported")
	}
	return s.loadRecord(name)
}

func (s *Service) loadRecord(name string) (functionRecord, error) {
	raw, err := s.metadata.Get(functionsBucket, name)
	if err != nil {
		return functionRecord{}, internal(err)
	}
	if raw == nil {
		return functionRecord{}, &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "ResourceNotFoundException",
			Message:    "Function not found: " + name,
		}
	}
	var record functionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return functionRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) configurationResponse(record functionRecord) functionConfigurationResponse {
	arch := append([]string(nil), record.Architectures...)
	sort.Strings(arch)
	resp := functionConfigurationResponse{
		Architectures: arch,
		CodeSha256:    record.CodeSHA256,
		CodeSize:      record.CodeSize,
		Description:   record.Description,
		Environment: environmentOutput{
			Variables: cloneMap(record.Environment),
		},
		FunctionArn:  lambdaARN(record.FunctionName),
		FunctionName: record.FunctionName,
		Handler:      record.Handler,
		LastModified: record.LastModified,
		MemorySize:   record.MemorySize,
		PackageType:  record.PackageType,
		RevisionID:   record.RevisionID,
		Role:         record.Role,
		Runtime:      record.Runtime,
		State:        "Active",
		Timeout:      record.Timeout,
		Version:      record.Version,
	}
	return resp
}

func validateCreateInput(input createFunctionInput) error {
	if input.FunctionName == "" {
		return badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	if input.Role == "" {
		return badRequest("InvalidParameterValueException", "Role is required")
	}
	if input.Runtime == "" {
		return badRequest("InvalidParameterValueException", "Runtime is required")
	}
	if input.Handler == "" {
		return badRequest("InvalidParameterValueException", "Handler is required")
	}
	if len(input.Code.ZipFile) == 0 {
		return badRequest("InvalidParameterValueException", "ZipFile is required")
	}
	if input.PackageType != "" && input.PackageType != "Zip" {
		return notImplemented("only Zip package type is supported")
	}
	if input.Publish {
		return notImplemented("published versions are not supported")
	}
	if input.Code.ImageURI != "" || input.Code.S3Bucket != "" || input.Code.S3Key != "" || input.Code.S3ObjectVersion != "" {
		return notImplemented("only inline ZipFile uploads are supported")
	}
	if len(input.Layers) > 0 || len(input.FileSystemConfigs) > 0 || len(input.ImageConfig) > 0 ||
		len(input.VpcConfig) > 0 || len(input.DeadLetterConfig) > 0 || len(input.TracingConfig) > 0 ||
		len(input.EphemeralStorage) > 0 || len(input.LoggingConfig) > 0 || len(input.SnapStart) > 0 ||
		len(input.RuntimeManagementConfig) > 0 || input.KMSKeyArn != "" || input.CodeSigningConfigArn != "" ||
		len(input.Tags) > 0 {
		return notImplemented("one or more requested Lambda features are not supported yet")
	}
	if input.Timeout != 0 && input.Timeout < 1 {
		return badRequest("InvalidParameterValueException", "Timeout must be at least 1")
	}
	if input.MemorySize != 0 && input.MemorySize < 128 {
		return badRequest("InvalidParameterValueException", "MemorySize must be at least 128")
	}
	if len(input.Architectures) > 1 {
		return notImplemented("multiple architectures are not supported")
	}
	return nil
}

func functionNameFromPath(p string, withConfiguration bool) (string, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 3 || parts[0] != "2015-03-31" || parts[1] != "functions" {
		return "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Function not found"}
	}
	name := parts[2]
	if name == "" {
		return "", badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	if withConfiguration && (len(parts) != 4 || parts[3] != "configuration") {
		return "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Function not found: " + name}
	}
	if !withConfiguration && len(parts) == 4 && parts[3] != "invocations" {
		return "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Function not found: " + name}
	}
	return name, nil
}

func lambdaARN(name string) string {
	return fmt.Sprintf("arn:aws:lambda:us-east-1:%s:function:%s", accountID, name)
}

func lambdaCodeLocation(r *http.Request, name string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/_stratus/lambda/functions/%s/code", scheme, r.Host, name)
}

func normalizeArchitectures(input []string) []string {
	if len(input) == 0 {
		return []string{"x86_64"}
	}
	out := append([]string(nil), input...)
	sort.Strings(out)
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Service) extractSource(functionName string, zipBytes []byte) error {
	sourceDir := s.blobs.NamespacePath(filepath.Join(codeNamespace, functionName, "source"))
	if err := os.RemoveAll(sourceDir); err != nil {
		return err
	}
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return err
	}

	reader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return err
	}

	for _, file := range reader.File {
		target := filepath.Join(sourceDir, file.Name)
		cleanTarget := filepath.Clean(target)
		if !strings.HasPrefix(cleanTarget, sourceDir+string(os.PathSeparator)) && cleanTarget != sourceDir {
			return fmt.Errorf("invalid zip entry path: %s", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(cleanTarget, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(cleanTarget), 0o755); err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			src.Close()
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			dst.Close()
			src.Close()
			return err
		}
		if err := dst.Close(); err != nil {
			src.Close()
			return err
		}
		if err := src.Close(); err != nil {
			return err
		}
	}
	return nil
}

func tailLog(data []byte, limit int) []byte {
	if len(data) <= limit {
		return data
	}
	return data[len(data)-limit:]
}
