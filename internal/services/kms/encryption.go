package kms

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

const (
	maxPlaintextBytes = 4096
	maxRandomBytes    = 1024
)

type encryptInput struct {
	DryRun              bool              `json:"DryRun"`
	EncryptionAlgorithm string            `json:"EncryptionAlgorithm"`
	EncryptionContext   map[string]string `json:"EncryptionContext"`
	KeyID               string            `json:"KeyId"`
	Plaintext           []byte            `json:"Plaintext"`
}

type decryptInput struct {
	CiphertextBlob      []byte            `json:"CiphertextBlob"`
	DryRun              bool              `json:"DryRun"`
	EncryptionAlgorithm string            `json:"EncryptionAlgorithm"`
	EncryptionContext   map[string]string `json:"EncryptionContext"`
	KeyID               string            `json:"KeyId"`
}

type generateDataKeyInput struct {
	DryRun            bool              `json:"DryRun"`
	EncryptionContext map[string]string `json:"EncryptionContext"`
	KeyID             string            `json:"KeyId"`
	KeySpec           string            `json:"KeySpec"`
	NumberOfBytes     *int              `json:"NumberOfBytes"`
}

func (s *Service) encrypt(w http.ResponseWriter, r *http.Request) error {
	var input encryptInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if len(input.Plaintext) == 0 || len(input.Plaintext) > maxPlaintextBytes {
		return validation("Plaintext must be between 1 and 4096 bytes")
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return err
	}
	algorithm, err := encryptionAlgorithm(record, input.EncryptionAlgorithm)
	if err != nil {
		return err
	}
	if input.DryRun {
		return dryRunSucceeded()
	}
	var ciphertext []byte
	if algorithm == algSymmetricDefault {
		ciphertext, err = s.sealWithKey(record, input.Plaintext, input.EncryptionContext)
	} else {
		ciphertext, err = s.encryptAsymmetric(record, algorithm, input.Plaintext)
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"CiphertextBlob":      ciphertext,
		"EncryptionAlgorithm": algorithm,
		"KeyId":               record.Arn,
	})
	return nil
}

func (s *Service) decrypt(w http.ResponseWriter, r *http.Request) error {
	var input decryptInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if len(input.CiphertextBlob) == 0 {
		return validation("CiphertextBlob is required")
	}
	if input.EncryptionAlgorithm != "" && input.EncryptionAlgorithm != algSymmetricDefault {
		return s.decryptAsymmetric(w, input)
	}
	blob, err := parseSymmetricCiphertext(input.CiphertextBlob)
	if err != nil {
		return invalidCiphertext()
	}
	record, err := s.loadKey(blob.keyID)
	if err != nil {
		return err
	}
	if input.KeyID != "" {
		requested, err := s.resolveKeyReference(input.KeyID)
		if err != nil {
			return err
		}
		if requested.KeyID != record.KeyID {
			return incorrectKey()
		}
	}
	if err := requireUsage(record, usageEncryptDecrypt); err != nil {
		return err
	}
	material, err := s.symmetricMaterial(&record)
	if err != nil {
		return err
	}
	if input.DryRun {
		return dryRunSucceeded()
	}
	plaintext, err := blob.open(material, input.EncryptionContext)
	if err != nil {
		return invalidCiphertext()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"EncryptionAlgorithm": algSymmetricDefault,
		"KeyId":               record.Arn,
		"Plaintext":           plaintext,
	})
	return nil
}

// decryptAsymmetric handles RSAES_OAEP_* ciphertext. Such blobs carry no key
// ID, so AWS requires KeyId.
func (s *Service) decryptAsymmetric(w http.ResponseWriter, input decryptInput) error {
	if input.KeyID == "" {
		return validation("KeyId is required to decrypt with an asymmetric KMS key")
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return err
	}
	algorithm, err := encryptionAlgorithm(record, input.EncryptionAlgorithm)
	if err != nil {
		return err
	}
	key, err := s.decrypterFor(record)
	if err != nil {
		return err
	}
	if input.DryRun {
		return dryRunSucceeded()
	}
	plaintext, err := key.Decrypt(algorithm, input.CiphertextBlob)
	if err != nil {
		return invalidCiphertext()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"EncryptionAlgorithm": algorithm,
		"KeyId":               record.Arn,
		"Plaintext":           plaintext,
	})
	return nil
}

// generateDataKey implements GenerateDataKey and, with includePlaintext
// false, GenerateDataKeyWithoutPlaintext. The returned CiphertextBlob is an
// ordinary symmetric ciphertext, so Decrypt with the same encryption context
// recovers the data key (envelope encryption).
func (s *Service) generateDataKey(w http.ResponseWriter, r *http.Request, includePlaintext bool) error {
	var input generateDataKeyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	size, err := dataKeySize(input.KeySpec, input.NumberOfBytes)
	if err != nil {
		return err
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return err
	}
	if !keySpecs[record.KeySpec].symmetric() {
		return invalidKeyUsage(fmt.Sprintf("%s key usage and %s key spec are not compatible with this operation; data keys need a symmetric encryption KMS key", record.KeyUsage, record.KeySpec))
	}
	if input.DryRun {
		return dryRunSucceeded()
	}
	plaintext := make([]byte, size)
	if _, err := rand.Read(plaintext); err != nil {
		return internal(err)
	}
	ciphertext, err := s.sealWithKey(record, plaintext, input.EncryptionContext)
	if err != nil {
		return err
	}
	response := map[string]any{
		"CiphertextBlob": ciphertext,
		"KeyId":          record.Arn,
	}
	if includePlaintext {
		response["Plaintext"] = plaintext
	}
	writeJSON(w, http.StatusOK, response)
	return nil
}

