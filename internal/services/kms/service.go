package kms

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	keysBucket    = "kms-keys"
	aliasesBucket = "kms-aliases"
	accountID     = "000000000000"
	region        = "us-east-1"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
}

type CreateKeyInput struct {
	Description string
	KeySpec     string
	KeyUsage    string
	MultiRegion bool
	Policy      string
}

type keyRecord struct {
	Arn         string    `json:"arn"`
	CreatedAt   time.Time `json:"created_at"`
	Description string    `json:"description,omitempty"`
	Enabled     bool      `json:"enabled"`
	KeyID       string    `json:"key_id"`
	KeySpec     string    `json:"key_spec"`
	KeyState    string    `json:"key_state"`
	KeyUsage    string    `json:"key_usage"`
	MultiRegion bool      `json:"multi_region"`
	Policy      string    `json:"policy,omitempty"`
}

type aliasRecord struct {
	AliasArn    string    `json:"alias_arn"`
	AliasName   string    `json:"alias_name"`
	CreatedAt   time.Time `json:"created_at"`
	TargetKeyID string    `json:"target_key_id"`
}

type createKeyInput struct {
	Description string `json:"Description"`
	KeySpec     string `json:"KeySpec"`
	KeyUsage    string `json:"KeyUsage"`
	MultiRegion bool   `json:"MultiRegion"`
	Policy      string `json:"Policy"`
}

type describeKeyInput struct {
	KeyID string `json:"KeyId"`
}

type createAliasInput struct {
	AliasName   string `json:"AliasName"`
	TargetKeyID string `json:"TargetKeyId"`
}

type encryptInput struct {
	KeyID     string `json:"KeyId"`
	Plaintext []byte `json:"Plaintext"`
}

type decryptInput struct {
	CiphertextBlob []byte `json:"CiphertextBlob"`
	KeyID          string `json:"KeyId"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	switch operation {
	case "CreateKey":
		return s.createKey(w, r)
	case "DescribeKey":
		return s.describeKey(w, r)
	case "GetKeyPolicy":
		return s.getKeyPolicy(w, r)
	case "GetKeyRotationStatus":
		return s.getKeyRotationStatus(w, r)
	case "ListResourceTags":
		return s.listResourceTags(w, r)
	case "ListKeys":
		return s.listKeys(w)
	case "CreateAlias":
		return s.createAlias(w, r)
	case "ListAliases":
		return s.listAliases(w)
	case "Encrypt":
		return s.encrypt(w, r)
	case "Decrypt":
		return s.decrypt(w, r)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "kms operation is not implemented",
		}
	}
}

func (s *Service) createKey(w http.ResponseWriter, r *http.Request) error {
	var input createKeyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if input.KeySpec == "" {
		input.KeySpec = "SYMMETRIC_DEFAULT"
	}
	if input.KeyUsage == "" {
		input.KeyUsage = "ENCRYPT_DECRYPT"
	}
	if input.KeySpec != "SYMMETRIC_DEFAULT" || input.KeyUsage != "ENCRYPT_DECRYPT" {
		return notImplemented("only symmetric ENCRYPT_DECRYPT KMS keys are supported")
	}

	keyID := uuid.NewString()
	record := keyRecord{
		Arn:         keyARN(keyID),
		CreatedAt:   s.now().UTC(),
		Description: input.Description,
		Enabled:     true,
		KeyID:       keyID,
		KeySpec:     input.KeySpec,
		KeyState:    "Enabled",
		KeyUsage:    input.KeyUsage,
		MultiRegion: input.MultiRegion,
		Policy:      input.Policy,
	}
	if err := s.putKey(record); err != nil {
		return err
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"KeyMetadata": s.keyMetadata(record),
	})
	return nil
}

func (s *Service) CreateKey(input CreateKeyInput) (string, string, error) {
	if input.KeySpec == "" {
		input.KeySpec = "SYMMETRIC_DEFAULT"
	}
	if input.KeyUsage == "" {
		input.KeyUsage = "ENCRYPT_DECRYPT"
	}
	if input.KeySpec != "SYMMETRIC_DEFAULT" || input.KeyUsage != "ENCRYPT_DECRYPT" {
		return "", "", notImplemented("only symmetric ENCRYPT_DECRYPT KMS keys are supported")
	}
	keyID := uuid.NewString()
	record := keyRecord{
		Arn:         keyARN(keyID),
		CreatedAt:   s.now().UTC(),
		Description: input.Description,
		Enabled:     true,
		KeyID:       keyID,
		KeySpec:     input.KeySpec,
		KeyState:    "Enabled",
		KeyUsage:    input.KeyUsage,
		MultiRegion: input.MultiRegion,
		Policy:      input.Policy,
	}
	if err := s.putKey(record); err != nil {
		return "", "", err
	}
	return keyID, record.Arn, nil
}

func (s *Service) CreateAlias(aliasName, targetKeyID string) error {
	if err := validateAliasName(aliasName); err != nil {
		return err
	}
	record, err := s.resolveKeyReference(targetKeyID)
	if err != nil {
		return err
	}
	existing, err := s.metadata.Get(aliasesBucket, aliasName)
	if err != nil {
		return internal(err)
	}
	if existing != nil {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "AlreadyExistsException",
			Message:    "An alias with the name " + aliasName + " already exists",
		}
	}
	alias := aliasRecord{
		AliasArn:    aliasARN(aliasName),
		AliasName:   aliasName,
		CreatedAt:   s.now().UTC(),
		TargetKeyID: record.KeyID,
	}
	payload, err := json.Marshal(alias)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(aliasesBucket, aliasName, payload); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) DeleteAlias(aliasName string) error {
	if err := s.metadata.Delete(aliasesBucket, aliasName); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) DeleteKey(keyID string) error {
	record, err := s.resolveKeyReference(keyID)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(keysBucket, record.KeyID); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) describeKey(w http.ResponseWriter, r *http.Request) error {
	var input describeKeyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"KeyMetadata": s.keyMetadata(record),
	})
	return nil
}

func (s *Service) getKeyPolicy(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		KeyID      string `json:"KeyId"`
		PolicyName string `json:"PolicyName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if input.PolicyName == "" {
		input.PolicyName = "default"
	}
	if input.PolicyName != "default" {
		return notImplemented("only the default key policy is supported")
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return err
	}
	policy := record.Policy
	if policy == "" {
		policy = defaultKeyPolicy()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Policy":     policy,
		"PolicyName": input.PolicyName,
	})
	return nil
}

