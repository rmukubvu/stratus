package apigatewayv2

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/store"
)

const (
	apisBucket         = "apigatewayv2-apis"
	integrationsBucket = "apigatewayv2-integrations"
	routesBucket       = "apigatewayv2-routes"
	stagesBucket       = "apigatewayv2-stages"
)

type LambdaInvoker interface {
	InvokeByName(ctx context.Context, name string, payload []byte) (lambdasvc.InvokeResult, error)
}

type Options struct {
	Metadata store.Store
	Lambda   LambdaInvoker
}

type Service struct {
	metadata store.Store
	lambda   LambdaInvoker
	now      func() time.Time
}

type APIRecord struct {
	APIID                     string            `json:"api_id"`
	APIKeySelectionExpression string            `json:"api_key_selection_expression"`
	CreatedAt                 time.Time         `json:"created_at"`
	Description               string            `json:"description,omitempty"`
	DisableExecuteAPIEndpoint bool              `json:"disable_execute_api_endpoint"`
	Name                      string            `json:"name"`
	ProtocolType              string            `json:"protocol_type"`
	RouteSelectionExpression  string            `json:"route_selection_expression"`
	Tags                      map[string]string `json:"tags,omitempty"`
}

type IntegrationRecord struct {
	APIID                string    `json:"api_id"`
	CreatedAt            time.Time `json:"created_at"`
	Description          string    `json:"description,omitempty"`
	IntegrationID        string    `json:"integration_id"`
	IntegrationMethod    string    `json:"integration_method,omitempty"`
	IntegrationType      string    `json:"integration_type"`
	IntegrationURI       string    `json:"integration_uri"`
	PayloadFormatVersion string    `json:"payload_format_version,omitempty"`
	TimeoutInMillis      int       `json:"timeout_in_millis,omitempty"`
}

type RouteRecord struct {
	APIID             string    `json:"api_id"`
	AuthorizationType string    `json:"authorization_type"`
	CreatedAt         time.Time `json:"created_at"`
	OperationName     string    `json:"operation_name,omitempty"`
	RouteID           string    `json:"route_id"`
	RouteKey          string    `json:"route_key"`
	Target            string    `json:"target"`
}

type StageRecord struct {
	APIID       string            `json:"api_id"`
	AutoDeploy  bool              `json:"auto_deploy"`
	CreatedAt   time.Time         `json:"created_at"`
	Description string            `json:"description,omitempty"`
	StageName   string            `json:"stage_name"`
	Variables   map[string]string `json:"variables,omitempty"`
}

type CreateAPIInput struct {
	APIKeySelectionExpression string            `json:"apiKeySelectionExpression"`
	Description               string            `json:"description"`
	DisableExecuteAPIEndpoint bool              `json:"disableExecuteApiEndpoint"`
	Name                      string            `json:"name"`
	ProtocolType              string            `json:"protocolType"`
	RouteSelectionExpression  string            `json:"routeSelectionExpression"`
	Tags                      map[string]string `json:"tags"`
}

type CreateIntegrationInput struct {
	Description          string `json:"description"`
	IntegrationMethod    string `json:"integrationMethod"`
	IntegrationType      string `json:"integrationType"`
	IntegrationURI       string `json:"integrationUri"`
	PayloadFormatVersion string `json:"payloadFormatVersion"`
	TimeoutInMillis      int    `json:"timeoutInMillis"`
}

type CreateRouteInput struct {
	AuthorizationType string `json:"authorizationType"`
	OperationName     string `json:"operationName"`
	RouteKey          string `json:"routeKey"`
	Target            string `json:"target"`
}

type CreateStageInput struct {
	AutoDeploy     bool              `json:"autoDeploy"`
	Description    string            `json:"description"`
	StageName      string            `json:"stageName"`
	StageVariables map[string]string `json:"stageVariables"`
}