func (s *Service) generateRandom(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		NumberOfBytes int `json:"NumberOfBytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if input.NumberOfBytes < 1 || input.NumberOfBytes > maxRandomBytes {
		return validation("NumberOfBytes must be between 1 and 1024")
	}
	plaintext := make([]byte, input.NumberOfBytes)
	if _, err := rand.Read(plaintext); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Plaintext": plaintext})
	return nil
}

// dataKeySize applies the GenerateDataKey rule that exactly one of KeySpec
// (AES_128 or AES_256) and NumberOfBytes (1-1024) is given.
func dataKeySize(spec string, numberOfBytes *int) (int, error) {
	switch {
	case spec != "" && numberOfBytes != nil:
		return 0, validation("Specify either KeySpec or NumberOfBytes, not both")
	case spec == "AES_256":
		return 32, nil
	case spec == "AES_128":
		return 16, nil
	case spec != "":
		return 0, validation("KeySpec must be AES_256 or AES_128")
	case numberOfBytes == nil:
		return 0, validation("Specify either KeySpec or NumberOfBytes")
	case *numberOfBytes < 1 || *numberOfBytes > maxRandomBytes:
		return 0, validation("NumberOfBytes must be between 1 and 1024")
	}
	return *numberOfBytes, nil
}

// encryptionAlgorithm resolves the requested algorithm (default
// SYMMETRIC_DEFAULT) and checks the key offers it.
func encryptionAlgorithm(record keyRecord, requested string) (string, error) {
	if err := requireUsage(record, usageEncryptDecrypt); err != nil {
		return "", err
	}
	if requested == "" {
		requested = algSymmetricDefault
	}
	if !slices.Contains(keySpecs[record.KeySpec].encryptionAlgorithmsFor(record.KeyUsage), requested) {
		return "", invalidKeyUsage(fmt.Sprintf("Encryption algorithm %s is not compatible with KMS key %s (KeySpec %s)", requested, record.Arn, record.KeySpec))
	}
	return requested, nil
}

func requireUsage(record keyRecord, usage string) error {
	if record.KeyUsage != usage {
		return invalidKeyUsage(fmt.Sprintf("%s key usage is %s which is not valid for this operation; it needs %s", record.Arn, record.KeyUsage, usage))
	}
	return nil
}

func (s *Service) sealWithKey(record keyRecord, plaintext []byte, context map[string]string) ([]byte, error) {
	material, err := s.symmetricMaterial(&record)
	if err != nil {
		return nil, err
	}
	ciphertext, err := sealSymmetric(record.KeyID, material, plaintext, context)
	if err != nil {
		return nil, internal(err)
	}
	return ciphertext, nil
}

func (s *Service) encryptAsymmetric(record keyRecord, algorithm string, plaintext []byte) ([]byte, error) {
	key, err := s.decrypterFor(record)
	if err != nil {
		return nil, err
	}
	ciphertext, err := key.Encrypt(algorithm, plaintext)
	if err != nil {
		return nil, validation(fmt.Sprintf("Plaintext is too long for %s with %s", record.KeySpec, algorithm))
	}
	return ciphertext, nil
}

// symmetricMaterial returns the AES key of a symmetric key, creating and
// storing it for keys made before Stratus kept key material.
func (s *Service) symmetricMaterial(record *keyRecord) ([]byte, error) {
	if len(record.Material) == symmetricKeySize {
		return record.Material, nil
	}
	material, err := generateMaterial(keySpecs[specSymmetricDefault])
	if err != nil {
		return nil, internal(err)
	}
	record.Material = material
	if err := s.putKey(*record); err != nil {
		return nil, err
	}
	return material, nil
}

func (s *Service) asymmetricKeyFor(record keyRecord) (asymmetricKey, error) {
	spec := keySpecs[record.KeySpec]
	if spec.symmetric() {
		return nil, unsupportedOperation(record.Arn + " is a symmetric KMS key; this operation needs an asymmetric KMS key")
	}
	key, err := parseAsymmetricKey(spec, record.Material)
	if err != nil {
		return nil, internal(err)
	}
	return key, nil
}

func (s *Service) decrypterFor(record keyRecord) (decrypter, error) {
	key, err := s.asymmetricKeyFor(record)
	if err != nil {
		return nil, err
	}
	d, ok := key.(decrypter)
	if !ok {
		return nil, invalidKeyUsage(record.KeySpec + " keys cannot encrypt or decrypt")
	}
	return d, nil
}