func (s *Service) getKeyRotationStatus(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		KeyID string `json:"KeyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if _, err := s.resolveKeyReference(input.KeyID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"KeyRotationEnabled": false,
	})
	return nil
}

func (s *Service) listResourceTags(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		KeyID string `json:"KeyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if _, err := s.resolveKeyReference(input.KeyID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Tags":      []map[string]string{},
		"Truncated": false,
	})
	return nil
}

func (s *Service) listKeys(w http.ResponseWriter) error {
	type keyListEntry struct {
		KeyArn string `json:"KeyArn"`
		KeyID  string `json:"KeyId"`
	}

	keys := make([]keyListEntry, 0)
	if err := s.metadata.Scan(keysBucket, "", func(_, v []byte) error {
		var record keyRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		keys = append(keys, keyListEntry{KeyArn: record.Arn, KeyID: record.KeyID})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].KeyID < keys[j].KeyID })

	writeJSON(w, http.StatusOK, map[string]any{
		"Keys":      keys,
		"Truncated": false,
	})
	return nil
}

func (s *Service) createAlias(w http.ResponseWriter, r *http.Request) error {
	var input createAliasInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if err := validateAliasName(input.AliasName); err != nil {
		return err
	}
	record, err := s.resolveKeyReference(input.TargetKeyID)
	if err != nil {
		return err
	}
	existing, err := s.metadata.Get(aliasesBucket, input.AliasName)
	if err != nil {
		return internal(err)
	}
	if existing != nil {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "AlreadyExistsException",
			Message:    "An alias with the name " + input.AliasName + " already exists",
		}
	}
	alias := aliasRecord{
		AliasArn:    aliasARN(input.AliasName),
		AliasName:   input.AliasName,
		CreatedAt:   s.now().UTC(),
		TargetKeyID: record.KeyID,
	}
	payload, err := json.Marshal(alias)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(aliasesBucket, input.AliasName, payload); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) listAliases(w http.ResponseWriter) error {
	aliases := make([]map[string]any, 0)
	if err := s.metadata.Scan(aliasesBucket, "", func(_, v []byte) error {
		var record aliasRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		key, err := s.loadKey(record.TargetKeyID)
		if err != nil {
			return nil
		}
		aliases = append(aliases, map[string]any{
			"AliasArn":     record.AliasArn,
			"AliasName":    record.AliasName,
			"TargetKeyId":  record.TargetKeyID,
			"TargetKeyArn": key.Arn,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(aliases, func(i, j int) bool {
		return aliases[i]["AliasName"].(string) < aliases[j]["AliasName"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"Aliases":   aliases,
		"Truncated": false,
	})
	return nil
}

func (s *Service) encrypt(w http.ResponseWriter, r *http.Request) error {
	var input encryptInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if len(input.Plaintext) == 0 {
		return validation("Plaintext is required")
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return err
	}
	ciphertext := []byte("stratuskms:v1:" + record.KeyID + ":" + base64.StdEncoding.EncodeToString(input.Plaintext))
	writeJSON(w, http.StatusOK, map[string]any{
		"CiphertextBlob":      ciphertext,
		"EncryptionAlgorithm": "SYMMETRIC_DEFAULT",
		"KeyId":               record.Arn,
	})
	return nil
}

func (s *Service) decrypt(w http.ResponseWriter, r *http.Request) error {
	var input decryptInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	parts := strings.SplitN(string(input.CiphertextBlob), ":", 4)
	if len(parts) != 4 || parts[0] != "stratuskms" || parts[1] != "v1" {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "InvalidCiphertextException",
			Message:    "ciphertext blob is not valid",
		}
	}
	record, err := s.loadKey(parts[2])
	if err != nil {
		return err
	}
	if input.KeyID != "" {
		requested, err := s.resolveKeyReference(input.KeyID)
		if err != nil {
			return err
		}
		if requested.KeyID != record.KeyID {
			return &apierror.Error{
				StatusCode: http.StatusBadRequest,
				Code:       "IncorrectKeyException",
				Message:    "ciphertext was encrypted under a different key",
			}
		}
	}
	plaintext, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "InvalidCiphertextException",
			Message:    "ciphertext blob is not valid",
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"KeyId":               record.Arn,
		"Plaintext":           plaintext,
		"EncryptionAlgorithm": "SYMMETRIC_DEFAULT",
	})
	return nil
}

func (s *Service) putKey(record keyRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(keysBucket, record.KeyID, payload); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) loadKey(keyID string) (keyRecord, error) {
	raw, err := s.metadata.Get(keysBucket, keyID)
	if err != nil {
		return keyRecord{}, internal(err)
	}
	if raw == nil {
		return keyRecord{}, &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "NotFoundException",
			Message:    "key " + keyID + " does not exist",
		}
	}
	var record keyRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return keyRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) resolveKeyReference(ref string) (keyRecord, error) {
	switch {
	case ref == "":
		return keyRecord{}, validation("KeyId is required")
	case strings.HasPrefix(ref, "alias/"):
		return s.loadKeyByAlias(ref)
	case strings.Contains(ref, ":alias/"):
		return s.loadKeyByAlias(ref[strings.Index(ref, ":alias/")+1:])
	case strings.Contains(ref, ":key/"):
		return s.loadKey(ref[strings.LastIndex(ref, "/")+1:])
	default:
		return s.loadKey(ref)
	}
}

func (s *Service) loadKeyByAlias(aliasName string) (keyRecord, error) {
	if aliasName == "" {
		return keyRecord{}, validation("AliasName is required")
	}
	raw, err := s.metadata.Get(aliasesBucket, aliasName)
	if err != nil {
		return keyRecord{}, internal(err)
	}
	if raw == nil {
		return keyRecord{}, &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "NotFoundException",
			Message:    "alias " + aliasName + " does not exist",
		}
	}
	var alias aliasRecord
	if err := json.Unmarshal(raw, &alias); err != nil {
		return keyRecord{}, internal(err)
	}
	return s.loadKey(alias.TargetKeyID)
}

