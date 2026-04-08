package secretsmanager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	secretsBucket = "secretsmanager-secrets"
	accountID     = "000000000000"
	region        = "us-east-1"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type CreateSecretInput struct {
	Description  string
	KMSKeyID     string
	Name         string
	SecretBinary []byte
	SecretString string
}

type secretRecord struct {
	ARN             string    `json:"arn"`
	CreatedDate     time.Time `json:"created_date"`
	Description     string    `json:"description,omitempty"`
	KMSKeyID        string    `json:"kms_key_id,omitempty"`
	LastChangedDate time.Time `json:"last_changed_date"`
	Name            string    `json:"name"`
	SecretBinary    []byte    `json:"secret_binary,omitempty"`
	SecretString    string    `json:"secret_string,omitempty"`
	VersionID       string    `json:"version_id"`
}

type createSecretInput struct {
	ClientRequestToken string `json:"ClientRequestToken"`
	Description        string `json:"Description"`
	KmsKeyID           string `json:"KmsKeyId"`
	Name               string `json:"Name"`
	SecretBinary       []byte `json:"SecretBinary"`
	SecretString       string `json:"SecretString"`
}

type describeSecretInput struct {
	SecretID string `json:"SecretId"`
}

type getSecretValueInput struct {
	SecretID     string `json:"SecretId"`
	VersionID    string `json:"VersionId"`
	VersionStage string `json:"VersionStage"`
}

type updateSecretInput struct {
	ClientRequestToken string `json:"ClientRequestToken"`
	Description        string `json:"Description"`
	KmsKeyID           string `json:"KmsKeyId"`
	SecretBinary       []byte `json:"SecretBinary"`
	SecretID           string `json:"SecretId"`
	SecretString       string `json:"SecretString"`
}

