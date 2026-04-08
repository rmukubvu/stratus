package apigateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/store"
)

const (
	apisBucket         = "apigateway-rest-apis"
	resourcesBucket    = "apigateway-rest-resources"
	methodsBucket      = "apigateway-rest-methods"
	integrationsBucket = "apigateway-rest-integrations"
	deploymentsBucket  = "apigateway-rest-deployments"
	stagesBucket       = "apigateway-rest-stages"
)

type Options struct {
	Metadata store.Store
	Lambda   *lambdasvc.Service
}

type Service struct {
	metadata store.Store
	lambda   *lambdasvc.Service
	now      func() time.Time
	mu       sync.Mutex
}

type CreateAPIInput struct {
	Description string
	Name        string
}

type CreateResourceInput struct {
	APIID    string
	ParentID string
	PathPart string
}

type PutMethodInput struct {
	APIID             string
	AuthorizationType string
	HTTPMethod        string
	ResourceID        string
}

type PutIntegrationInput struct {
	APIID                 string
	HTTPMethod            string
	IntegrationHTTPMethod string
	ResourceID            string
	Type                  string
	URI                   string
}

type CreateDeploymentInput struct {
	APIID       string
	Description string
	StageName   string
}

type CreateStageInput struct {
	APIID        string
	DeploymentID string
	StageName    string
}

