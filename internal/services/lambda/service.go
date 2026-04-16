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
	functionsBucket     = "lambda-functions"
	versionsBucket      = "lambda-versions"
	aliasesBucket       = "lambda-aliases"
	layersBucket        = "lambda-layers"
	mappingsBucket      = "lambda-event-source-mappings"
	invokeConfigsBucket = "lambda-invoke-configs"
	permissionsBucket   = "lambda-permissions"
	codeNamespace       = "lambda"
	accountID           = "000000000000"
)

type Options struct {
	Metadata store.Store
	Blobs    *fsblob.Store
	Invoker  Invoker
}

type Service struct {
	metadata       store.Store
	blobs          *fsblob.Store
	invoker        Invoker
	queuePublisher QueuePublisher
	topicPublisher TopicPublisher
	now            func() time.Time
}

type QueuePublisher interface {
	SendMessageToARN(queueARN, body string) error
}

type TopicPublisher interface {
	PublishToTopic(topicARN, message string) error
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
	LayerDirs    []string
}

type ProvisionInput struct {
	Architectures []string
	CodeZip       []byte
	Description   string
	Environment   map[string]string
	FunctionName  string
	Handler       string
	Layers        []string
	MemorySize    int
	Role          string
	Runtime       string
	Timeout       int
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
	Layers        []string          `json:"layers,omitempty"`
	PackageType   string            `json:"package_type"`
}

type aliasRecord struct {
	AliasName       string `json:"alias_name"`
	Description     string `json:"description"`
	FunctionName    string `json:"function_name"`
	FunctionVersion string `json:"function_version"`
	RevisionID      string `json:"revision_id"`
}

type layerVersionRecord struct {
	Arn                string   `json:"arn"`
	CompatibleRuntimes []string `json:"compatible_runtimes,omitempty"`
	ContentSHA256      string   `json:"content_sha256"`
	ContentSize        int64    `json:"content_size"`
	CreatedDate        string   `json:"created_date"`
	Description        string   `json:"description,omitempty"`
	LayerName          string   `json:"layer_name"`
	LicenseInfo        string   `json:"license_info,omitempty"`
	Version            int      `json:"version"`
}

type eventSourceMappingRecord struct {
	BatchSize        int       `json:"batch_size"`
	CreatedAt        time.Time `json:"created_at"`
	Enabled          bool      `json:"enabled"`
	EventSourceArn   string    `json:"event_source_arn"`
	FunctionName     string    `json:"function_name"`
	FunctionArn      string    `json:"function_arn"`
	LastModified     string    `json:"last_modified"`
	StartingPosition string    `json:"starting_position,omitempty"`
	State            string    `json:"state"`
	UUID             string    `json:"uuid"`
}

type eventInvokeConfigRecord struct {
	FunctionName string `json:"function_name"`
	Qualifier    string `json:"qualifier"`
	MaxEventAge  int    `json:"max_event_age"`
	MaxRetries   int    `json:"max_retries"`
	OnFailureArn string `json:"on_failure_arn,omitempty"`
	OnSuccessArn string `json:"on_success_arn,omitempty"`
}

