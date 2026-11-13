package contract

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// TestAWSCLIKMSSigningKeys signs with secp256k1 and Ed25519 keys through the
// AWS CLI and checks each signature offline against GetPublicKey, the way a
// wallet verifies a KMS signature.
func TestAWSCLIKMSSigningKeys(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	messagePath := filepath.Join(t.TempDir(), "message.bin")
	message := []byte("transfer 10 tokens to 0xabc")
	if err := os.WriteFile(messagePath, message, 0o644); err != nil {
		t.Fatalf("write message: %v", err)
	}

	t.Run("secp256k1", func(t *testing.T) {
		keyID := createSigningKey(t, h.baseURL, "ECC_SECG_P256K1")
		signature := cliBlob(t, runAWS(t, h.baseURL, "kms", "sign", "--key-id", keyID, "--message", "fileb://"+messagePath,
			"--message-type", "RAW", "--signing-algorithm", "ECDSA_SHA_256", "--output", "json"), "Signature")

		var spki struct {
			Algorithm struct {
				Algorithm  asn1.ObjectIdentifier
				Parameters asn1.ObjectIdentifier
			}
			PublicKey asn1.BitString
		}
		if _, err := asn1.Unmarshal(publicKeyDER(t, h.baseURL, keyID), &spki); err != nil {
			t.Fatalf("decode SubjectPublicKeyInfo: %v", err)
		}
		if !spki.Algorithm.Parameters.Equal(asn1.ObjectIdentifier{1, 3, 132, 0, 10}) {
			t.Fatalf("public key curve is %v, want secp256k1", spki.Algorithm.Parameters)
		}
		public, err := secp256k1.ParsePubKey(spki.PublicKey.Bytes)
		if err != nil {
			t.Fatalf("parse public key: %v", err)
		}
		parsed, err := secpecdsa.ParseDERSignature(signature)
		if err != nil {
			t.Fatalf("parse DER signature: %v", err)
		}
		digest := sha256.Sum256(message)
		if !parsed.Verify(digest[:], public) {
			t.Fatal("signature does not verify against the public key")
		}
		verifyWithCLI(t, h.baseURL, keyID, messagePath, signature, "RAW", "ECDSA_SHA_256")
	})

	t.Run("ed25519", func(t *testing.T) {
		keyID := createSigningKey(t, h.baseURL, "ECC_NIST_EDWARDS25519")
		signature := cliBlob(t, runAWS(t, h.baseURL, "kms", "sign", "--key-id", keyID, "--message", "fileb://"+messagePath,
			"--message-type", "RAW", "--signing-algorithm", "ED25519_SHA_512", "--output", "json"), "Signature")
		public, err := x509.ParsePKIXPublicKey(publicKeyDER(t, h.baseURL, keyID))
		if err != nil {
			t.Fatalf("parse public key: %v", err)
		}
		if !ed25519.Verify(public.(ed25519.PublicKey), message, signature) {
			t.Fatal("signature does not verify against the public key")
		}
		verifyWithCLI(t, h.baseURL, keyID, messagePath, signature, "RAW", "ED25519_SHA_512")
	})
}

// TestAWSCLIKMSEnvelopeEncryption generates a data key under an encryption
// context and recovers it with Decrypt, as envelope encryption does.
func TestAWSCLIKMSEnvelopeEncryption(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	var created struct {
		KeyMetadata struct{ KeyID string } `json:"KeyMetadata"`
	}
	mustDecode(t, runAWS(t, h.baseURL, "kms", "create-key", "--output", "json"), &created)

	out := runAWS(t, h.baseURL, "kms", "generate-data-key", "--key-id", created.KeyMetadata.KeyID, "--key-spec", "AES_256",
		"--encryption-context", "purpose=checkpoint", "--output", "json")
	plaintext := cliBlob(t, out, "Plaintext")
	if len(plaintext) != 32 {
		t.Fatalf("data key is %d bytes, want 32", len(plaintext))
	}
	ciphertextPath := filepath.Join(t.TempDir(), "datakey.bin")
	if err := os.WriteFile(ciphertextPath, cliBlob(t, out, "CiphertextBlob"), 0o644); err != nil {
		t.Fatalf("write ciphertext: %v", err)
	}
	decrypted := cliBlob(t, runAWS(t, h.baseURL, "kms", "decrypt", "--ciphertext-blob", "fileb://"+ciphertextPath,
		"--encryption-context", "purpose=checkpoint", "--output", "json"), "Plaintext")
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatal("Decrypt did not return the generated data key")
	}

	withoutOut := runAWS(t, h.baseURL, "kms", "generate-data-key-without-plaintext", "--key-id", created.KeyMetadata.KeyID,
		"--number-of-bytes", "16", "--output", "json")
	var without map[string]any
	mustDecode(t, withoutOut, &without)
	if _, ok := without["Plaintext"]; ok {
		t.Fatalf("generate-data-key-without-plaintext returned a plaintext: %s", withoutOut)
	}
}

func createSigningKey(t *testing.T, endpoint, spec string) string {
	t.Helper()
	var created struct {
		KeyMetadata struct {
			KeyID             string   `json:"KeyId"`
			SigningAlgorithms []string `json:"SigningAlgorithms"`
		} `json:"KeyMetadata"`
	}
	mustDecode(t, runAWS(t, endpoint, "kms", "create-key", "--key-spec", spec, "--key-usage", "SIGN_VERIFY", "--output", "json"), &created)
	if len(created.KeyMetadata.SigningAlgorithms) == 0 {
		t.Fatalf("%s key lists no signing algorithms", spec)
	}
	return created.KeyMetadata.KeyID
}

func publicKeyDER(t *testing.T, endpoint, keyID string) []byte {
	t.Helper()
	return cliBlob(t, runAWS(t, endpoint, "kms", "get-public-key", "--key-id", keyID, "--output", "json"), "PublicKey")
}

func verifyWithCLI(t *testing.T, endpoint, keyID, messagePath string, signature []byte, messageType, algorithm string) {
	t.Helper()
	signaturePath := filepath.Join(t.TempDir(), "signature.bin")
	if err := os.WriteFile(signaturePath, signature, 0o644); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	var verified struct{ SignatureValid bool }
	mustDecode(t, runAWS(t, endpoint, "kms", "verify", "--key-id", keyID, "--message", "fileb://"+messagePath,
		"--message-type", messageType, "--signature", "fileb://"+signaturePath, "--signing-algorithm", algorithm, "--output", "json"), &verified)
	if !verified.SignatureValid {
		t.Fatal("kms verify did not accept the signature")
	}
}

func cliBlob(t *testing.T, out []byte, field string) []byte {
	t.Helper()
	var fields map[string]any
	mustDecode(t, out, &fields)
	encoded, _ := fields[field].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		t.Fatalf("field %s is not a base64 blob in %s", field, out)
	}
	return decoded
}

func mustDecode(t *testing.T, out []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(out, into); err != nil {
		t.Fatalf("decode CLI output: %v\n%s", err, out)
	}
}