type apiRecord struct {
	CreatedAt      time.Time `json:"created_at"`
	Description    string    `json:"description,omitempty"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	RootResourceID string    `json:"root_resource_id"`
}

type resourceRecord struct {
	APIID      string `json:"api_id"`
	ParentID   string `json:"parent_id,omitempty"`
	Path       string `json:"path"`
	PathPart   string `json:"path_part,omitempty"`
	ResourceID string `json:"resource_id"`
}

type methodRecord struct {
	APIID             string `json:"api_id"`
	AuthorizationType string `json:"authorization_type"`
	HTTPMethod        string `json:"http_method"`
	ResourceID        string `json:"resource_id"`
}

type integrationRecord struct {
	APIID                 string `json:"api_id"`
	HTTPMethod            string `json:"http_method"`
	IntegrationHTTPMethod string `json:"integration_http_method"`
	ResourceID            string `json:"resource_id"`
	Type                  string `json:"type"`
	URI                   string `json:"uri"`
}

type deploymentRecord struct {
	APIID        string    `json:"api_id"`
	CreatedAt    time.Time `json:"created_at"`
	DeploymentID string    `json:"deployment_id"`
	Description  string    `json:"description,omitempty"`
}

type stageRecord struct {
	APIID        string    `json:"api_id"`
	CreatedAt    time.Time `json:"created_at"`
	DeploymentID string    `json:"deployment_id"`
	StageName    string    `json:"stage_name"`
}

func NewService(opts Options) *Service {
	return &Service{metadata: opts.Metadata, lambda: opts.Lambda, now: time.Now}
}

func (s *Service) CreateAPI(input CreateAPIInput) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.Name == "" {
		return "", "", validation("name is required")
	}
	apiID := shortID()
	rootID := shortID()
	record := apiRecord{
		CreatedAt:      s.now().UTC(),
		Description:    input.Description,
		ID:             apiID,
		Name:           input.Name,
		RootResourceID: rootID,
	}
	if err := s.putJSON(apisBucket, apiID, record); err != nil {
		return "", "", err
	}
	root := resourceRecord{APIID: apiID, Path: "/", ResourceID: rootID}
	if err := s.putJSON(resourcesBucket, resourceKey(apiID, rootID), root); err != nil {
		return "", "", err
	}
	return apiID, rootID, nil
}

func (s *Service) CreateResource(input CreateResourceInput) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadAPI(input.APIID); err != nil {
		return "", "", err
	}
	parent, err := s.loadResource(input.APIID, input.ParentID)
	if err != nil {
		return "", "", err
	}
	if input.PathPart == "" {
		return "", "", validation("pathPart is required")
	}
	record := resourceRecord{
		APIID:      input.APIID,
		ParentID:   input.ParentID,
		Path:       joinPath(parent.Path, input.PathPart),
		PathPart:   input.PathPart,
		ResourceID: shortID(),
	}
	if err := s.putJSON(resourcesBucket, resourceKey(input.APIID, record.ResourceID), record); err != nil {
		return "", "", err
	}
	return record.ResourceID, record.Path, nil
}

func (s *Service) PutMethod(input PutMethodInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadResource(input.APIID, input.ResourceID); err != nil {
		return err
	}
	record := methodRecord{
		APIID:             input.APIID,
		AuthorizationType: defaultString(input.AuthorizationType, "NONE"),
		HTTPMethod:        strings.ToUpper(input.HTTPMethod),
		ResourceID:        input.ResourceID,
	}
	return s.putJSON(methodsBucket, methodKey(input.APIID, input.ResourceID, record.HTTPMethod), record)
}

func (s *Service) PutIntegration(input PutIntegrationInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadMethod(input.APIID, input.ResourceID, input.HTTPMethod); err != nil {
		return err
	}
	if input.Type != "AWS_PROXY" {
		return notImplemented("only AWS_PROXY integrations are supported")
	}
	if !strings.Contains(input.URI, ":function:") {
		return notImplemented("integration uri must reference a lambda function")
	}
	record := integrationRecord{
		APIID:                 input.APIID,
		HTTPMethod:            strings.ToUpper(input.HTTPMethod),
		IntegrationHTTPMethod: defaultString(input.IntegrationHTTPMethod, "POST"),
		ResourceID:            input.ResourceID,
		Type:                  input.Type,
		URI:                   input.URI,
	}
	return s.putJSON(integrationsBucket, integrationKey(input.APIID, input.ResourceID, record.HTTPMethod), record)
}

func (s *Service) CreateDeployment(input CreateDeploymentInput) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadAPI(input.APIID); err != nil {
		return "", err
	}
	record := deploymentRecord{
		APIID:        input.APIID,
		CreatedAt:    s.now().UTC(),
		DeploymentID: shortID(),
		Description:  input.Description,
	}
	if err := s.putJSON(deploymentsBucket, deploymentKey(input.APIID, record.DeploymentID), record); err != nil {
		return "", err
	}
	if input.StageName != "" {
		stage := stageRecord{
			APIID:        input.APIID,
			CreatedAt:    s.now().UTC(),
			DeploymentID: record.DeploymentID,
			StageName:    input.StageName,
		}
		if err := s.putJSON(stagesBucket, stageKey(input.APIID, stage.StageName), stage); err != nil {
			return "", err
		}
	}
	return record.DeploymentID, nil
}

func (s *Service) CreateStage(input CreateStageInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadAPI(input.APIID); err != nil {
		return err
	}
	stage := stageRecord{
		APIID:        input.APIID,
		CreatedAt:    s.now().UTC(),
		DeploymentID: input.DeploymentID,
		StageName:    input.StageName,
	}
	return s.putJSON(stagesBucket, stageKey(input.APIID, stage.StageName), stage)
}

func (s *Service) DeleteAPI(apiID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	for _, bucket := range []string{apisBucket, resourcesBucket, methodsBucket, integrationsBucket, deploymentsBucket, stagesBucket} {
		switch bucket {
		case apisBucket:
			if err := s.metadata.Delete(bucket, apiID); err != nil {
				return internal(err)
			}
		default:
			if err := s.metadata.DeletePrefix(bucket, apiID+"|"); err != nil {
				return internal(err)
			}
		}
	}
	return nil
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.HasPrefix(r.URL.Path, "/_aws/restapis/") {
		return s.invoke(w, r)
	}

	path := strings.TrimPrefix(strings.Trim(r.URL.Path, "/"), "/")
	parts := []string{}
	if path != "" {
		parts = strings.Split(path, "/")
	}
	switch {
	case len(parts) == 1 && parts[0] == "restapis" && r.Method == http.MethodPost:
		return s.createRestAPI(w, r)
	case len(parts) == 1 && parts[0] == "restapis" && r.Method == http.MethodGet:
		return s.getRestAPIs(w)
	case len(parts) == 2 && parts[0] == "restapis" && r.Method == http.MethodGet:
		return s.getRestAPI(w, parts[1])
	case len(parts) == 2 && parts[0] == "restapis" && r.Method == http.MethodDelete:
		return s.deleteRestAPI(w, parts[1])
	case len(parts) == 3 && parts[0] == "restapis" && parts[2] == "resources" && r.Method == http.MethodGet:
		return s.getResources(w, parts[1])
	case len(parts) == 4 && parts[0] == "restapis" && parts[2] == "resources" && r.Method == http.MethodPost:
		return s.createResource(w, r, parts[1], parts[3])
	case len(parts) == 6 && parts[0] == "restapis" && parts[2] == "resources" && parts[4] == "methods" && r.Method == http.MethodPut:
		return s.putMethod(w, r, parts[1], parts[3], parts[5])
	case len(parts) == 7 && parts[0] == "restapis" && parts[2] == "resources" && parts[4] == "methods" && parts[6] == "integration" && r.Method == http.MethodPut:
		return s.putIntegration(w, r, parts[1], parts[3], parts[5])
	case len(parts) == 3 && parts[0] == "restapis" && parts[2] == "deployments" && r.Method == http.MethodPost:
		return s.createDeployment(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "restapis" && parts[2] == "stages" && r.Method == http.MethodGet:
		return s.getStages(w, parts[1])
	default:
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "api gateway route was not found"}
	}
}

func (s *Service) createRestAPI(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Description string `json:"description"`
		Name        string `json:"name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.Name == "" {
		return validation("name is required")
	}
	apiID := shortID()
	rootID := shortID()
	record := apiRecord{
		CreatedAt:      s.now().UTC(),
		Description:    input.Description,
		ID:             apiID,
		Name:           input.Name,
		RootResourceID: rootID,
	}
	if err := s.putJSON(apisBucket, apiID, record); err != nil {
		return err
	}
	root := resourceRecord{
		APIID:      apiID,
		Path:       "/",
		ResourceID: rootID,
	}
	if err := s.putJSON(resourcesBucket, resourceKey(apiID, rootID), root); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, apiResponse(record))
	return nil
}