type deleteSecretInput struct {
	ForceDeleteWithoutRecovery bool   `json:"ForceDeleteWithoutRecovery"`
	RecoveryWindowInDays       int    `json:"RecoveryWindowInDays"`
	SecretID                   string `json:"SecretId"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateSecret":
		return s.createSecret(w, r)
	case "DescribeSecret":
		return s.describeSecret(w, r)
	case "GetSecretValue":
		return s.getSecretValue(w, r)
	case "ListSecrets":
		return s.listSecrets(w)
	case "UpdateSecret":
		return s.updateSecret(w, r)
	case "DeleteSecret":
		return s.deleteSecret(w, r)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "secretsmanager operation is not implemented",
		}
	}
}

func (s *Service) createSecret(w http.ResponseWriter, r *http.Request) error {
	var input createSecretInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.Name == "" {
		return validation("Name is required")
	}
	if _, err := s.loadSecret(input.Name); err == nil {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ResourceExistsException",
			Message:    "A resource with the ID you requested already exists.",
		}
	}
	versionID := versionID(input.ClientRequestToken)
	now := s.now().UTC()
	record := secretRecord{
		ARN:             secretARN(input.Name),
		CreatedDate:     now,
		Description:     input.Description,
		KMSKeyID:        input.KmsKeyID,
		LastChangedDate: now,
		Name:            input.Name,
		SecretBinary:    cloneBytes(input.SecretBinary),
		SecretString:    input.SecretString,
		VersionID:       versionID,
	}
	if err := s.putSecret(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ARN":       record.ARN,
		"Name":      record.Name,
		"VersionId": record.VersionID,
	})
	return nil
}

func (s *Service) CreateSecret(input CreateSecretInput) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.Name == "" {
		return "", validation("Name is required")
	}
	if _, err := s.loadSecret(input.Name); err == nil {
		return "", &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ResourceExistsException",
			Message:    "A resource with the ID you requested already exists.",
		}
	}
	versionID := versionID("")
	now := s.now().UTC()
	record := secretRecord{
		ARN:             secretARN(input.Name),
		CreatedDate:     now,
		Description:     input.Description,
		KMSKeyID:        input.KMSKeyID,
		LastChangedDate: now,
		Name:            input.Name,
		SecretBinary:    cloneBytes(input.SecretBinary),
		SecretString:    input.SecretString,
		VersionID:       versionID,
	}
	if err := s.putSecret(record); err != nil {
		return "", internal(err)
	}
	return record.ARN, nil
}

func (s *Service) DeleteSecret(secretID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, err := s.resolveSecret(secretID)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(secretsBucket, record.Name); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) describeSecret(w http.ResponseWriter, r *http.Request) error {
	var input describeSecretInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveSecret(input.SecretID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ARN":             record.ARN,
		"CreatedDate":     record.CreatedDate.Unix(),
		"Description":     record.Description,
		"KmsKeyId":        record.KMSKeyID,
		"LastChangedDate": record.LastChangedDate.Unix(),
		"Name":            record.Name,
		"VersionIdsToStages": map[string][]string{
			record.VersionID: {"AWSCURRENT"},
		},
	})
	return nil
}

func (s *Service) getSecretValue(w http.ResponseWriter, r *http.Request) error {
	var input getSecretValueInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveSecret(input.SecretID)
	if err != nil {
		return err
	}
	if input.VersionID != "" && input.VersionID != record.VersionID {
		return validation("requested VersionId is not available")
	}
	if input.VersionStage != "" && input.VersionStage != "AWSCURRENT" {
		return validation("only AWSCURRENT is supported")
	}
	resp := map[string]any{
		"ARN":           record.ARN,
		"CreatedDate":   record.LastChangedDate.Unix(),
		"Name":          record.Name,
		"VersionId":     record.VersionID,
		"VersionStages": []string{"AWSCURRENT"},
	}
	if len(record.SecretBinary) > 0 {
		resp["SecretBinary"] = record.SecretBinary
	}
	if record.SecretString != "" {
		resp["SecretString"] = record.SecretString
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Service) listSecrets(w http.ResponseWriter) error {
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(secretsBucket, "", func(_, v []byte) error {
		var record secretRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"ARN":             record.ARN,
			"CreatedDate":     record.CreatedDate.Unix(),
			"Description":     record.Description,
			"LastChangedDate": record.LastChangedDate.Unix(),
			"Name":            record.Name,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["Name"].(string) < items[j]["Name"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"SecretList": items})
	return nil
}

func (s *Service) updateSecret(w http.ResponseWriter, r *http.Request) error {
	var input updateSecretInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveSecret(input.SecretID)
	if err != nil {
		return err
	}
	if input.Description != "" {
		record.Description = input.Description
	}
	if input.KmsKeyID != "" {
		record.KMSKeyID = input.KmsKeyID
	}
	if input.SecretString != "" || len(input.SecretBinary) > 0 {
		record.SecretString = input.SecretString
		record.SecretBinary = cloneBytes(input.SecretBinary)
		record.VersionID = versionID(input.ClientRequestToken)
	}
	record.LastChangedDate = s.now().UTC()
	if err := s.putSecret(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ARN":       record.ARN,
		"Name":      record.Name,
		"VersionId": record.VersionID,
	})
	return nil
}

func (s *Service) deleteSecret(w http.ResponseWriter, r *http.Request) error {
	var input deleteSecretInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveSecret(input.SecretID)
	if err != nil {
		return err
	}
	if !input.ForceDeleteWithoutRecovery {
		return notImplemented("only force delete without recovery is supported")
	}
	if input.RecoveryWindowInDays != 0 {
		return notImplemented("custom recovery windows are not supported")
	}
	if err := s.metadata.Delete(secretsBucket, record.Name); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ARN":          record.ARN,
		"DeletionDate": s.now().UTC().Unix(),
		"Name":         record.Name,
	})
	return nil
}

func (s *Service) resolveSecret(secretID string) (secretRecord, error) {
	if secretID == "" {
		return secretRecord{}, validation("SecretId is required")
	}
	if strings.Contains(secretID, ":secret:") {
		parts := strings.Split(secretID, ":secret:")
		if len(parts) != 2 {
			return secretRecord{}, notFound()
		}
		baseName := secretNameFromARNSecret(parts[1])
		return s.loadSecret(baseName)
	}
	return s.loadSecret(secretID)
}

func (s *Service) loadSecret(name string) (secretRecord, error) {
	raw, err := s.metadata.Get(secretsBucket, name)
	if err != nil {
		return secretRecord{}, internal(err)
	}
	if raw == nil {
		return secretRecord{}, notFound()
	}
	var record secretRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return secretRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putSecret(record secretRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(secretsBucket, record.Name, raw)
}

func secretARN(name string) string {
	suffix := strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	return fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:%s-%s", region, accountID, name, suffix)
}

func secretNameFromARNSecret(value string) string {
	if idx := strings.LastIndex(value, "-"); idx > 0 {
		return value[:idx]
	}
	return value
}

func versionID(candidate string) string {
	if candidate != "" {
		return candidate
	}
	return uuid.NewString()
}

func cloneBytes(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func decodeJSON(r *http.Request, target any) error {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
}

func notFound() error {
	return &apierror.Error{
		StatusCode: http.StatusBadRequest,
		Code:       "ResourceNotFoundException",
		Message:    "Secrets Manager can't find the specified secret.",
	}
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServiceError", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
