package lambda

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
)

const (
	defaultListFunctionsPage = 50
	maxListFunctionsPage     = 10000
	maxFunctionTimeout       = 900
	maxFunctionMemory        = 10240
)

type updateFunctionCodeInput struct {
	Architectures   []string `json:"Architectures"`
	DryRun          bool     `json:"DryRun"`
	ImageURI        string   `json:"ImageUri"`
	Publish         bool     `json:"Publish"`
	RevisionID      string   `json:"RevisionId"`
	S3Bucket        string   `json:"S3Bucket"`
	S3Key           string   `json:"S3Key"`
	S3ObjectVersion string   `json:"S3ObjectVersion"`
	ZipFile         []byte   `json:"ZipFile"`
}

// updateFunctionConfigurationInput uses pointers so an update changes only
// the settings it names.
type updateFunctionConfigurationInput struct {
	Description       *string           `json:"Description"`
	Environment       *environmentInput `json:"Environment"`
	Handler           *string           `json:"Handler"`
	ImageConfig       *ImageConfig      `json:"ImageConfig"`
	Layers            *[]string         `json:"Layers"`
	MemorySize        *int              `json:"MemorySize"`
	RevisionID        string            `json:"RevisionId"`
	Role              *string           `json:"Role"`
	Runtime           *string           `json:"Runtime"`
	Timeout           *int              `json:"Timeout"`
	DeadLetterConfig  map[string]any    `json:"DeadLetterConfig"`
	EphemeralStorage  map[string]any    `json:"EphemeralStorage"`
	FileSystemConfigs []any             `json:"FileSystemConfigs"`
	KMSKeyArn         string            `json:"KMSKeyArn"`
	LoggingConfig     map[string]any    `json:"LoggingConfig"`
	SnapStart         map[string]any    `json:"SnapStart"`
	TracingConfig     map[string]any    `json:"TracingConfig"`
	VpcConfig         map[string]any    `json:"VpcConfig"`
}

func (in updateFunctionConfigurationInput) usesUnsupportedFeatures() bool {
	return len(in.DeadLetterConfig) > 0 || len(in.EphemeralStorage) > 0 || len(in.FileSystemConfigs) > 0 ||
		in.KMSKeyArn != "" || len(in.LoggingConfig) > 0 || len(in.SnapStart) > 0 ||
		len(in.TracingConfig) > 0 || len(in.VpcConfig) > 0
}

// listFunctions pages through the functions' $LATEST configurations in
// name order. The marker is the name of the first function on the next
// page.
func (s *Service) listFunctions(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	pageSize := defaultListFunctionsPage
	if raw := query.Get("MaxItems"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxListFunctionsPage {
			return badRequest("InvalidParameterValueException", "MaxItems must be between 1 and 10000")
		}
		pageSize = value
	}
	records, err := s.listFunctionRecords()
	if err != nil {
		return err
	}
	marker := query.Get("Marker")
	start := sort.Search(len(records), func(i int) bool { return records[i].FunctionName >= marker })
	end := min(start+pageSize, len(records))

	functions := make([]functionConfigurationResponse, 0, end-start)
	for _, record := range records[start:end] {
		functions = append(functions, s.configurationResponse(record))
	}
	resp := map[string]any{"Functions": functions}
	if end < len(records) {
		resp["NextMarker"] = records[end].FunctionName
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Service) listFunctionRecords() ([]functionRecord, error) {
	var records []functionRecord
	if err := s.metadata.Scan(functionsBucket, "", func(_, v []byte) error {
		var record functionRecord
		if err := json.Unmarshal(v, &record); err == nil {
			records = append(records, record)
		}
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].FunctionName < records[j].FunctionName })
	return records, nil
}