type permissionRecord struct {
	Action        string `json:"action"`
	FunctionName  string `json:"function_name"`
	Principal     string `json:"principal"`
	Qualifier     string `json:"qualifier,omitempty"`
	RevisionID    string `json:"revision_id"`
	SourceAccount string `json:"source_account,omitempty"`
	SourceARN     string `json:"source_arn,omitempty"`
	StatementID   string `json:"statement_id"`
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

type aliasResponse struct {
	AliasArn        string `json:"AliasArn"`
	Description     string `json:"Description,omitempty"`
	FunctionVersion string `json:"FunctionVersion"`
	Name            string `json:"Name"`
	RevisionID      string `json:"RevisionId"`
}

func NewService(opts Options) *Service {
	return &Service{
		metadata: opts.Metadata,
		blobs:    opts.Blobs,
		invoker:  opts.Invoker,
		now:      time.Now,
	}
}

func (s *Service) SetQueuePublisher(publisher QueuePublisher) {
	s.queuePublisher = publisher
}

func (s *Service) SetTopicPublisher(publisher TopicPublisher) {
	s.topicPublisher = publisher
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
	case "PublishVersion":
		return s.publishVersion(w, r)
	case "ListVersionsByFunction":
		return s.listVersionsByFunction(w, r)
	case "CreateAlias":
		return s.createAlias(w, r)
	case "ListAliases":
		return s.listAliases(w, r)
	case "GetAlias":
		return s.getAlias(w, r)
	case "UpdateAlias":
		return s.updateAlias(w, r)
	case "DeleteAlias":
		return s.deleteAlias(w, r)
	case "PublishLayerVersion":
		return s.publishLayerVersion(w, r)
	case "ListLayerVersions":
		return s.listLayerVersions(w, r)
	case "GetLayerVersion":
		return s.getLayerVersion(w, r)
	case "CreateEventSourceMapping":
		return s.createEventSourceMapping(w, r)
	case "ListEventSourceMappings":
		return s.listEventSourceMappings(w, r)
	case "GetEventSourceMapping":
		return s.getEventSourceMapping(w, r)
	case "DeleteEventSourceMapping":
		return s.deleteEventSourceMapping(w, r)
	case "PutFunctionEventInvokeConfig":
		return s.putFunctionEventInvokeConfig(w, r)
	case "GetFunctionEventInvokeConfig":
		return s.getFunctionEventInvokeConfig(w, r)
	case "DeleteFunctionEventInvokeConfig":
		return s.deleteFunctionEventInvokeConfig(w, r)
	case "GetFunctionCodeSigningConfig":
		return s.getFunctionCodeSigningConfig(w, r)
	case "AddPermission":
		return s.addPermission(w, r)
	case "GetPolicy":
		return s.getPolicy(w, r)
	case "RemovePermission":
		return s.removePermission(w, r)
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
	record, err := s.provisionFunction(ProvisionInput{
		Architectures: input.Architectures,
		CodeZip:       input.Code.ZipFile,
		Description:   input.Description,
		Environment:   input.Environment.Variables,
		FunctionName:  input.FunctionName,
		Handler:       input.Handler,
		Layers:        append([]string(nil), input.Layers...),
		MemorySize:    input.MemorySize,
		Role:          input.Role,
		Runtime:       input.Runtime,
		Timeout:       input.Timeout,
	})
	if err != nil {
		return err
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
	if err := s.Delete(r.Context(), name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) invokeFunction(w http.ResponseWriter, r *http.Request) error {
	if s.invoker == nil {
		return &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "lambda runtime is not configured",
		}
	}

	name, err := functionNameFromPath(r.URL.Path, false)
	if err != nil {
		return err
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return badRequest("InvalidRequestContentException", "unable to read invocation payload")
	}

	qualifier := r.URL.Query().Get("Qualifier")
	invocationType := r.Header.Get("X-Amz-Invocation-Type")
	if invocationType == "" {
		invocationType = "RequestResponse"
	}
	if invocationType == "DryRun" {
		if _, err := s.resolveRecord(name, qualifier); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if invocationType == "Event" {
		record, err := s.resolveRecord(name, qualifier)
		if err != nil {
			return err
		}
		go func(body []byte, record functionRecord) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(record.Timeout+5)*time.Second)
			defer cancel()
			result, err := s.invoker.Invoke(ctx, s.specFromRecord(record), body)
			s.deliverAsyncResult(record, body, result, err)
		}(append([]byte(nil), payload...), record)
		w.Header().Set("X-Amz-Executed-Version", record.Version)
		w.WriteHeader(http.StatusAccepted)
		return nil
	}
	if invocationType != "RequestResponse" {
		return notImplemented("only RequestResponse, Event, and DryRun invocation types are supported")
	}

	record, result, err := s.invokeByName(r.Context(), name, qualifier, payload)
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

func (s *Service) publishVersion(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromPath(strings.TrimSuffix(r.URL.Path, "/versions"), false)
	if err != nil {
		return err
	}
	record, err := s.loadRecord(name)
	if err != nil {
		return err
	}
	version, err := s.nextVersion(name)
	if err != nil {
		return err
	}
	record.Version = version
	record.RevisionID = uuid.NewString()
	record.LastModified = s.now().UTC().Format("2006-01-02T15:04:05.000-0700")
	if err := s.putVersionRecord(record); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.configurationResponse(record))
	return nil
}

func (s *Service) listVersionsByFunction(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromPath(strings.TrimSuffix(r.URL.Path, "/versions"), false)
	if err != nil {
		return err
	}
	base, err := s.loadRecord(name)
	if err != nil {
		return err
	}
	versions, err := s.listVersionRecords(name)
	if err != nil {
		return err
	}
	items := make([]functionConfigurationResponse, 0, len(versions)+1)
	items = append(items, s.configurationResponse(base))
	for _, version := range versions {
		items = append(items, s.configurationResponse(version))
	}
	writeJSON(w, http.StatusOK, map[string]any{"Versions": items})
	return nil
}

func (s *Service) createAlias(w http.ResponseWriter, r *http.Request) error {
	name, aliasName, err := aliasPathParts(r.URL.Path, false)
	if err != nil {
		return err
	}
	var input struct {
		Name            string `json:"Name"`
		FunctionVersion string `json:"FunctionVersion"`
		Description     string `json:"Description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	if aliasName == "" {
		aliasName = input.Name
	}
	alias, err := s.saveAlias(name, aliasName, input.FunctionVersion, input.Description, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, aliasResponseForRecord(alias))
	return nil
}

func (s *Service) listAliases(w http.ResponseWriter, r *http.Request) error {
	name, _, err := aliasPathParts(r.URL.Path, false)
	if err != nil {
		return err
	}
	aliases, err := s.listAliasRecords(name)
	if err != nil {
		return err
	}
	items := make([]aliasResponse, 0, len(aliases))
	for _, alias := range aliases {
		items = append(items, aliasResponseForRecord(alias))
	}
	writeJSON(w, http.StatusOK, map[string]any{"Aliases": items})
	return nil
}

func (s *Service) getAlias(w http.ResponseWriter, r *http.Request) error {
	name, aliasName, err := aliasPathParts(r.URL.Path, true)
	if err != nil {
		return err
	}
	alias, err := s.loadAlias(name, aliasName)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, aliasResponseForRecord(alias))
	return nil
}

func (s *Service) updateAlias(w http.ResponseWriter, r *http.Request) error {
	name, aliasName, err := aliasPathParts(r.URL.Path, true)
	if err != nil {
		return err
	}
	var input struct {
		FunctionVersion string `json:"FunctionVersion"`
		Description     string `json:"Description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	alias, err := s.saveAlias(name, aliasName, input.FunctionVersion, input.Description, true)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, aliasResponseForRecord(alias))
	return nil
}

func (s *Service) deleteAlias(w http.ResponseWriter, r *http.Request) error {
	name, aliasName, err := aliasPathParts(r.URL.Path, true)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(aliasesBucket, aliasKey(name, aliasName)); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) publishLayerVersion(w http.ResponseWriter, r *http.Request) error {
	layerName, version, err := s.createLayerVersion(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.layerVersionResponse(layerName, version))
	return nil
}

func (s *Service) listLayerVersions(w http.ResponseWriter, r *http.Request) error {
	layerName, _, err := layerNameFromPath(r.URL.Path, false)
	if err != nil {
		return err
	}
	versions, err := s.listLayerVersionRecords(layerName)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(versions))
	for _, version := range versions {
		items = append(items, s.layerVersionResponse(layerName, version))
	}
	writeJSON(w, http.StatusOK, map[string]any{"LayerVersions": items})
	return nil
}

func (s *Service) getLayerVersion(w http.ResponseWriter, r *http.Request) error {
	layerName, versionNumber, err := layerNameFromPath(r.URL.Path, true)
	if err != nil {
		return err
	}
	version, err := s.loadLayerVersion(layerName, versionNumber)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.layerVersionResponse(layerName, version))
	return nil
}

