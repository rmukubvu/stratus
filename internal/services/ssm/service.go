package ssm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const parametersBucket = "ssm-parameters"

type Service struct {
	metadata store.Store
	now      func() time.Time
}

type PutParameterInput struct {
	Name  string
	Type  string
	Value string
}

type parameterRecord struct {
	Name             string    `json:"name"`
	Type             string    `json:"type"`
	Value            string    `json:"value"`
	Version          int64     `json:"version"`
	Description      string    `json:"description,omitempty"`
	LastModifiedTime time.Time `json:"last_modified_time"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	switch operation {
	case "PutParameter":
		return s.putParameter(w, r)
	case "GetParameter":
		return s.getParameter(w, r)
	case "DeleteParameter":
		return s.deleteParameter(w, r)
	case "DescribeParameters":
		return s.describeParameters(w)
	case "ListTagsForResource":
		return s.listTagsForResource(w, r)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "ssm operation is not implemented",
		}
	}
}

func (s *Service) PutParameter(input PutParameterInput) error {
	if input.Name == "" {
		return badRequest("ValidationException", "Name is required")
	}
	if input.Type == "" {
		input.Type = "String"
	}
	if input.Type != "String" {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "only String parameters are supported",
		}
	}
	version := int64(1)
	if existing, err := s.load(input.Name); err == nil {
		version = existing.Version + 1
	}
	record := parameterRecord{
		Name:             input.Name,
		Type:             input.Type,
		Value:            input.Value,
		Version:          version,
		LastModifiedTime: s.now().UTC(),
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(parametersBucket, input.Name, raw); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) DeleteParameterByName(name string) error {
	if err := s.metadata.Delete(parametersBucket, name); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) putParameter(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Name        string `json:"Name"`
		Value       string `json:"Value"`
		Type        string `json:"Type"`
		Overwrite   bool   `json:"Overwrite"`
		Description string `json:"Description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequest", "request body is not valid JSON")
	}
	if input.Name == "" || input.Value == "" {
		return badRequest("ValidationException", "Name and Value are required")
	}
	if input.Type == "" {
		input.Type = "String"
	}

	record, err := s.load(input.Name)
	if err != nil && !isNotFound(err) {
		return err
	}
	if err == nil && !input.Overwrite {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ParameterAlreadyExists",
			Message:    "The parameter already exists. To overwrite this value, set overwrite to true.",
		}
	}

	version := int64(1)
	if err == nil {
		version = record.Version + 1
	}

	next := parameterRecord{
		Name:             input.Name,
		Type:             input.Type,
		Value:            input.Value,
		Version:          version,
		Description:      input.Description,
		LastModifiedTime: s.now().UTC(),
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(parametersBucket, input.Name, raw); err != nil {
		return internal(err)
	}

	writeJSON(w, http.StatusOK, map[string]int64{"Version": version})
	return nil
}

func (s *Service) getParameter(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Name string `json:"Name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequest", "request body is not valid JSON")
	}
	record, err := s.load(input.Name)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"Parameter": map[string]any{
			"Name":             record.Name,
			"Type":             record.Type,
			"Value":            record.Value,
			"Version":          record.Version,
			"LastModifiedDate": float64(record.LastModifiedTime.Unix()),
			"ARN":              fmt.Sprintf("arn:aws:ssm:us-east-1:000000000000:parameter%s", record.Name),
			"DataType":         "text",
		},
	})
	return nil
}

func (s *Service) deleteParameter(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Name string `json:"Name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequest", "request body is not valid JSON")
	}
	if input.Name == "" {
		return badRequest("ValidationException", "Name is required")
	}
	if err := s.metadata.Delete(parametersBucket, input.Name); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) describeParameters(w http.ResponseWriter) error {
	var parameters []map[string]any
	if err := s.metadata.Scan(parametersBucket, "", func(_, v []byte) error {
		var record parameterRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		parameters = append(parameters, map[string]any{
			"Name":             record.Name,
			"Type":             record.Type,
			"Version":          record.Version,
			"LastModifiedDate": float64(record.LastModifiedTime.Unix()),
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(parameters, func(i, j int) bool {
		return parameters[i]["Name"].(string) < parameters[j]["Name"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"Parameters": parameters,
	})
	return nil
}

func (s *Service) listTagsForResource(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ResourceID   string `json:"ResourceId"`
		ResourceType string `json:"ResourceType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequest", "request body is not valid JSON")
	}
	if input.ResourceID == "" {
		return badRequest("ValidationException", "ResourceId is required")
	}
	if input.ResourceType != "" && input.ResourceType != "Parameter" {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "only Parameter resource tags are supported",
		}
	}
	if _, err := s.load(input.ResourceID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"TagList": []map[string]string{},
	})
	return nil
}

func (s *Service) load(name string) (parameterRecord, error) {
	raw, err := s.metadata.Get(parametersBucket, name)
	if err != nil {
		return parameterRecord{}, internal(err)
	}
	if raw == nil {
		return parameterRecord{}, &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ParameterNotFound",
			Message:    "Parameter " + name + " not found.",
		}
	}
	var record parameterRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return parameterRecord{}, internal(err)
	}
	return record, nil
}

func isNotFound(err error) bool {
	apiErr, ok := err.(*apierror.Error)
	return ok && apiErr.Code == "ParameterNotFound"
}

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerError", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
