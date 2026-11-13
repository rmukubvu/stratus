package kms

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/stratus/internal/apierror"
)

const maxMessageBytes = 4096

type signInput struct {
	DryRun           bool   `json:"DryRun"`
	KeyID            string `json:"KeyId"`
	Message          []byte `json:"Message"`
	MessageType      string `json:"MessageType"`
	SigningAlgorithm string `json:"SigningAlgorithm"`
}

type verifyInput struct {
	signInput
	Signature []byte `json:"Signature"`
}

func (s *Service) sign(w http.ResponseWriter, r *http.Request) error {
	var input signInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	record, key, algorithm, payload, err := s.prepareSignature(input)
	if err != nil {
		return err
	}
	if input.DryRun {
		return dryRunSucceeded()
	}
	signature, err := key.Sign(algorithm, payload)
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"KeyId":            record.Arn,
		"Signature":        signature,
		"SigningAlgorithm": algorithm.name,
	})
	return nil
}

func (s *Service) verify(w http.ResponseWriter, r *http.Request) error {
	var input verifyInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	if len(input.Signature) == 0 {
		return validation("Signature is required")
	}
	record, key, algorithm, payload, err := s.prepareSignature(input.signInput)
	if err != nil {
		return err
	}
	if input.DryRun {
		return dryRunSucceeded()
	}
	if !key.Verify(algorithm, payload, input.Signature) {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "KMSInvalidSignatureException",
			Message:    "The signature is not valid for this message, KMS key and signing algorithm.",
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"KeyId":            record.Arn,
		"SignatureValid":   true,
		"SigningAlgorithm": algorithm.name,
	})
	return nil
}

// prepareSignature does the validation Sign and Verify share and returns the
// key, the algorithm and the exact bytes the signature covers.
func (s *Service) prepareSignature(input signInput) (keyRecord, signer, signingAlgorithm, []byte, error) {
	fail := func(err error) (keyRecord, signer, signingAlgorithm, []byte, error) {
		return keyRecord{}, nil, signingAlgorithm{}, nil, err
	}
	if len(input.Message) == 0 || len(input.Message) > maxMessageBytes {
		return fail(validation("Message must be between 1 and 4096 bytes"))
	}
	algorithm, ok := signingAlgorithms[input.SigningAlgorithm]
	if !ok {
		return fail(validation(fmt.Sprintf("1 validation error detected: Value '%s' at 'signingAlgorithm' failed to satisfy constraint: Member must satisfy enum value set", input.SigningAlgorithm)))
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return fail(err)
	}
	if err := requireUsage(record, usageSignVerify); err != nil {
		return fail(err)
	}
	if !slices.Contains(keySpecs[record.KeySpec].signingAlgorithmsFor(record.KeyUsage), algorithm.name) {
		return fail(invalidKeyUsage(fmt.Sprintf("Signing algorithm %s is not compatible with KMS key %s (KeySpec %s)", algorithm.name, record.Arn, record.KeySpec)))
	}
	payload, err := algorithm.signingInput(input.Message, input.MessageType)
	if err != nil {
		return fail(err)
	}
	key, err := s.asymmetricKeyFor(record)
	if err != nil {
		return fail(err)
	}
	sig, ok := key.(signer)
	if !ok {
		return fail(invalidKeyUsage(record.KeySpec + " keys cannot sign"))
	}
	return record, sig, algorithm, payload, nil
}

func (s *Service) getPublicKey(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		KeyID string `json:"KeyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return validation("request body is not valid JSON")
	}
	record, err := s.resolveKeyReference(input.KeyID)
	if err != nil {
		return err
	}
	key, err := s.asymmetricKeyFor(record)
	if err != nil {
		return err
	}
	publicKey, err := key.PublicKeyDER()
	if err != nil {
		return internal(err)
	}
	spec := keySpecs[record.KeySpec]
	response := map[string]any{
		"CustomerMasterKeySpec": record.KeySpec,
		"KeyId":                 record.Arn,
		"KeySpec":               record.KeySpec,
		"KeyUsage":              record.KeyUsage,
		"PublicKey":             publicKey,
	}
	if algorithms := spec.signingAlgorithmsFor(record.KeyUsage); len(algorithms) > 0 {
		response["SigningAlgorithms"] = algorithms
	}
	if algorithms := spec.encryptionAlgorithmsFor(record.KeyUsage); len(algorithms) > 0 {
		response["EncryptionAlgorithms"] = algorithms
	}
	writeJSON(w, http.StatusOK, response)
	return nil
}