func NewService(opts Options) *Service {
	return &Service{
		metadata: opts.Metadata,
		lambda:   opts.Lambda,
		now:      time.Now,
	}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request) error {
	if strings.HasPrefix(r.URL.Path, "/_aws/execute-api/") {
		return s.invoke(w, r)
	}

	parts := splitPath(r.URL.Path)
	if len(parts) >= 2 && parts[0] == "v2" && parts[1] == "apis" {
		switch {
		case len(parts) == 2 && r.Method == http.MethodPost:
			return s.createAPI(w, r)
		case len(parts) == 2 && r.Method == http.MethodGet:
			return s.getAPIs(w, r)
		case len(parts) == 3 && r.Method == http.MethodGet:
			return s.getAPI(w, r, parts[2])
		case len(parts) == 3 && r.Method == http.MethodPatch:
			return s.updateAPI(w, r, parts[2])
		case len(parts) == 3 && r.Method == http.MethodDelete:
			return s.deleteAPI(w, parts[2])
		case len(parts) == 4 && parts[3] == "integrations" && r.Method == http.MethodPost:
			return s.createIntegration(w, r, parts[2])
		case len(parts) == 4 && parts[3] == "integrations" && r.Method == http.MethodGet:
			return s.getIntegrations(w, parts[2])
		case len(parts) == 5 && parts[3] == "integrations" && r.Method == http.MethodGet:
			return s.getIntegration(w, parts[2], parts[4])
		case len(parts) == 4 && parts[3] == "routes" && r.Method == http.MethodPost:
			return s.createRoute(w, r, parts[2])
		case len(parts) == 4 && parts[3] == "routes" && r.Method == http.MethodGet:
			return s.getRoutes(w, parts[2])
		case len(parts) == 5 && parts[3] == "routes" && r.Method == http.MethodGet:
			return s.getRoute(w, parts[2], parts[4])
		case len(parts) == 4 && parts[3] == "stages" && r.Method == http.MethodPost:
			return s.createStage(w, r, parts[2])
		case len(parts) == 4 && parts[3] == "stages" && r.Method == http.MethodGet:
			return s.getStages(w, parts[2])
		case len(parts) == 5 && parts[3] == "stages" && r.Method == http.MethodGet:
			return s.getStage(w, parts[2], parts[4])
		}
	}

	return &apierror.Error{
		StatusCode: http.StatusNotFound,
		Code:       "NotFoundException",
		Message:    "apigatewayv2 resource was not found",
	}
}

func (s *Service) createAPI(w http.ResponseWriter, r *http.Request) error {
	var input CreateAPIInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	record, err := s.CreateAPI(input)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.apiResponse(r, record))
	return nil
}

func (s *Service) CreateAPI(input CreateAPIInput) (APIRecord, error) {
	if input.Name == "" {
		return APIRecord{}, validation("name is required")
	}
	if input.ProtocolType == "" {
		return APIRecord{}, validation("protocolType is required")
	}
	if input.ProtocolType != "HTTP" {
		return APIRecord{}, notImplemented("only HTTP APIs are supported")
	}
	if input.RouteSelectionExpression == "" {
		input.RouteSelectionExpression = "$request.method $request.path"
	}
	if input.APIKeySelectionExpression == "" {
		input.APIKeySelectionExpression = "$request.header.x-api-key"
	}
	record := APIRecord{
		APIID:                     shortID(),
		APIKeySelectionExpression: input.APIKeySelectionExpression,
		CreatedAt:                 s.now().UTC(),
		Description:               input.Description,
		DisableExecuteAPIEndpoint: input.DisableExecuteAPIEndpoint,
		Name:                      input.Name,
		ProtocolType:              input.ProtocolType,
		RouteSelectionExpression:  input.RouteSelectionExpression,
		Tags:                      cloneMap(input.Tags),
	}
	if err := s.putJSON(apisBucket, record.APIID, record); err != nil {
		return APIRecord{}, err
	}
	return record, nil
}

func (s *Service) updateAPI(w http.ResponseWriter, r *http.Request, apiID string) error {
	record, err := s.loadAPI(apiID)
	if err != nil {
		return err
	}
	var input struct {
		APIKeySelectionExpression *string           `json:"apiKeySelectionExpression"`
		Description               *string           `json:"description"`
		DisableExecuteAPIEndpoint *bool             `json:"disableExecuteApiEndpoint"`
		Name                      *string           `json:"name"`
		RouteSelectionExpression  *string           `json:"routeSelectionExpression"`
		Tags                      map[string]string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if input.APIKeySelectionExpression != nil && *input.APIKeySelectionExpression != "" {
		record.APIKeySelectionExpression = *input.APIKeySelectionExpression
	}
	if input.Description != nil {
		record.Description = *input.Description
	}
	if input.DisableExecuteAPIEndpoint != nil {
		record.DisableExecuteAPIEndpoint = *input.DisableExecuteAPIEndpoint
	}
	if input.Name != nil && *input.Name != "" {
		record.Name = *input.Name
	}
	if input.RouteSelectionExpression != nil && *input.RouteSelectionExpression != "" {
		record.RouteSelectionExpression = *input.RouteSelectionExpression
	}
	if input.Tags != nil {
		record.Tags = cloneMap(input.Tags)
	}
	if err := s.putJSON(apisBucket, record.APIID, record); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.apiResponse(r, record))
	return nil
}