func (s *Service) createEventSourceMapping(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		BatchSize        int    `json:"BatchSize"`
		Enabled          *bool  `json:"Enabled"`
		EventSourceArn   string `json:"EventSourceArn"`
		FunctionName     string `json:"FunctionName"`
		StartingPosition string `json:"StartingPosition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	mapping, err := s.createEventSourceMappingRecord(input.FunctionName, input.EventSourceArn, input.BatchSize, input.StartingPosition, input.Enabled)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, eventSourceMappingResponse(mapping))
	return nil
}

func (s *Service) listEventSourceMappings(w http.ResponseWriter, r *http.Request) error {
	functionName := r.URL.Query().Get("FunctionName")
	eventSourceArn := r.URL.Query().Get("EventSourceArn")
	mappings, err := s.listEventSourceMappingRecords(functionName, eventSourceArn)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(mappings))
	for _, mapping := range mappings {
		items = append(items, eventSourceMappingResponse(mapping))
	}
	writeJSON(w, http.StatusOK, map[string]any{"EventSourceMappings": items})
	return nil
}

func (s *Service) getEventSourceMapping(w http.ResponseWriter, r *http.Request) error {
	uuidValue, err := mappingUUIDFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	mapping, err := s.loadEventSourceMapping(uuidValue)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, eventSourceMappingResponse(mapping))
	return nil
}

func (s *Service) deleteEventSourceMapping(w http.ResponseWriter, r *http.Request) error {
	uuidValue, err := mappingUUIDFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	mapping, err := s.loadEventSourceMapping(uuidValue)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(mappingsBucket, uuidValue); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, eventSourceMappingResponse(mapping))
	return nil
}

func (s *Service) putFunctionEventInvokeConfig(w http.ResponseWriter, r *http.Request) error {
	name, qualifier, err := functionNameAndQualifierFromInvokeConfigPath(r.URL.Path)
	if err != nil {
		return err
	}
	if _, err := s.resolveRecord(name, qualifier); err != nil {
		return err
	}
	var input struct {
		DestinationConfig struct {
			OnFailure struct {
				Destination string `json:"Destination"`
			} `json:"OnFailure"`
			OnSuccess struct {
				Destination string `json:"Destination"`
			} `json:"OnSuccess"`
		} `json:"DestinationConfig"`
		MaximumEventAgeInSeconds int `json:"MaximumEventAgeInSeconds"`
		MaximumRetryAttempts     int `json:"MaximumRetryAttempts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	record := eventInvokeConfigRecord{
		FunctionName: name,
		Qualifier:    qualifier,
		MaxEventAge:  input.MaximumEventAgeInSeconds,
		MaxRetries:   input.MaximumRetryAttempts,
		OnFailureArn: input.DestinationConfig.OnFailure.Destination,
		OnSuccessArn: input.DestinationConfig.OnSuccess.Destination,
	}
	if record.MaxEventAge == 0 {
		record.MaxEventAge = 21600
	}
	if record.MaxRetries == 0 && input.MaximumRetryAttempts == 0 {
		record.MaxRetries = 2
	}
	if err := s.saveEventInvokeConfig(record); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, eventInvokeConfigResponse(record))
	return nil
}

func (s *Service) getFunctionEventInvokeConfig(w http.ResponseWriter, r *http.Request) error {
	name, qualifier, err := functionNameAndQualifierFromInvokeConfigPath(r.URL.Path)
	if err != nil {
		return err
	}
	record, err := s.loadEventInvokeConfig(name, qualifier)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, eventInvokeConfigResponse(record))
	return nil
}

func (s *Service) deleteFunctionEventInvokeConfig(w http.ResponseWriter, r *http.Request) error {
	name, qualifier, err := functionNameAndQualifierFromInvokeConfigPath(r.URL.Path)
	if err != nil {
		return err
	}
	record, err := s.loadEventInvokeConfig(name, qualifier)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(invokeConfigsBucket, eventInvokeConfigKey(name, qualifier)); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, eventInvokeConfigResponse(record))
	return nil
}

func (s *Service) getFunctionCodeSigningConfig(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromCodeSigningConfigPath(r.URL.Path)
	if err != nil {
		return err
	}
	if _, err := s.loadRecord(name); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"CodeSigningConfigArn": "",
		"FunctionName":         name,
	})
	return nil
}

