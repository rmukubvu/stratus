package kms

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
)

const (
	keysBucket    = "kms-keys"
	aliasesBucket = "kms-aliases"
)

type Service struct {
	scope    scope.Scope
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
	// Material is the private key material: AES-256 bytes for symmetric
	// keys, see generateMaterial for asymmetric ones. Keys created before
	// material existed get it lazily on first cryptographic use.
	Material []byte `json:"material,omitempty"`
}

type aliasRecord struct {
	AliasArn    string    `json:"alias_arn"`
	AliasName   string    `json:"alias_name"`
	CreatedAt   time.Time `json:"created_at"`
	TargetKeyID string    `json:"target_key_id"`
}

type createKeyInput struct {
	Description string `json:"Description"`
	// CustomerMasterKeySpec is the deprecated name of KeySpec.
	CustomerMasterKeySpec string `json:"CustomerMasterKeySpec"`
	KeySpec               string `json:"KeySpec"`
	KeyUsage              string `json:"KeyUsage"`
	MultiRegion           bool   `json:"MultiRegion"`
	Policy                string `json:"Policy"`
}

type describeKeyInput struct {
	KeyID string `json:"KeyId"`
}

type createAliasInput struct {
	AliasName   string `json:"AliasName"`
	TargetKeyID string `json:"TargetKeyId"`
}

func NewService(metadata store.Store) *Service {
	return &Service{scope: scope.Default(), metadata: metadata, now: time.Now}
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
	case "GenerateDataKey":
		return s.generateDataKey(w, r, true)
	case "GenerateDataKeyWithoutPlaintext":
		return s.generateDataKey(w, r, false)
	case "GenerateRandom":
		return s.generateRandom(w, r)
	case "Sign":
		return s.sign(w, r)
	case "Verify":
		return s.verify(w, r)
	case "GetPublicKey":
		return s.getPublicKey(w, r)
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
		input.KeySpec = input.CustomerMasterKeySpec
	}
	record, err := s.newKey(CreateKeyInput{
		Description: input.Description,
		KeySpec:     input.KeySpec,
		KeyUsage:    input.KeyUsage,
		MultiRegion: input.MultiRegion,
		Policy:      input.Policy,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"KeyMetadata": s.keyMetadata(record),
	})
	return nil
}

// CreateKey creates a key for in-process callers such as CloudFormation and
// returns its ID and ARN.
func (s *Service) CreateKey(input CreateKeyInput) (string, string, error) {
	record, err := s.newKey(input)
	if err != nil {
		return "", "", err
	}
	return record.KeyID, record.Arn, nil
}

// newKey validates the spec/usage pair, generates key material and stores
// the key. KeySpec defaults to SYMMETRIC_DEFAULT and KeyUsage to
// ENCRYPT_DECRYPT, as in AWS, so an asymmetric signing key must name
// SIGN_VERIFY explicitly.
func (s *Service) newKey(input CreateKeyInput) (keyRecord, error) {
	if input.KeySpec == "" {
		input.KeySpec = specSymmetricDefault
	}
	if input.KeyUsage == "" {
		input.KeyUsage = usageEncryptDecrypt
	}
	spec, err := resolveKeySpec(input.KeySpec, input.KeyUsage)
	if err != nil {
		return keyRecord{}, err
	}
	material, err := generateMaterial(spec)
	if err != nil {
		return keyRecord{}, internal(err)
	}
	keyID := uuid.NewString()
	record := keyRecord{
		Arn:         s.keyARN(keyID),
		CreatedAt:   s.now().UTC(),
		Description: input.Description,
		Enabled:     true,
		KeyID:       keyID,
		KeySpec:     spec.name,
		KeyState:    "Enabled",
		KeyUsage:    input.KeyUsage,
		MultiRegion: input.MultiRegion,
		Policy:      input.Policy,
		Material:    material,
	}
	if err := s.putKey(record); err != nil {
		return keyRecord{}, err
	}
	return record, nil
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
		AliasArn:    s.aliasARN(aliasName),
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
		policy = s.defaultKeyPolicy()
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
		AliasArn:    s.aliasARN(input.AliasName),
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
	spec := keySpecs[record.KeySpec]
	metadata := map[string]any{
		"AWSAccountId":          s.scope.Account,
		"Arn":                   record.Arn,
		"CreationDate":          float64(record.CreatedAt.Unix()),
		"CustomerMasterKeySpec": record.KeySpec,
		"Description":           record.Description,
		"Enabled":               record.Enabled,
		"KeyId":                 record.KeyID,
		"KeyManager":            "CUSTOMER",
		"KeySpec":               record.KeySpec,
		"KeyState":              record.KeyState,
		"KeyUsage":              record.KeyUsage,
		"MultiRegion":           record.MultiRegion,
		"Origin":                "AWS_KMS",
	}
	if algorithms := spec.encryptionAlgorithmsFor(record.KeyUsage); len(algorithms) > 0 {
		metadata["EncryptionAlgorithms"] = algorithms
	}
	if algorithms := spec.signingAlgorithmsFor(record.KeyUsage); len(algorithms) > 0 {
		metadata["SigningAlgorithms"] = algorithms
	}
	return metadata
}

func (s *Service) keyARN(keyID string) string {
	return fmt.Sprintf("arn:aws:kms:%s:%s:key/%s", s.scope.Region, s.scope.Account, keyID)
}

func (s *Service) aliasARN(aliasName string) string {
	return fmt.Sprintf("arn:aws:kms:%s:%s:%s", s.scope.Region, s.scope.Account, aliasName)
}

func (s *Service) defaultKeyPolicy() string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Sid":"EnableRoot","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::%s:root"},"Action":"kms:*","Resource":"*"}]}`, s.scope.Account)
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

func invalidKeyUsage(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidKeyUsageException", Message: message}
}

func invalidCiphertext() error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidCiphertextException", Message: "The ciphertext is invalid or was encrypted under a different key or encryption context."}
}

func unsupportedOperation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "UnsupportedOperationException", Message: message}
}

func incorrectKey() error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "IncorrectKeyException", Message: "The key ID in the request does not identify a KMS key that can perform this operation."}
}

func dryRunSucceeded() error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "DryRunOperationException", Message: "The request would have succeeded, but the DryRun option is set."}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "DependencyTimeoutException", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// SetScope sets the account and region this service instance serves.
func (s *Service) SetScope(sc scope.Scope) {
	s.scope = sc
}