func (s *Service) getAPIs(w http.ResponseWriter, r *http.Request) error {
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(apisBucket, "", func(_, v []byte) error {
		var record APIRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, s.apiResponse(r, record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["apiId"].(string) < items[j]["apiId"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Service) getAPI(w http.ResponseWriter, r *http.Request, apiID string) error {
	record, err := s.loadAPI(apiID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.apiResponse(r, record))
	return nil
}

func (s *Service) deleteAPI(w http.ResponseWriter, apiID string) error {
	if err := s.DeleteAPI(apiID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) DeleteAPI(apiID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	if err := s.metadata.Delete(apisBucket, apiID); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(integrationsBucket, apiID+"|"); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(routesBucket, apiID+"|"); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(stagesBucket, apiID+"|"); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) DeleteIntegration(apiID, integrationID string) error {
	if _, err := s.loadIntegration(apiID, integrationID); err != nil {
		return err
	}
	if err := s.metadata.Delete(integrationsBucket, integrationStoreKey(apiID, integrationID)); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) DeleteRoute(apiID, routeID string) error {
	raw, err := s.metadata.Get(routesBucket, routeStoreKey(apiID, routeID))
	if err != nil {
		return internal(err)
	}
	if raw == nil {
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "route " + routeID + " was not found"}
	}
	if err := s.metadata.Delete(routesBucket, routeStoreKey(apiID, routeID)); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) DeleteStage(apiID, stageName string) error {
	raw, err := s.metadata.Get(stagesBucket, stageStoreKey(apiID, stageName))
	if err != nil {
		return internal(err)
	}
	if raw == nil {
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "stage " + stageName + " was not found"}
	}
	if err := s.metadata.Delete(stagesBucket, stageStoreKey(apiID, stageName)); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) createIntegration(w http.ResponseWriter, r *http.Request, apiID string) error {
	var input CreateIntegrationInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	record, err := s.CreateIntegration(apiID, input)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, integrationResponse(record))
	return nil
}

func (s *Service) CreateIntegration(apiID string, input CreateIntegrationInput) (IntegrationRecord, error) {
	if _, err := s.loadAPI(apiID); err != nil {
		return IntegrationRecord{}, err
	}
	if input.IntegrationType != "AWS_PROXY" {
		return IntegrationRecord{}, notImplemented("only AWS_PROXY integrations are supported")
	}
	if input.IntegrationURI == "" {
		return IntegrationRecord{}, validation("integrationUri is required")
	}
	record := IntegrationRecord{
		APIID:                apiID,
		CreatedAt:            s.now().UTC(),
		Description:          input.Description,
		IntegrationID:        shortID(),
		IntegrationMethod:    input.IntegrationMethod,
		IntegrationType:      input.IntegrationType,
		IntegrationURI:       input.IntegrationURI,
		PayloadFormatVersion: defaultString(input.PayloadFormatVersion, "2.0"),
		TimeoutInMillis:      defaultInt(input.TimeoutInMillis, 30000),
	}
	if err := s.putJSON(integrationsBucket, integrationStoreKey(apiID, record.IntegrationID), record); err != nil {
		return IntegrationRecord{}, err
	}
	return record, nil
}

func (s *Service) getIntegrations(w http.ResponseWriter, apiID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(integrationsBucket, apiID+"|", func(_, v []byte) error {
		var record IntegrationRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, integrationResponse(record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["integrationId"].(string) < items[j]["integrationId"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Service) getIntegration(w http.ResponseWriter, apiID, integrationID string) error {
	record, err := s.loadIntegration(apiID, integrationID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, integrationResponse(record))
	return nil
}

func (s *Service) createRoute(w http.ResponseWriter, r *http.Request, apiID string) error {
	var input CreateRouteInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	record, err := s.CreateRoute(apiID, input)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, routeResponse(record))
	return nil
}

func (s *Service) CreateRoute(apiID string, input CreateRouteInput) (RouteRecord, error) {
	if _, err := s.loadAPI(apiID); err != nil {
		return RouteRecord{}, err
	}
	if input.RouteKey == "" {
		return RouteRecord{}, validation("routeKey is required")
	}
	if input.Target == "" || !strings.HasPrefix(input.Target, "integrations/") {
		return RouteRecord{}, validation("target must reference an integration")
	}
	if _, err := s.loadIntegration(apiID, strings.TrimPrefix(input.Target, "integrations/")); err != nil {
		return RouteRecord{}, err
	}
	record := RouteRecord{
		APIID:             apiID,
		AuthorizationType: defaultString(input.AuthorizationType, "NONE"),
		CreatedAt:         s.now().UTC(),
		OperationName:     input.OperationName,
		RouteID:           shortID(),
		RouteKey:          input.RouteKey,
		Target:            input.Target,
	}
	if err := s.putJSON(routesBucket, routeStoreKey(apiID, record.RouteID), record); err != nil {
		return RouteRecord{}, err
	}
	return record, nil
}

func (s *Service) getRoutes(w http.ResponseWriter, apiID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(routesBucket, apiID+"|", func(_, v []byte) error {
		var record RouteRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, routeResponse(record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["routeId"].(string) < items[j]["routeId"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Service) getRoute(w http.ResponseWriter, apiID, routeID string) error {
	record, err := s.loadRoute(apiID, routeID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, routeResponse(record))
	return nil
}

func (s *Service) createStage(w http.ResponseWriter, r *http.Request, apiID string) error {
	var input CreateStageInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	record, err := s.CreateStage(apiID, input)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, stageResponse(record))
	return nil
}

func (s *Service) CreateStage(apiID string, input CreateStageInput) (StageRecord, error) {
	if _, err := s.loadAPI(apiID); err != nil {
		return StageRecord{}, err
	}
	if input.StageName == "" {
		return StageRecord{}, validation("stageName is required")
	}
	record := StageRecord{
		APIID:       apiID,
		AutoDeploy:  input.AutoDeploy,
		CreatedAt:   s.now().UTC(),
		Description: input.Description,
		StageName:   input.StageName,
		Variables:   cloneMap(input.StageVariables),
	}
	if err := s.putJSON(stagesBucket, stageStoreKey(apiID, record.StageName), record); err != nil {
		return StageRecord{}, err
	}
	return record, nil
}

func (s *Service) getStages(w http.ResponseWriter, apiID string) error {
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(stagesBucket, apiID+"|", func(_, v []byte) error {
		var record StageRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, stageResponse(record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["stageName"].(string) < items[j]["stageName"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (s *Service) getStage(w http.ResponseWriter, apiID, stageName string) error {
	record, err := s.loadStage(apiID, stageName)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, stageResponse(record))
	return nil
}

func (s *Service) invoke(w http.ResponseWriter, r *http.Request) error {
	if s.lambda == nil {
		return &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceUnavailableException",
			Message:    "lambda runtime is not configured",
		}
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/_aws/execute-api/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" {
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "api was not found"}
	}
	apiID := parts[0]
	if _, err := s.loadAPI(apiID); err != nil {
		return err
	}

	stages, err := s.listStageRecords(apiID)
	if err != nil {
		return err
	}
	stage, remaining, err := chooseStage(stages, parts[1:])
	if err != nil {
		return err
	}
	requestPath := "/" + strings.Join(remaining, "/")
	if requestPath == "/" && len(remaining) == 1 && remaining[0] == "" {
		requestPath = "/"
	}
	if requestPath == "//" {
		requestPath = "/"
	}

	routes, err := s.listRouteRecords(apiID)
	if err != nil {
		return err
	}
	route, pathParams, err := matchRoute(routes, r.Method, requestPath)
	if err != nil {
		return err
	}
	integrationID := strings.TrimPrefix(route.Target, "integrations/")
	integration, err := s.loadIntegration(apiID, integrationID)
	if err != nil {
		return err
	}
	functionName, err := functionNameFromIntegrationURI(integration.IntegrationURI)
	if err != nil {
		return err
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return validation("unable to read request body")
	}
	event := map[string]any{
		"version":        "2.0",
		"routeKey":       route.RouteKey,
		"rawPath":        requestPath,
		"rawQueryString": r.URL.RawQuery,
		"headers":        cloneHeaders(r.Header),
		"pathParameters": pathParams,
		"requestContext": map[string]any{
			"apiId":     apiID,
			"routeKey":  route.RouteKey,
			"stage":     stage.StageName,
			"requestId": uuid.NewString(),
			"http": map[string]any{
				"method":    r.Method,
				"path":      requestPath,
				"sourceIp":  clientIP(r),
				"userAgent": r.UserAgent(),
			},
		},
		"isBase64Encoded": false,
	}
	if len(body) > 0 {
		event["body"] = string(body)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return internal(err)
	}
	result, err := s.lambda.InvokeByName(r.Context(), functionName, payload)
	if err != nil {
		return err
	}
	return writeLambdaProxyResponse(w, result)
}

func (s *Service) loadAPI(apiID string) (APIRecord, error) {
	raw, err := s.metadata.Get(apisBucket, apiID)
	if err != nil {
		return APIRecord{}, internal(err)
	}
	if raw == nil {
		return APIRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "api " + apiID + " was not found"}
	}
	var record APIRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return APIRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadIntegration(apiID, integrationID string) (IntegrationRecord, error) {
	raw, err := s.metadata.Get(integrationsBucket, integrationStoreKey(apiID, integrationID))
	if err != nil {
		return IntegrationRecord{}, internal(err)
	}
	if raw == nil {
		return IntegrationRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "integration " + integrationID + " was not found"}
	}
	var record IntegrationRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return IntegrationRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadRoute(apiID, routeID string) (RouteRecord, error) {
	raw, err := s.metadata.Get(routesBucket, routeStoreKey(apiID, routeID))
	if err != nil {
		return RouteRecord{}, internal(err)
	}
	if raw == nil {
		return RouteRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "route " + routeID + " was not found"}
	}
	var record RouteRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return RouteRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadStage(apiID, stageName string) (StageRecord, error) {
	raw, err := s.metadata.Get(stagesBucket, stageStoreKey(apiID, stageName))
	if err != nil {
		return StageRecord{}, internal(err)
	}
	if raw == nil {
		return StageRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "stage " + stageName + " was not found"}
	}
	var record StageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return StageRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) listRouteRecords(apiID string) ([]RouteRecord, error) {
	routes := make([]RouteRecord, 0)
	if err := s.metadata.Scan(routesBucket, apiID+"|", func(_, v []byte) error {
		var record RouteRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		routes = append(routes, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].RouteKey < routes[j].RouteKey })
	return routes, nil
}

func (s *Service) listStageRecords(apiID string) ([]StageRecord, error) {
	stages := make([]StageRecord, 0)
	if err := s.metadata.Scan(stagesBucket, apiID+"|", func(_, v []byte) error {
		var record StageRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		stages = append(stages, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].StageName < stages[j].StageName })
	return stages, nil
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

func (s *Service) apiResponse(r *http.Request, record APIRecord) map[string]any {
	return map[string]any{
		"apiEndpoint":               apiEndpoint(r, record.APIID),
		"apiId":                     record.APIID,
		"apiKeySelectionExpression": record.APIKeySelectionExpression,
		"createdDate":               record.CreatedAt,
		"description":               record.Description,
		"disableExecuteApiEndpoint": record.DisableExecuteAPIEndpoint,
		"name":                      record.Name,
		"protocolType":              record.ProtocolType,
		"routeSelectionExpression":  record.RouteSelectionExpression,
		"tags":                      cloneMap(record.Tags),
	}
}

func integrationResponse(record IntegrationRecord) map[string]any {
	return map[string]any{
		"description":          record.Description,
		"integrationId":        record.IntegrationID,
		"integrationMethod":    record.IntegrationMethod,
		"integrationType":      record.IntegrationType,
		"integrationUri":       record.IntegrationURI,
		"payloadFormatVersion": record.PayloadFormatVersion,
		"timeoutInMillis":      record.TimeoutInMillis,
	}
}

func routeResponse(record RouteRecord) map[string]any {
	return map[string]any{
		"authorizationType": record.AuthorizationType,
		"operationName":     record.OperationName,
		"routeId":           record.RouteID,
		"routeKey":          record.RouteKey,
		"target":            record.Target,
	}
}

func stageResponse(record StageRecord) map[string]any {
	return map[string]any{
		"autoDeploy":     record.AutoDeploy,
		"createdDate":    record.CreatedAt,
		"description":    record.Description,
		"stageName":      record.StageName,
		"stageVariables": cloneMap(record.Variables),
	}
}

func apiEndpoint(r *http.Request, apiID string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/_aws/execute-api/%s", scheme, r.Host, apiID)
}

func functionNameFromIntegrationURI(uri string) (string, error) {
	switch {
	case strings.Contains(uri, ":function:"):
		segment := uri[strings.Index(uri, ":function:")+len(":function:"):]
		if idx := strings.Index(segment, ":"); idx >= 0 {
			segment = segment[:idx]
		}
		if idx := strings.Index(segment, "/"); idx >= 0 {
			segment = segment[:idx]
		}
		if segment == "" {
			return "", validation("integrationUri does not reference a lambda function")
		}
		return segment, nil
	default:
		return "", notImplemented("integrationUri must reference a lambda function")
	}
}

func chooseStage(stages []StageRecord, remaining []string) (StageRecord, []string, error) {
	if len(stages) == 0 {
		return StageRecord{}, nil, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "api has no deployed stages"}
	}
	byName := make(map[string]StageRecord, len(stages))
	var defaultStage *StageRecord
	for _, stage := range stages {
		byName[stage.StageName] = stage
		if stage.StageName == "$default" {
			stageCopy := stage
			defaultStage = &stageCopy
		}
	}
	if len(remaining) > 0 {
		if stage, ok := byName[remaining[0]]; ok && remaining[0] != "" {
			return stage, remaining[1:], nil
		}
	}
	if defaultStage != nil {
		return *defaultStage, remaining, nil
	}
	return StageRecord{}, nil, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "request did not match a deployed stage"}
}

func matchRoute(routes []RouteRecord, method, requestPath string) (RouteRecord, map[string]string, error) {
	type candidate struct {
		record     RouteRecord
		pathParams map[string]string
		score      int
	}
	best := candidate{score: -1}
	for _, route := range routes {
		score, params, ok := routeMatch(route.RouteKey, method, requestPath)
		if ok && score > best.score {
			best = candidate{record: route, pathParams: params, score: score}
		}
	}
	if best.score < 0 {
		return RouteRecord{}, nil, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "route was not found"}
	}
	return best.record, best.pathParams, nil
}

func routeMatch(routeKey, method, requestPath string) (int, map[string]string, bool) {
	if routeKey == "$default" {
		return 0, nil, true
	}
	parts := strings.SplitN(routeKey, " ", 2)
	if len(parts) != 2 {
		return 0, nil, false
	}
	routeMethod, routePath := parts[0], parts[1]
	if routeMethod != "ANY" && routeMethod != method {
		return 0, nil, false
	}
	if routePath == requestPath {
		return 3, nil, true
	}
	if strings.HasSuffix(routePath, "/{proxy+}") {
		prefix := strings.TrimSuffix(routePath, "{proxy+}")
		if strings.HasSuffix(prefix, "/") {
			prefix = strings.TrimSuffix(prefix, "/")
		}
		if prefix == "" {
			prefix = "/"
		}
		if prefix == "/" {
			if requestPath == "/" {
				return 2, map[string]string{"proxy": ""}, true
			}
			return 2, map[string]string{"proxy": strings.TrimPrefix(requestPath, "/")}, true
		}
		if requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/") {
			return 2, map[string]string{"proxy": strings.TrimPrefix(strings.TrimPrefix(requestPath, prefix), "/")}, true
		}
	}
	return 0, nil, false
}

func writeLambdaProxyResponse(w http.ResponseWriter, result lambdasvc.InvokeResult) error {
	if result.FunctionError != "" {
		return &apierror.Error{
			StatusCode: http.StatusBadGateway,
			Code:       "IntegrationFailure",
			Message:    "lambda integration returned a function error",
		}
	}

	var payload struct {
		StatusCode      int               `json:"statusCode"`
		Headers         map[string]string `json:"headers"`
		Body            string            `json:"body"`
		IsBase64Encoded bool              `json:"isBase64Encoded"`
	}
	if err := json.Unmarshal(result.Payload, &payload); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Payload)
		return nil
	}
	if payload.StatusCode == 0 {
		payload.StatusCode = http.StatusOK
	}
	for key, value := range payload.Headers {
		w.Header().Set(key, value)
	}
	body := []byte(payload.Body)
	if payload.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(payload.Body)
		if err != nil {
			return validation("lambda proxy response contained invalid base64")
		}
		body = decoded
	}
	w.WriteHeader(payload.StatusCode)
	_, _ = io.Copy(w, bytes.NewReader(body))
	return nil
}

func integrationStoreKey(apiID, integrationID string) string {
	return apiID + "|" + integrationID
}

func routeStoreKey(apiID, routeID string) string {
	return apiID + "|" + routeID
}

func stageStoreKey(apiID, stageName string) string {
	return apiID + "|" + stageName
}

func splitPath(path string) []string {
	trimmed := strings.Trim(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func shortID() string {
	return strings.ToLower(strings.ReplaceAll(uuid.NewString(), "-", ""))[:10]
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneHeaders(in http.Header) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, values := range in {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	return host
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "BadRequestException", Message: message}
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