func (s *Service) keyMetadata(record keyRecord) map[string]any {
	algorithms := []string{}
	if record.KeySpec == "SYMMETRIC_DEFAULT" {
		algorithms = []string{"SYMMETRIC_DEFAULT"}
	}
	return map[string]any{
		"AWSAccountId":         accountID,
		"Arn":                  record.Arn,
		"CreationDate":         float64(record.CreatedAt.Unix()),
		"Description":          record.Description,
		"Enabled":              record.Enabled,
		"EncryptionAlgorithms": algorithms,
		"KeyId":                record.KeyID,
		"KeyManager":           "CUSTOMER",
		"KeySpec":              record.KeySpec,
		"KeyState":             record.KeyState,
		"KeyUsage":             record.KeyUsage,
		"MultiRegion":          record.MultiRegion,
		"Origin":               "AWS_KMS",
	}
}

func keyARN(keyID string) string {
	return fmt.Sprintf("arn:aws:kms:%s:%s:key/%s", region, accountID, keyID)
}

func aliasARN(aliasName string) string {
	return fmt.Sprintf("arn:aws:kms:%s:%s:%s", region, accountID, aliasName)
}

func defaultKeyPolicy() string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"EnableRoot","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::%s:root"},"Action":"kms:*","Resource":"*"}]}`, accountID)
}

func validateAliasName(name string) error {
	if name == "" {
		return validation("AliasName is required")
	}
	if !strings.HasPrefix(name, "alias/") {
		return validation("AliasName must begin with alias/")
	}
	if strings.HasPrefix(name, "alias/aws/") {
		return validation("AliasName cannot begin with alias/aws/")
	}
	if strings.Contains(name, " ") {
		return validation("AliasName is not valid")
	}
	return nil
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "DependencyTimeoutException", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
