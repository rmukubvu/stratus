package kms

import (
	"fmt"
	"slices"
)

// Key usages accepted by CreateKey.
const (
	usageEncryptDecrypt    = "ENCRYPT_DECRYPT"
	usageSignVerify        = "SIGN_VERIFY"
	usageGenerateVerifyMAC = "GENERATE_VERIFY_MAC"
	usageKeyAgreement      = "KEY_AGREEMENT"
	usageEncapsulate       = "ENCAPSULATE_DECAPSULATE"
)

// Key specs Stratus implements with real key material.
const (
	specSymmetricDefault = "SYMMETRIC_DEFAULT"
	specRSA2048          = "RSA_2048"
	specRSA3072          = "RSA_3072"
	specRSA4096          = "RSA_4096"
	specNISTP256         = "ECC_NIST_P256"
	specNISTP384         = "ECC_NIST_P384"
	specNISTP521         = "ECC_NIST_P521"
	specSECGP256K1       = "ECC_SECG_P256K1"
	specEdwards25519     = "ECC_NIST_EDWARDS25519"
)

const algSymmetricDefault = "SYMMETRIC_DEFAULT"

var (
	rsaSigningAlgorithms = []string{
		"RSASSA_PSS_SHA_256", "RSASSA_PSS_SHA_384", "RSASSA_PSS_SHA_512",
		"RSASSA_PKCS1_V1_5_SHA_256", "RSASSA_PKCS1_V1_5_SHA_384", "RSASSA_PKCS1_V1_5_SHA_512",
	}
	rsaEncryptionAlgorithms = []string{"RSAES_OAEP_SHA_1", "RSAES_OAEP_SHA_256"}
)

// keySpec is what AWS KMS allows for one KeySpec value: which usages a key of
// that spec may have and which algorithms each usage offers. implemented is
// false for specs Stratus recognises but does not back with key material.
type keySpec struct {
	name        string
	usages      []string
	signing     []string
	encryption  []string
	implemented bool
}

var keySpecs = map[string]keySpec{
	specSymmetricDefault: {name: specSymmetricDefault, usages: []string{usageEncryptDecrypt}, encryption: []string{algSymmetricDefault}, implemented: true},
	specRSA2048:          rsaSpec(specRSA2048),
	specRSA3072:          rsaSpec(specRSA3072),
	specRSA4096:          rsaSpec(specRSA4096),
	specNISTP256:         eccSpec(specNISTP256, "ECDSA_SHA_256"),
	specNISTP384:         eccSpec(specNISTP384, "ECDSA_SHA_384"),
	specNISTP521:         eccSpec(specNISTP521, "ECDSA_SHA_512"),
	specSECGP256K1:       {name: specSECGP256K1, usages: []string{usageSignVerify}, signing: []string{"ECDSA_SHA_256"}, implemented: true},
	specEdwards25519:     {name: specEdwards25519, usages: []string{usageSignVerify}, signing: []string{"ED25519_SHA_512", "ED25519_PH_SHA_512"}, implemented: true},
	"HMAC_224":           {name: "HMAC_224", usages: []string{usageGenerateVerifyMAC}},
	"HMAC_256":           {name: "HMAC_256", usages: []string{usageGenerateVerifyMAC}},
	"HMAC_384":           {name: "HMAC_384", usages: []string{usageGenerateVerifyMAC}},
	"HMAC_512":           {name: "HMAC_512", usages: []string{usageGenerateVerifyMAC}},
	"ML_DSA_44":          {name: "ML_DSA_44", usages: []string{usageSignVerify}},
	"ML_DSA_65":          {name: "ML_DSA_65", usages: []string{usageSignVerify}},
	"ML_DSA_87":          {name: "ML_DSA_87", usages: []string{usageSignVerify}},
	"ML_KEM_512":         {name: "ML_KEM_512", usages: []string{usageEncapsulate}},
	"ML_KEM_768":         {name: "ML_KEM_768", usages: []string{usageEncapsulate}},
	"ML_KEM_1024":        {name: "ML_KEM_1024", usages: []string{usageEncapsulate}},
	"SM2":                {name: "SM2", usages: []string{usageEncryptDecrypt, usageSignVerify, usageKeyAgreement}},
}

func rsaSpec(name string) keySpec {
	return keySpec{
		name:        name,
		usages:      []string{usageEncryptDecrypt, usageSignVerify},
		signing:     rsaSigningAlgorithms,
		encryption:  rsaEncryptionAlgorithms,
		implemented: true,
	}
}

func eccSpec(name, signing string) keySpec {
	return keySpec{
		name:        name,
		usages:      []string{usageSignVerify, usageKeyAgreement},
		signing:     []string{signing},
		implemented: true,
	}
}

// resolveKeySpec validates a CreateKey KeySpec/KeyUsage pair the way AWS
// does: an unknown spec or an incompatible usage is a ValidationException,
// and a valid pair Stratus cannot back is NotImplementedException.
func resolveKeySpec(name, usage string) (keySpec, error) {
	spec, ok := keySpecs[name]
	if !ok {
		return keySpec{}, validation(fmt.Sprintf("1 validation error detected: Value '%s' at 'keySpec' failed to satisfy constraint: Member must satisfy enum value set", name))
	}
	if !slices.Contains(spec.usages, usage) {
		return keySpec{}, validation(fmt.Sprintf("KeyUsage %s is not compatible with KeySpec %s", usage, name))
	}
	if !spec.implemented || usage == usageKeyAgreement {
		return keySpec{}, notImplemented(fmt.Sprintf("KMS keys with KeySpec %s and KeyUsage %s are not implemented", name, usage))
	}
	return spec, nil
}

// signingAlgorithmsFor lists the signing algorithms a key offers; empty unless
// the key is a SIGN_VERIFY key.
func (k keySpec) signingAlgorithmsFor(usage string) []string {
	if usage != usageSignVerify {
		return nil
	}
	return k.signing
}

// encryptionAlgorithmsFor lists the encryption algorithms a key offers; empty
// unless the key is an ENCRYPT_DECRYPT key.
func (k keySpec) encryptionAlgorithmsFor(usage string) []string {
	if usage != usageEncryptDecrypt {
		return nil
	}
	return k.encryption
}

func (k keySpec) symmetric() bool {
	return k.name == specSymmetricDefault
}