func (s *Service) getRestAPIs(w http.ResponseWriter) error {
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(apisBucket, "", func(_, v []byte) error {
		var record apiRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, apiResponse(record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["name"].(string) < items[j]["name"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"item": items})
	return nil
}

func (s *Service) getRestAPI(w http.ResponseWriter, apiID string) error {
	record, err := s.loadAPI(apiID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, apiResponse(record))
	return nil
}

func (s *Service) deleteRestAPI(w http.ResponseWriter, apiID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	for _, bucket := range []string{apisBucket, resourcesBucket, methodsBucket, integrationsBucket, deploymentsBucket, stagesBucket} {
		switch bucket {
		case apisBucket:
			if err := s.metadata.Delete(bucket, apiID); err != nil {
				return internal(err)
			}
		default:
			if err := s.metadata.DeletePrefix(bucket, apiID+"|"); err != nil {
				return internal(err)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) getResources(w http.ResponseWriter, apiID string) error {
	api, err := s.loadAPI(apiID)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(resourcesBucket, apiID+"|", func(_, v []byte) error {
		var record resourceRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, resourceResponse(record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["path"].(string) < items[j]["path"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"_embedded": map[string]any{"item": items}, "item": items, "id": api.ID})
	return nil
}

func (s *Service) createResource(w http.ResponseWriter, r *http.Request, apiID, parentID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	parent, err := s.loadResource(apiID, parentID)
	if err != nil {
		return err
	}
	var input struct {
		PathPart string `json:"pathPart"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.PathPart == "" {
		return validation("pathPart is required")
	}
	record := resourceRecord{
		APIID:      apiID,
		ParentID:   parentID,
		Path:       joinPath(parent.Path, input.PathPart),
		PathPart:   input.PathPart,
		ResourceID: shortID(),
	}
	if err := s.putJSON(resourcesBucket, resourceKey(apiID, record.ResourceID), record); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, resourceResponse(record))
	return nil
}

func (s *Service) putMethod(w http.ResponseWriter, r *http.Request, apiID, resourceID, method string) error {
	if _, err := s.loadResource(apiID, resourceID); err != nil {
		return err
	}
	var input struct {
		AuthorizationType string `json:"authorizationType"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record := methodRecord{
		APIID:             apiID,
		AuthorizationType: defaultString(input.AuthorizationType, "NONE"),
		HTTPMethod:        strings.ToUpper(method),
		ResourceID:        resourceID,
	}
	if err := s.putJSON(methodsBucket, methodKey(apiID, resourceID, record.HTTPMethod), record); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"apiKeyRequired":    false,
		"authorizationType": record.AuthorizationType,
		"httpMethod":        record.HTTPMethod,
	})
	return nil
}

func (s *Service) putIntegration(w http.ResponseWriter, r *http.Request, apiID, resourceID, method string) error {
	if _, err := s.loadMethod(apiID, resourceID, method); err != nil {
		return err
	}
	var input struct {
		IntegrationHTTPMethod string `json:"httpMethod"`
		Type                  string `json:"type"`
		URI                   string `json:"uri"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.Type != "AWS_PROXY" {
		return notImplemented("only AWS_PROXY integrations are supported")
	}
	if !strings.Contains(input.URI, ":function:") {
		return notImplemented("integration uri must reference a lambda function")
	}
	record := integrationRecord{
		APIID:                 apiID,
		HTTPMethod:            strings.ToUpper(method),
		IntegrationHTTPMethod: defaultString(input.IntegrationHTTPMethod, "POST"),
		ResourceID:            resourceID,
		Type:                  input.Type,
		URI:                   input.URI,
	}
	if err := s.putJSON(integrationsBucket, integrationKey(apiID, resourceID, record.HTTPMethod), record); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"httpMethod": record.IntegrationHTTPMethod,
		"type":       record.Type,
		"uri":        record.URI,
	})
	return nil
}

func (s *Service) createDeployment(w http.ResponseWriter, r *http.Request, apiID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	var input struct {
		Description string `json:"description"`
		StageName   string `json:"stageName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record := deploymentRecord{
		APIID:        apiID,
		CreatedAt:    s.now().UTC(),
		DeploymentID: shortID(),
		Description:  input.Description,
	}
	if err := s.putJSON(deploymentsBucket, deploymentKey(apiID, record.DeploymentID), record); err != nil {
		return err
	}
	if input.StageName != "" {
		stage := stageRecord{
			APIID:        apiID,
			CreatedAt:    s.now().UTC(),
			DeploymentID: record.DeploymentID,
			StageName:    input.StageName,
		}
		if err := s.putJSON(stagesBucket, stageKey(apiID, stage.StageName), stage); err != nil {
			return err
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"createdDate": formatTime(record.CreatedAt),
		"description": record.Description,
		"id":          record.DeploymentID,
	})
	return nil
}

func (s *Service) getStages(w http.ResponseWriter, apiID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(stagesBucket, apiID+"|", func(_, v []byte) error {
		var record stageRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"createdDate":  formatTime(record.CreatedAt),
			"deploymentId": record.DeploymentID,
			"stageName":    record.StageName,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["stageName"].(string) < items[j]["stageName"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"item": items})
	return nil
}

func (s *Service) invoke(w http.ResponseWriter, r *http.Request) error {
	if s.lambda == nil {
		return &apierror.Error{StatusCode: http.StatusServiceUnavailable, Code: "ServiceUnavailableException", Message: "lambda runtime is not configured"}
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/_aws/restapis/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] != "_user_request_" {
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "invoke route was not found"}
	}
	apiID := parts[0]
	stageName := parts[1]
	requestPath := "/"
	if len(parts) > 3 {
		requestPath = "/" + strings.Join(parts[3:], "/")
	}
	if _, err := s.loadStage(apiID, stageName); err != nil {
		return err
	}
	resource, err := s.loadResourceByPath(apiID, requestPath)
	if err != nil {
		return err
	}
	method, err := s.loadMethod(apiID, resource.ResourceID, r.Method)
	if err != nil {
		return err
	}
	integration, err := s.loadIntegration(apiID, resource.ResourceID, method.HTTPMethod)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return validation("unable to read request body")
	}
	event := map[string]any{
		"body":                  string(body),
		"headers":               cloneHeaders(r.Header),
		"httpMethod":            method.HTTPMethod,
		"isBase64Encoded":       false,
		"path":                  requestPath,
		"queryStringParameters": singleValueQuery(r.URL.Query()),
		"requestContext": map[string]any{
			"httpMethod":   method.HTTPMethod,
			"requestId":    uuid.NewString(),
			"resourcePath": resource.Path,
			"stage":        stageName,
		},
		"resource": resource.Path,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return internal(err)
	}
	result, err := s.lambda.InvokeByName(context.Background(), lambdaName(integration.URI), payload)
	if err != nil {
		return err
	}
	return writeLambdaProxyResponse(w, result)
}

func (s *Service) loadAPI(apiID string) (apiRecord, error) {
	raw, err := s.metadata.Get(apisBucket, apiID)
	if err != nil {
		return apiRecord{}, internal(err)
	}
	if raw == nil {
		return apiRecord{}, notFound("rest api not found")
	}
	var record apiRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return apiRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadResource(apiID, resourceID string) (resourceRecord, error) {
	raw, err := s.metadata.Get(resourcesBucket, resourceKey(apiID, resourceID))
	if err != nil {
		return resourceRecord{}, internal(err)
	}
	if raw == nil {
		return resourceRecord{}, notFound("resource not found")
	}
	var record resourceRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return resourceRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadResourceByPath(apiID, path string) (resourceRecord, error) {
	var match resourceRecord
	found := false
	if err := s.metadata.Scan(resourcesBucket, apiID+"|", func(_, v []byte) error {
		var record resourceRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if record.Path == path {
			match = record
			found = true
		}
		return nil
	}); err != nil {
		return resourceRecord{}, internal(err)
	}
	if !found {
		return resourceRecord{}, notFound("resource path not found")
	}
	return match, nil
}

func (s *Service) loadMethod(apiID, resourceID, method string) (methodRecord, error) {
	raw, err := s.metadata.Get(methodsBucket, methodKey(apiID, resourceID, strings.ToUpper(method)))
	if err != nil {
		return methodRecord{}, internal(err)
	}
	if raw == nil {
		return methodRecord{}, notFound("method not found")
	}
	var record methodRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return methodRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadIntegration(apiID, resourceID, method string) (integrationRecord, error) {
	raw, err := s.metadata.Get(integrationsBucket, integrationKey(apiID, resourceID, strings.ToUpper(method)))
	if err != nil {
		return integrationRecord{}, internal(err)
	}
	if raw == nil {
		return integrationRecord{}, notFound("integration not found")
	}
	var record integrationRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return integrationRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadStage(apiID, stageName string) (stageRecord, error) {
	raw, err := s.metadata.Get(stagesBucket, stageKey(apiID, stageName))
	if err != nil {
		return stageRecord{}, internal(err)
	}
	if raw == nil {
		return stageRecord{}, notFound("stage not found")
	}
	var record stageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return stageRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putJSON(bucket, key string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(bucket, key, raw); err != nil {
		return internal(err)
	}
	return nil
}

func apiResponse(record apiRecord) map[string]any {
	return map[string]any{
		"createdDate":    formatTime(record.CreatedAt),
		"description":    record.Description,
		"id":             record.ID,
		"name":           record.Name,
		"rootResourceId": record.RootResourceID,
	}
}

func resourceResponse(record resourceRecord) map[string]any {
	resp := map[string]any{
		"id":   record.ResourceID,
		"path": record.Path,
	}
	if record.ParentID != "" {
		resp["parentId"] = record.ParentID
		resp["pathPart"] = record.PathPart
	}
	return resp
}

func resourceKey(apiID, resourceID string) string { return apiID + "|" + resourceID }
func methodKey(apiID, resourceID, method string) string {
	return apiID + "|" + resourceID + "|" + strings.ToUpper(method)
}
func integrationKey(apiID, resourceID, method string) string {
	return apiID + "|" + resourceID + "|" + strings.ToUpper(method)
}
func deploymentKey(apiID, deploymentID string) string { return apiID + "|" + deploymentID }
func stageKey(apiID, stageName string) string         { return apiID + "|" + stageName }

func joinPath(parent, part string) string {
	if parent == "/" {
		return "/" + part
	}
	return parent + "/" + part
}

func shortID() string { return strings.ReplaceAll(uuid.NewString()[:10], "-", "") }

func lambdaName(uri string) string {
	parts := strings.Split(uri, ":function:")
	if len(parts) != 2 {
		return uri
	}
	name := parts[1]
	if idx := strings.IndexAny(name, ":/"); idx >= 0 {
		name = name[:idx]
	}
	return name
}

func cloneHeaders(in http.Header) map[string]string {
	out := map[string]string{}
	for key, values := range in {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func singleValueQuery(in map[string][]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for key, values := range in {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func writeLambdaProxyResponse(w http.ResponseWriter, result lambdasvc.InvokeResult) error {
	var proxy struct {
		StatusCode      int               `json:"statusCode"`
		Headers         map[string]string `json:"headers"`
		Body            string            `json:"body"`
		IsBase64Encoded bool              `json:"isBase64Encoded"`
	}
	if err := json.Unmarshal(result.Payload, &proxy); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Payload)
		return nil
	}
	if proxy.StatusCode == 0 {
		proxy.StatusCode = http.StatusOK
	}
	for key, value := range proxy.Headers {
		w.Header().Set(key, value)
	}
	body := []byte(proxy.Body)
	if proxy.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(proxy.Body)
		if err != nil {
			return validation("lambda response body is not valid base64")
		}
		body = decoded
	}
	w.WriteHeader(proxy.StatusCode)
	_, _ = w.Write(body)
	return nil
}

func decodeJSON(r *http.Request, out any) error {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil && err != io.EOF {
		return validation("request body is not valid JSON")
	}
	return nil
}

func defaultString(value, def string) string {
	if value == "" {
		return def
	}
	return value
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "BadRequestException", Message: message}
}

func notFound(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: message}
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