func (s *Service) addPermission(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromPolicyPath(r.URL.Path)
	if err != nil {
		return err
	}
	qualifier := r.URL.Query().Get("Qualifier")
	if _, err := s.resolveRecord(name, qualifier); err != nil {
		return err
	}

	var input struct {
		Action        string `json:"Action"`
		Principal     string `json:"Principal"`
		SourceARN     string `json:"SourceArn"`
		SourceAccount string `json:"SourceAccount"`
		StatementID   string `json:"StatementId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	if input.StatementID == "" {
		return badRequest("InvalidParameterValueException", "StatementId is required")
	}
	if input.Action == "" {
		return badRequest("InvalidParameterValueException", "Action is required")
	}
	if input.Principal == "" {
		return badRequest("InvalidParameterValueException", "Principal is required")
	}

	record := permissionRecord{
		Action:        input.Action,
		FunctionName:  name,
		Principal:     input.Principal,
		Qualifier:     qualifier,
		RevisionID:    uuid.NewString(),
		SourceAccount: input.SourceAccount,
		SourceARN:     input.SourceARN,
		StatementID:   input.StatementID,
	}
	if err := s.putPermissionRecord(record); err != nil {
		return err
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"Statement": permissionStatementJSON(record),
	})
	return nil
}

func (s *Service) getPolicy(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromPolicyPath(r.URL.Path)
	if err != nil {
		return err
	}
	qualifier := r.URL.Query().Get("Qualifier")
	if _, err := s.resolveRecord(name, qualifier); err != nil {
		return err
	}

	records, err := s.listPermissionRecords(name, qualifier)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "ResourceNotFoundException",
			Message:    "The resource you requested does not exist.",
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"Policy":     permissionPolicyJSON(records),
		"RevisionId": records[0].RevisionID,
	})
	return nil
}

func (s *Service) removePermission(w http.ResponseWriter, r *http.Request) error {
	name, statementID, err := functionNameAndStatementIDFromPolicyPath(r.URL.Path)
	if err != nil {
		return err
	}
	qualifier := r.URL.Query().Get("Qualifier")
	if _, err := s.resolveRecord(name, qualifier); err != nil {
		return err
	}
	if _, err := s.loadPermissionRecord(name, qualifier, statementID); err != nil {
		return err
	}
	if err := s.metadata.Delete(permissionsBucket, permissionKey(name, qualifier, statementID)); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) DispatchSQSEvent(ctx context.Context, queueARN string, messageID, receiptHandle, body string, attributes map[string]string) {
	event := map[string]any{
		"Records": []map[string]any{{
			"messageId":      messageID,
			"receiptHandle":  receiptHandle,
			"body":           body,
			"attributes":     attributes,
			"eventSource":    "aws:sqs",
			"eventSourceARN": queueARN,
			"awsRegion":      "us-east-1",
		}},
	}
	s.dispatchEventSource(ctx, queueARN, event)
}

func (s *Service) DispatchKinesisRecords(ctx context.Context, streamARN string, records []map[string]any) {
	if len(records) == 0 {
		return
	}
	s.dispatchEventSource(ctx, streamARN, map[string]any{"Records": records})
}

func (s *Service) DispatchDynamoDBRecords(ctx context.Context, streamARN string, records []map[string]any) {
	if len(records) == 0 {
		return
	}
	s.dispatchEventSource(ctx, streamARN, map[string]any{"Records": records})
}

func (s *Service) Provision(input ProvisionInput) (string, error) {
	record, err := s.provisionFunction(input)
	if err != nil {
		return "", err
	}
	return lambdaARN(record.FunctionName), nil
}

func (s *Service) Delete(ctx context.Context, name string) error {
	if _, err := s.loadRecord(name); err != nil {
		return err
	}
	if err := s.metadata.Delete(functionsBucket, name); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(versionsBucket, name+"|"); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(aliasesBucket, name+"|"); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(permissionsBucket, permissionPrefix(name, "")); err != nil {
		return internal(err)
	}
	mappings, err := s.listEventSourceMappingRecords(name, "")
	if err != nil {
		return err
	}
	for _, mapping := range mappings {
		if err := s.metadata.Delete(mappingsBucket, mapping.UUID); err != nil {
			return internal(err)
		}
	}
	if err := s.blobs.DeleteNamespace(codeNamespace + "/" + name); err != nil {
		return internal(err)
	}
	if s.invoker != nil {
		if err := s.invoker.CleanupFunction(ctx, name); err != nil {
			return internal(err)
		}
	}
	return nil
}

func (s *Service) CreateEventSourceMappingRecord(functionNameOrArn, eventSourceArn string, batchSize int, startingPosition string, enabled *bool) (string, error) {
	mapping, err := s.createEventSourceMappingRecord(functionNameOrArn, eventSourceArn, batchSize, startingPosition, enabled)
	if err != nil {
		return "", err
	}
	return mapping.UUID, nil
}

func (s *Service) DeleteEventSourceMappingByID(uuidValue string) error {
	if _, err := s.loadEventSourceMapping(uuidValue); err != nil {
		return err
	}
	if err := s.metadata.Delete(mappingsBucket, uuidValue); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) InvokeByName(ctx context.Context, name string, payload []byte) (InvokeResult, error) {
	_, result, err := s.invokeByName(ctx, name, "", payload)
	return result, err
}

func (s *Service) InvokeAsyncByName(ctx context.Context, name string, payload []byte) error {
	record, err := s.loadRecord(name)
	if err != nil {
		return err
	}
	go func(body []byte, record functionRecord) {
		callCtx, cancel := context.WithTimeout(context.Background(), time.Duration(record.Timeout+5)*time.Second)
		defer cancel()
		result, err := s.invoker.Invoke(callCtx, s.specFromRecord(record), body)
		s.deliverAsyncResult(record, body, result, err)
	}(append([]byte(nil), payload...), record)
	return nil
}

func FunctionARN(name string) string {
	return lambdaARN(name)
}

func (s *Service) recordFromRequest(r *http.Request) (functionRecord, error) {
	withConfig := strings.HasSuffix(r.URL.Path, "/configuration")
	name, err := functionNameFromPath(r.URL.Path, withConfig)
	if err != nil {
		return functionRecord{}, err
	}
	return s.resolveRecord(name, r.URL.Query().Get("Qualifier"))
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

func (s *Service) loadVersionRecord(name, version string) (functionRecord, error) {
	if version == "" || version == "$LATEST" {
		return s.loadRecord(name)
	}
	raw, err := s.metadata.Get(versionsBucket, versionKey(name, version))
	if err != nil {
		return functionRecord{}, internal(err)
	}
	if raw == nil {
		return functionRecord{}, &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "ResourceNotFoundException",
			Message:    "Function not found: " + name + ":" + version,
		}
	}
	var record functionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return functionRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadAlias(name, aliasName string) (aliasRecord, error) {
	raw, err := s.metadata.Get(aliasesBucket, aliasKey(name, aliasName))
	if err != nil {
		return aliasRecord{}, internal(err)
	}
	if raw == nil {
		return aliasRecord{}, &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "ResourceNotFoundException",
			Message:    "Alias not found: " + aliasName,
		}
	}
	var alias aliasRecord
	if err := json.Unmarshal(raw, &alias); err != nil {
		return aliasRecord{}, internal(err)
	}
	return alias, nil
}

func (s *Service) putPermissionRecord(record permissionRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(permissionsBucket, permissionKey(record.FunctionName, record.Qualifier, record.StatementID), payload); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) loadPermissionRecord(name, qualifier, statementID string) (permissionRecord, error) {
	raw, err := s.metadata.Get(permissionsBucket, permissionKey(name, qualifier, statementID))
	if err != nil {
		return permissionRecord{}, internal(err)
	}
	if raw == nil {
		return permissionRecord{}, &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "ResourceNotFoundException",
			Message:    "The resource you requested does not exist.",
		}
	}
	var record permissionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return permissionRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) listPermissionRecords(name, qualifier string) ([]permissionRecord, error) {
	records := make([]permissionRecord, 0)
	if err := s.metadata.Scan(permissionsBucket, permissionPrefix(name, qualifier), func(_, v []byte) error {
		var record permissionRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return err
		}
		records = append(records, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].StatementID < records[j].StatementID
	})
	return records, nil
}

func (s *Service) resolveRecord(name, qualifier string) (functionRecord, error) {
	if qualifier == "" || qualifier == "$LATEST" {
		return s.loadRecord(name)
	}
	if isNumeric(qualifier) {
		return s.loadVersionRecord(name, qualifier)
	}
	alias, err := s.loadAlias(name, qualifier)
	if err != nil {
		return functionRecord{}, err
	}
	return s.loadVersionRecord(name, alias.FunctionVersion)
}

func (s *Service) provisionFunction(input ProvisionInput) (functionRecord, error) {
	createInput := createFunctionInput{
		Architectures: input.Architectures,
		Code: createCodeInput{
			ZipFile: input.CodeZip,
		},
		Description: input.Description,
		Environment: environmentInput{
			Variables: cloneMap(input.Environment),
		},
		FunctionName: input.FunctionName,
		Handler:      input.Handler,
		Layers:       append([]string(nil), input.Layers...),
		MemorySize:   input.MemorySize,
		Role:         input.Role,
		Runtime:      input.Runtime,
		Timeout:      input.Timeout,
	}
	if err := validateCreateInput(createInput); err != nil {
		return functionRecord{}, err
	}
	if err := s.validateLayers(createInput.Layers); err != nil {
		return functionRecord{}, err
	}
	if _, err := s.loadRecord(createInput.FunctionName); err == nil {
		return functionRecord{}, &apierror.Error{
			StatusCode: http.StatusConflict,
			Code:       "ResourceConflictException",
			Message:    "Function already exist: " + createInput.FunctionName,
		}
	}

	result, err := s.blobs.Put(codeNamespace+"/"+createInput.FunctionName, "code.zip", bytes.NewReader(createInput.Code.ZipFile))
	if err != nil {
		return functionRecord{}, internal(err)
	}

	now := s.now().UTC()
	record := functionRecord{
		FunctionName:  createInput.FunctionName,
		Description:   createInput.Description,
		Role:          createInput.Role,
		Runtime:       createInput.Runtime,
		Handler:       createInput.Handler,
		Timeout:       defaultInt(createInput.Timeout, 3),
		MemorySize:    defaultInt(createInput.MemorySize, 128),
		Environment:   cloneMap(createInput.Environment.Variables),
		CodeSize:      result.Size,
		CodeSHA256:    result.SHA256Base64,
		LastModified:  now.Format("2006-01-02T15:04:05.000-0700"),
		RevisionID:    uuid.NewString(),
		Version:       "$LATEST",
		Architectures: normalizeArchitectures(createInput.Architectures),
		Layers:        append([]string(nil), createInput.Layers...),
		PackageType:   "Zip",
	}

	raw, err := json.Marshal(record)
	if err != nil {
		return functionRecord{}, internal(err)
	}
	if err := s.metadata.Put(functionsBucket, createInput.FunctionName, raw); err != nil {
		return functionRecord{}, internal(err)
	}
	if err := s.extractSource(createInput.FunctionName, createInput.Code.ZipFile); err != nil {
		return functionRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putVersionRecord(record functionRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(versionsBucket, versionKey(record.FunctionName, record.Version), raw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) nextVersion(name string) (string, error) {
	maxVersion := 0
	if err := s.metadata.Scan(versionsBucket, name+"|", func(k, _ []byte) error {
		parts := strings.Split(string(k), "|")
		if len(parts) != 2 {
			return nil
		}
		if version := parts[1]; isNumeric(version) {
			if parsed := atoiDefault(version, 0); parsed > maxVersion {
				maxVersion = parsed
			}
		}
		return nil
	}); err != nil {
		return "", internal(err)
	}
	return fmt.Sprintf("%d", maxVersion+1), nil
}

func (s *Service) listVersionRecords(name string) ([]functionRecord, error) {
	var items []functionRecord
	if err := s.metadata.Scan(versionsBucket, name+"|", func(_, v []byte) error {
		var record functionRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool {
		return atoiDefault(items[i].Version, 0) < atoiDefault(items[j].Version, 0)
	})
	return items, nil
}

func (s *Service) saveAlias(name, aliasName, functionVersion, description string, requireExisting bool) (aliasRecord, error) {
	if aliasName == "" {
		return aliasRecord{}, badRequest("InvalidParameterValueException", "Alias name is required")
	}
	if functionVersion == "" {
		return aliasRecord{}, badRequest("InvalidParameterValueException", "FunctionVersion is required")
	}
	if _, err := s.resolveRecord(name, functionVersion); err != nil {
		return aliasRecord{}, err
	}
	if requireExisting {
		if _, err := s.loadAlias(name, aliasName); err != nil {
			return aliasRecord{}, err
		}
	} else {
		if _, err := s.loadAlias(name, aliasName); err == nil {
			return aliasRecord{}, &apierror.Error{
				StatusCode: http.StatusConflict,
				Code:       "ResourceConflictException",
				Message:    "Alias already exists: " + aliasName,
			}
		}
	}
	alias := aliasRecord{
		AliasName:       aliasName,
		Description:     description,
		FunctionName:    name,
		FunctionVersion: functionVersion,
		RevisionID:      uuid.NewString(),
	}
	raw, err := json.Marshal(alias)
	if err != nil {
		return aliasRecord{}, internal(err)
	}
	if err := s.metadata.Put(aliasesBucket, aliasKey(name, aliasName), raw); err != nil {
		return aliasRecord{}, internal(err)
	}
	return alias, nil
}

func (s *Service) listAliasRecords(name string) ([]aliasRecord, error) {
	var items []aliasRecord
	if err := s.metadata.Scan(aliasesBucket, name+"|", func(_, v []byte) error {
		var alias aliasRecord
		if err := json.Unmarshal(v, &alias); err != nil {
			return nil
		}
		items = append(items, alias)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].AliasName < items[j].AliasName
	})
	return items, nil
}

func (s *Service) createLayerVersion(r *http.Request) (string, layerVersionRecord, error) {
	layerName, _, err := layerNameFromPath(r.URL.Path, false)
	if err != nil {
		return "", layerVersionRecord{}, err
	}
	var input struct {
		CompatibleRuntimes []string `json:"CompatibleRuntimes"`
		Content            struct {
			ZipFile []byte `json:"ZipFile"`
		} `json:"Content"`
		Description string `json:"Description"`
		LicenseInfo string `json:"LicenseInfo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return "", layerVersionRecord{}, badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	if layerName == "" {
		return "", layerVersionRecord{}, badRequest("InvalidParameterValueException", "LayerName is required")
	}
	if len(input.Content.ZipFile) == 0 {
		return "", layerVersionRecord{}, badRequest("InvalidParameterValueException", "ZipFile is required")
	}
	nextVersion, err := s.nextLayerVersion(layerName)
	if err != nil {
		return "", layerVersionRecord{}, err
	}
	result, err := s.blobs.Put(layerNamespace(layerName, nextVersion), "layer.zip", bytes.NewReader(input.Content.ZipFile))
	if err != nil {
		return "", layerVersionRecord{}, internal(err)
	}
	if err := s.extractLayerSource(layerName, nextVersion, input.Content.ZipFile); err != nil {
		return "", layerVersionRecord{}, internal(err)
	}
	record := layerVersionRecord{
		Arn:                layerVersionARN(layerName, nextVersion),
		CompatibleRuntimes: append([]string(nil), input.CompatibleRuntimes...),
		ContentSHA256:      result.SHA256Base64,
		ContentSize:        result.Size,
		CreatedDate:        s.now().UTC().Format(time.RFC3339),
		Description:        input.Description,
		LayerName:          layerName,
		LicenseInfo:        input.LicenseInfo,
		Version:            nextVersion,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", layerVersionRecord{}, internal(err)
	}
	if err := s.metadata.Put(layersBucket, layerVersionKey(layerName, nextVersion), raw); err != nil {
		return "", layerVersionRecord{}, internal(err)
	}
	return layerName, record, nil
}