// updateFunctionCode replaces a function's zip or image. The next
// invocation starts a fresh environment running the new code.
func (s *Service) updateFunctionCode(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromPath(strings.TrimSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/code"), false)
	if err != nil {
		return err
	}
	var input updateFunctionCodeInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	record, err := s.loadRecord(name)
	if err != nil {
		return err
	}
	if err := checkRevision(record, input.RevisionID); err != nil {
		return err
	}
	if err := validateCodeUpdate(record, input); err != nil {
		return err
	}
	if len(input.Architectures) == 1 {
		record.Architectures = normalizeArchitectures(input.Architectures)
	}
	if input.DryRun {
		writeJSON(w, http.StatusOK, s.configurationResponse(record))
		return nil
	}

	if record.PackageType == PackageTypeImage {
		sum := sha256.Sum256([]byte(input.ImageURI))
		record.ImageURI = input.ImageURI
		record.CodeSHA256 = base64.StdEncoding.EncodeToString(sum[:])
	} else {
		stored, err := s.blobs.Put(codeNamespace+"/"+name, "code.zip", bytes.NewReader(input.ZipFile))
		if err != nil {
			return internal(err)
		}
		if err := s.extractSource(name, input.ZipFile); err != nil {
			return badRequest("InvalidParameterValueException", "Could not unzip uploaded file: "+err.Error())
		}
		record.CodeSize = stored.Size
		record.CodeSHA256 = stored.SHA256Base64
	}
	return s.saveUpdatedFunction(r.Context(), w, record, input.Publish)
}

func validateCodeUpdate(record functionRecord, input updateFunctionCodeInput) error {
	if input.S3Bucket != "" || input.S3Key != "" || input.S3ObjectVersion != "" {
		return notImplemented("only inline ZipFile uploads and container images are supported")
	}
	if len(input.Architectures) > 1 {
		return notImplemented("multiple architectures are not supported")
	}
	if record.PackageType == PackageTypeImage {
		if input.ImageURI == "" || len(input.ZipFile) > 0 {
			return badRequest("InvalidParameterValueException", "Please provide ImageUri when updating a function with packageType Image.")
		}
		return nil
	}
	if len(input.ZipFile) == 0 || input.ImageURI != "" {
		return badRequest("InvalidParameterValueException", "Please provide ZipFile when updating a function with packageType Zip.")
	}
	return nil
}

// updateFunctionConfiguration changes the settings an update names. The
// next invocation starts a fresh environment with them.
func (s *Service) updateFunctionConfiguration(w http.ResponseWriter, r *http.Request) error {
	name, err := functionNameFromPath(r.URL.Path, true)
	if err != nil {
		return err
	}
	var input updateFunctionConfigurationInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	if input.usesUnsupportedFeatures() {
		return notImplemented("one or more requested Lambda features are not supported yet")
	}
	record, err := s.loadRecord(name)
	if err != nil {
		return err
	}
	if err := checkRevision(record, input.RevisionID); err != nil {
		return err
	}
	if err := s.applyConfiguration(&record, input); err != nil {
		return err
	}
	return s.saveUpdatedFunction(r.Context(), w, record, false)
}

// applyConfiguration validates an update and copies it onto the record.
func (s *Service) applyConfiguration(record *functionRecord, input updateFunctionConfigurationInput) error {
	image := record.PackageType == PackageTypeImage
	if image && (input.Runtime != nil || input.Handler != nil || input.Layers != nil) {
		return badRequest("InvalidParameterValueException", "Runtime, Handler and Layers cannot be set for PackageType Image; use ImageConfig")
	}
	if !image && input.ImageConfig != nil {
		return badRequest("InvalidParameterValueException", "ImageConfig requires PackageType Image")
	}
	if input.Timeout != nil && (*input.Timeout < 1 || *input.Timeout > maxFunctionTimeout) {
		return badRequest("InvalidParameterValueException", "Timeout must be between 1 and 900")
	}
	if input.MemorySize != nil && (*input.MemorySize < 128 || *input.MemorySize > maxFunctionMemory) {
		return badRequest("InvalidParameterValueException", "MemorySize must be between 128 and 10240")
	}
	if input.Role != nil && *input.Role == "" {
		return badRequest("InvalidParameterValueException", "Role cannot be empty")
	}
	if input.Runtime != nil && *input.Runtime == "" {
		return badRequest("InvalidParameterValueException", "Runtime cannot be empty")
	}
	if input.Handler != nil && *input.Handler == "" {
		return badRequest("InvalidParameterValueException", "Handler cannot be empty")
	}
	if input.Layers != nil {
		if err := s.validateLayers(*input.Layers); err != nil {
			return err
		}
		record.Layers = append([]string(nil), (*input.Layers)...)
	}
	assign(&record.Description, input.Description)
	assign(&record.Handler, input.Handler)
	assign(&record.Role, input.Role)
	assign(&record.Runtime, input.Runtime)
	assign(&record.Timeout, input.Timeout)
	assign(&record.MemorySize, input.MemorySize)
	assign(&record.ImageConfig, input.ImageConfig)
	if input.Environment != nil {
		record.Environment = cloneMap(input.Environment.Variables)
	}
	return nil
}

// saveUpdatedFunction stores an updated $LATEST, optionally publishing it
// as a version, and discards the warm environment so the next invocation
// runs the update.
func (s *Service) saveUpdatedFunction(ctx context.Context, w http.ResponseWriter, record functionRecord, publish bool) error {
	record.LastModified = s.now().UTC().Format(lastModifiedLayout)
	record.RevisionID = uuid.NewString()
	if err := s.putFunctionRecord(record); err != nil {
		return err
	}
	if s.invoker != nil {
		if err := s.invoker.CleanupFunction(ctx, RuntimeKey(s.scope, record.FunctionName)); err != nil {
			return internal(err)
		}
	}
	if publish {
		published, err := s.publishRecord(record)
		if err != nil {
			return err
		}
		record = published
	}
	writeJSON(w, http.StatusOK, s.configurationResponse(record))
	return nil
}

// checkRevision rejects an update made against a stale revision.
func checkRevision(record functionRecord, revisionID string) error {
	if revisionID == "" || revisionID == record.RevisionID {
		return nil
	}
	return &apierror.Error{
		StatusCode: http.StatusPreconditionFailed,
		Code:       "PreconditionFailedException",
		Message:    "The Revision Id provided does not match the latest Revision Id. Call the GetFunction/GetAlias API to retrieve the latest Revision Id",
	}
}

func assign[T any](target *T, value *T) {
	if value != nil {
		*target = *value
	}
}