func (s *Service) nextLayerVersion(layerName string) (int, error) {
	versions, err := s.listLayerVersionRecords(layerName)
	if err != nil {
		return 0, err
	}
	if len(versions) == 0 {
		return 1, nil
	}
	return versions[len(versions)-1].Version + 1, nil
}

func (s *Service) listLayerVersionRecords(layerName string) ([]layerVersionRecord, error) {
	var items []layerVersionRecord
	if err := s.metadata.Scan(layersBucket, layerName+"|", func(_, v []byte) error {
		var record layerVersionRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Version < items[j].Version })
	return items, nil
}

func (s *Service) loadLayerVersion(layerName string, version int) (layerVersionRecord, error) {
	raw, err := s.metadata.Get(layersBucket, layerVersionKey(layerName, version))
	if err != nil {
		return layerVersionRecord{}, internal(err)
	}
	if raw == nil {
		return layerVersionRecord{}, &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "ResourceNotFoundException",
			Message:    "Layer version not found",
		}
	}
	var record layerVersionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return layerVersionRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) validateLayers(layers []string) error {
	for _, layerArn := range layers {
		layerName, version, err := parseLayerVersionARN(layerArn)
		if err != nil {
			return err
		}
		if _, err := s.loadLayerVersion(layerName, version); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) resolveLayerDirs(layers []string) []string {
	out := make([]string, 0, len(layers))
	for _, layerArn := range layers {
		layerName, version, err := parseLayerVersionARN(layerArn)
		if err != nil {
			continue
		}
		out = append(out, s.blobs.NamespacePath(layerNamespace(layerName, version)+"/source"))
	}
	return out
}

func (s *Service) createEventSourceMappingRecord(functionNameOrArn, eventSourceArn string, batchSize int, startingPosition string, enabled *bool) (eventSourceMappingRecord, error) {
	record, err := s.resolveFunctionFromReference(functionNameOrArn)
	if err != nil {
		return eventSourceMappingRecord{}, err
	}
	if eventSourceArn == "" {
		return eventSourceMappingRecord{}, badRequest("InvalidParameterValueException", "EventSourceArn is required")
	}
	if batchSize <= 0 {
		batchSize = 1
	}
	stateEnabled := true
	if enabled != nil {
		stateEnabled = *enabled
	}
	if strings.Contains(eventSourceArn, ":stream/") && startingPosition == "" {
		return eventSourceMappingRecord{}, badRequest("InvalidParameterValueException", "StartingPosition is required for stream event sources")
	}
	now := s.now().UTC()
	mapping := eventSourceMappingRecord{
		BatchSize:        batchSize,
		CreatedAt:        now,
		Enabled:          stateEnabled,
		EventSourceArn:   eventSourceArn,
		FunctionArn:      lambdaARN(record.FunctionName),
		FunctionName:     record.FunctionName,
		LastModified:     now.Format(time.RFC3339),
		StartingPosition: startingPosition,
		State:            ternary(stateEnabled, "Enabled", "Disabled"),
		UUID:             uuid.NewString(),
	}
	raw, err := json.Marshal(mapping)
	if err != nil {
		return eventSourceMappingRecord{}, internal(err)
	}
	if err := s.metadata.Put(mappingsBucket, mapping.UUID, raw); err != nil {
		return eventSourceMappingRecord{}, internal(err)
	}
	return mapping, nil
}

func (s *Service) listEventSourceMappingRecords(functionName, eventSourceArn string) ([]eventSourceMappingRecord, error) {
	var items []eventSourceMappingRecord
	if err := s.metadata.Scan(mappingsBucket, "", func(_, v []byte) error {
		var record eventSourceMappingRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if functionName != "" && functionName != record.FunctionName && functionName != record.FunctionArn {
			return nil
		}
		if eventSourceArn != "" && eventSourceArn != record.EventSourceArn {
			return nil
		}
		items = append(items, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return items, nil
}

func (s *Service) loadEventSourceMapping(uuidValue string) (eventSourceMappingRecord, error) {
	raw, err := s.metadata.Get(mappingsBucket, uuidValue)
	if err != nil {
		return eventSourceMappingRecord{}, internal(err)
	}
	if raw == nil {
		return eventSourceMappingRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Event source mapping not found"}
	}
	var record eventSourceMappingRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return eventSourceMappingRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadEventInvokeConfig(name, qualifier string) (eventInvokeConfigRecord, error) {
	raw, err := s.metadata.Get(invokeConfigsBucket, eventInvokeConfigKey(name, qualifier))
	if err != nil {
		return eventInvokeConfigRecord{}, internal(err)
	}
	if raw == nil {
		return eventInvokeConfigRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Function event invoke config not found"}
	}
	var record eventInvokeConfigRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return eventInvokeConfigRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) saveEventInvokeConfig(record eventInvokeConfigRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(invokeConfigsBucket, eventInvokeConfigKey(record.FunctionName, record.Qualifier), raw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) dispatchEventSource(ctx context.Context, eventSourceArn string, payload map[string]any) {
	mappings, err := s.listEventSourceMappingRecords("", eventSourceArn)
	if err != nil || len(mappings) == 0 {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	for _, mapping := range mappings {
		if !mapping.Enabled {
			continue
		}
		go func(mapping eventSourceMappingRecord, body []byte) {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, _, _ = s.invokeByName(callCtx, mapping.FunctionName, "", append([]byte(nil), body...))
		}(mapping, body)
	}
}

func (s *Service) deliverAsyncResult(record functionRecord, requestPayload []byte, result InvokeResult, invokeErr error) {
	config, err := s.loadEventInvokeConfig(record.FunctionName, record.Version)
	if err != nil {
		config, err = s.loadEventInvokeConfig(record.FunctionName, "$LATEST")
		if err != nil {
			return
		}
	}
	success := invokeErr == nil && result.FunctionError == ""
	destination := config.OnFailureArn
	if success {
		destination = config.OnSuccessArn
	}
	if destination == "" {
		return
	}
	message, err := json.Marshal(map[string]any{
		"requestContext": map[string]any{
			"condition":   successCondition(success),
			"functionArn": lambdaARN(record.FunctionName),
		},
		"requestPayload": json.RawMessage(nonEmptyJSON(requestPayload)),
		"responseContext": map[string]any{
			"functionError": result.FunctionError,
			"statusCode":    ternaryInt(success, 200, 500),
		},
		"responsePayload": json.RawMessage(nonEmptyJSON(result.Payload)),
	})
	if err != nil {
		return
	}
	switch {
	case strings.Contains(destination, ":sqs:") && s.queuePublisher != nil:
		_ = s.queuePublisher.SendMessageToARN(destination, string(message))
	case strings.Contains(destination, ":sns:") && s.topicPublisher != nil:
		_ = s.topicPublisher.PublishToTopic(destination, string(message))
	}
}

func (s *Service) resolveFunctionFromReference(ref string) (functionRecord, error) {
	if ref == "" {
		return functionRecord{}, badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	if strings.Contains(ref, ":function:") {
		name, qualifier := functionRefParts(ref)
		return s.resolveRecord(name, qualifier)
	}
	return s.loadRecord(ref)
}

func (s *Service) invokeByName(ctx context.Context, name, qualifier string, payload []byte) (functionRecord, InvokeResult, error) {
	if s.invoker == nil {
		return functionRecord{}, InvokeResult{}, &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "lambda runtime is not configured",
		}
	}
	record, err := s.resolveRecord(name, qualifier)
	if err != nil {
		return functionRecord{}, InvokeResult{}, err
	}
	result, err := s.invoker.Invoke(ctx, s.specFromRecord(record), payload)
	if err != nil {
		return functionRecord{}, InvokeResult{}, err
	}
	return record, result, nil
}

func (s *Service) specFromRecord(record functionRecord) FunctionSpec {
	return FunctionSpec{
		FunctionName: record.FunctionName,
		Handler:      record.Handler,
		Runtime:      record.Runtime,
		Timeout:      record.Timeout,
		MemorySize:   record.MemorySize,
		Environment:  cloneMap(record.Environment),
		LayerDirs:    s.resolveLayerDirs(record.Layers),
	}
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
		FunctionArn:  qualifiedFunctionARN(record.FunctionName, record.Version),
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

func (s *Service) layerVersionResponse(layerName string, record layerVersionRecord) map[string]any {
	return map[string]any{
		"CompatibleRuntimes": record.CompatibleRuntimes,
		"Content": map[string]any{
			"CodeSha256": record.ContentSHA256,
			"CodeSize":   record.ContentSize,
			"Location":   fmt.Sprintf("file://%s", s.blobs.NamespacePath(layerNamespace(layerName, record.Version)+"/layer.zip")),
		},
		"CreatedDate":     record.CreatedDate,
		"Description":     record.Description,
		"LayerArn":        fmt.Sprintf("arn:aws:lambda:%s:%s:layer:%s", regionName(), accountID, layerName),
		"LayerVersionArn": record.Arn,
		"LicenseInfo":     record.LicenseInfo,
		"Version":         record.Version,
	}
}

func eventSourceMappingResponse(record eventSourceMappingRecord) map[string]any {
	return map[string]any{
		"BatchSize":        record.BatchSize,
		"EventSourceArn":   record.EventSourceArn,
		"FunctionArn":      record.FunctionArn,
		"FunctionName":     record.FunctionName,
		"LastModified":     record.LastModified,
		"StartingPosition": record.StartingPosition,
		"State":            record.State,
		"UUID":             record.UUID,
	}
}

func eventInvokeConfigResponse(record eventInvokeConfigRecord) map[string]any {
	resp := map[string]any{
		"FunctionArn":              lambdaARN(record.FunctionName),
		"LastModified":             time.Now().UTC().Format(time.RFC3339),
		"MaximumEventAgeInSeconds": record.MaxEventAge,
		"MaximumRetryAttempts":     record.MaxRetries,
	}
	if record.Qualifier != "" {
		resp["Qualifier"] = record.Qualifier
	}
	destinations := map[string]any{}
	if record.OnFailureArn != "" {
		destinations["OnFailure"] = map[string]any{"Destination": record.OnFailureArn}
	}
	if record.OnSuccessArn != "" {
		destinations["OnSuccess"] = map[string]any{"Destination": record.OnSuccessArn}
	}
	if len(destinations) > 0 {
		resp["DestinationConfig"] = destinations
	}
	return resp
}

func functionRefParts(ref string) (string, string) {
	parts := strings.Split(ref, ":function:")
	if len(parts) != 2 {
		return ref, ""
	}
	fn := parts[1]
	if name, qualifier, ok := strings.Cut(fn, ":"); ok {
		return name, qualifier
	}
	return fn, ""
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
	if len(input.FileSystemConfigs) > 0 || len(input.ImageConfig) > 0 ||
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

func layerNameFromPath(p string, withVersion bool) (string, int, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 3 || parts[0] != "2018-10-31" || parts[1] != "layers" {
		return "", 0, &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Layer not found"}
	}
	name := parts[2]
	if name == "" {
		return "", 0, badRequest("InvalidParameterValueException", "LayerName is required")
	}
	if !withVersion {
		return name, 0, nil
	}
	if len(parts) != 5 || parts[3] != "versions" {
		return "", 0, &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Layer version not found"}
	}
	version := atoiDefault(parts[4], 0)
	if version <= 0 {
		return "", 0, badRequest("InvalidParameterValueException", "Layer version is invalid")
	}
	return name, version, nil
}

func mappingUUIDFromPath(p string) (string, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) != 3 || parts[0] != "2015-03-31" || parts[1] != "event-source-mappings" || parts[2] == "" {
		return "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Event source mapping not found"}
	}
	return parts[2], nil
}

func functionNameAndQualifierFromInvokeConfigPath(p string) (string, string, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 4 || parts[0] != "2019-09-25" || parts[1] != "functions" || parts[len(parts)-1] != "event-invoke-config" {
		return "", "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Function event invoke config not found"}
	}
	name := parts[2]
	if name == "" {
		return "", "", badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	qualifier := "$LATEST"
	if len(parts) == 6 && parts[3] == "versions" {
		qualifier = parts[4]
	}
	return name, qualifier, nil
}

func functionNameFromCodeSigningConfigPath(p string) (string, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) != 4 || (parts[0] != "2019-09-25" && parts[0] != "2020-06-30") || parts[1] != "functions" || parts[3] != "code-signing-config" {
		return "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Function code signing config not found"}
	}
	name := parts[2]
	if name == "" {
		return "", badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	return name, nil
}

func functionNameFromPolicyPath(p string) (string, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) != 4 || parts[0] != "2015-03-31" || parts[1] != "functions" || parts[3] != "policy" {
		return "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "The resource you requested does not exist."}
	}
	name := parts[2]
	if name == "" {
		return "", badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	return name, nil
}

func functionNameAndStatementIDFromPolicyPath(p string) (string, string, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) != 5 || parts[0] != "2015-03-31" || parts[1] != "functions" || parts[3] != "policy" || parts[4] == "" {
		return "", "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "The resource you requested does not exist."}
	}
	name := parts[2]
	if name == "" {
		return "", "", badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	return name, parts[4], nil
}

func aliasPathParts(p string, requireAlias bool) (string, string, error) {
	clean := path.Clean(p)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 4 || parts[0] != "2015-03-31" || parts[1] != "functions" || parts[3] != "aliases" {
		return "", "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Alias not found"}
	}
	name := parts[2]
	if name == "" {
		return "", "", badRequest("InvalidParameterValueException", "FunctionName is required")
	}
	if requireAlias {
		if len(parts) != 5 || parts[4] == "" {
			return "", "", &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Alias not found"}
		}
		return name, parts[4], nil
	}
	return name, "", nil
}

func lambdaARN(name string) string {
	return fmt.Sprintf("arn:aws:lambda:us-east-1:%s:function:%s", accountID, name)
}

func qualifiedFunctionARN(name, qualifier string) string {
	base := lambdaARN(name)
	if qualifier == "" || qualifier == "$LATEST" {
		return base
	}
	return base + ":" + qualifier
}

func aliasARN(name, alias string) string {
	return qualifiedFunctionARN(name, alias)
}

func permissionPrefix(name, qualifier string) string {
	return permissionKey(name, qualifier, "")
}

func permissionKey(name, qualifier, statementID string) string {
	if qualifier == "" {
		return name + "||" + statementID
	}
	return name + "|" + qualifier + "|" + statementID
}

func permissionStatementJSON(record permissionRecord) string {
	statement := map[string]any{
		"Action":    record.Action,
		"Effect":    "Allow",
		"Principal": map[string]string{"Service": record.Principal},
		"Resource":  qualifiedFunctionARN(record.FunctionName, record.Qualifier),
		"Sid":       record.StatementID,
	}
	if record.SourceARN != "" {
		statement["Condition"] = map[string]map[string]string{
			"ArnLike": {"AWS:SourceArn": record.SourceARN},
		}
	}
	body, _ := json.Marshal(statement)
	return string(body)
}

func permissionPolicyJSON(records []permissionRecord) string {
	statements := make([]map[string]any, 0, len(records))
	for _, record := range records {
		statement := map[string]any{
			"Action":    record.Action,
			"Effect":    "Allow",
			"Principal": map[string]string{"Service": record.Principal},
			"Resource":  qualifiedFunctionARN(record.FunctionName, record.Qualifier),
			"Sid":       record.StatementID,
		}
		if record.SourceARN != "" {
			statement["Condition"] = map[string]map[string]string{
				"ArnLike": {"AWS:SourceArn": record.SourceARN},
			}
		}
		statements = append(statements, statement)
	}
	body, _ := json.Marshal(map[string]any{
		"Id":        "default",
		"Statement": statements,
		"Version":   "2012-10-17",
	})
	return string(body)
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

func aliasResponseForRecord(alias aliasRecord) aliasResponse {
	return aliasResponse{
		AliasArn:        aliasARN(alias.FunctionName, alias.AliasName),
		Description:     alias.Description,
		FunctionVersion: alias.FunctionVersion,
		Name:            alias.AliasName,
		RevisionID:      alias.RevisionID,
	}
}

func layerVersionKey(layerName string, version int) string {
	return fmt.Sprintf("%s|%08d", layerName, version)
}

func layerNamespace(layerName string, version int) string {
	return fmt.Sprintf("lambda-layers/%s/%d", layerName, version)
}

func layerVersionARN(layerName string, version int) string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:layer:%s:%d", regionName(), accountID, layerName, version)
}

func parseLayerVersionARN(arn string) (string, int, error) {
	parts := strings.Split(arn, ":layer:")
	if len(parts) != 2 {
		return "", 0, badRequest("InvalidParameterValueException", "Layer ARN is invalid")
	}
	layerRef := parts[1]
	name, versionRaw, ok := strings.Cut(layerRef, ":")
	if !ok {
		return "", 0, badRequest("InvalidParameterValueException", "Layer ARN is missing version")
	}
	version := atoiDefault(versionRaw, 0)
	if name == "" || version <= 0 {
		return "", 0, badRequest("InvalidParameterValueException", "Layer ARN is invalid")
	}
	return name, version, nil
}

func versionKey(name, version string) string {
	return name + "|" + version
}

func aliasKey(name, alias string) string {
	return name + "|" + alias
}

func eventInvokeConfigKey(name, qualifier string) string {
	return name + "|" + qualifier
}

func successCondition(success bool) string {
	if success {
		return "Success"
	}
	return "RetriesExhausted"
}

func ternaryInt(cond bool, ifTrue, ifFalse int) int {
	if cond {
		return ifTrue
	}
	return ifFalse
}

func nonEmptyJSON(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte("null")
	}
	return payload
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func atoiDefault(value string, fallback int) int {
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return fallback
		}
		n = (n * 10) + int(r-'0')
	}
	return n
}

func ternary(condition bool, left, right string) string {
	if condition {
		return left
	}
	return right
}

func regionName() string {
	return "us-east-1"
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
	return extractArchiveToDir(sourceDir, zipBytes)
}

func (s *Service) extractLayerSource(layerName string, version int, zipBytes []byte) error {
	sourceDir := s.blobs.NamespacePath(filepath.Join(layerNamespace(layerName, version), "source"))
	return extractArchiveToDir(sourceDir, zipBytes)
}

func extractArchiveToDir(sourceDir string, zipBytes []byte) error {
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
