package kms

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	return NewService(metadata)
}

// call invokes one KMS operation with a JSON body and decodes the response.
func call(t *testing.T, svc *Service, operation string, body any) (map[string]any, error) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s: %v", operation, err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://localhost:4566/", bytes.NewReader(payload))
	if err := svc.Handle(rec, req, operation); err != nil {
		return nil, err
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s response: %v", operation, err)
		}
	}
	return out, nil
}

func mustCall(t *testing.T, svc *Service, operation string, body any) map[string]any {
	t.Helper()
	out, err := call(t, svc, operation, body)
	if err != nil {
		t.Fatalf("%s failed: %v", operation, err)
	}
	return out
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

// blob decodes a base64 JSON blob field.
func blob(t *testing.T, out map[string]any, field string) []byte {
	t.Helper()
	var decoded []byte
	raw, _ := json.Marshal(out[field])
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded) == 0 {
		t.Fatalf("field %s is not a blob: %v", field, out[field])
	}
	return decoded
}

func createKey(t *testing.T, svc *Service, spec, usage string) string {
	t.Helper()
	out := mustCall(t, svc, "CreateKey", map[string]any{"KeySpec": spec, "KeyUsage": usage})
	return out["KeyMetadata"].(map[string]any)["KeyId"].(string)
}

func TestSecp256k1SignaturesVerifyOfflineAgainstPublicKey(t *testing.T) {
	svc := newTestService(t)
	keyID := createKey(t, svc, specSECGP256K1, usageSignVerify)

	der := blob(t, mustCall(t, svc, "GetPublicKey", map[string]any{"KeyId": keyID}), "PublicKey")
	var spki subjectPublicKeyInfo
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		t.Fatalf("public key is not SubjectPublicKeyInfo DER: %v", err)
	}
	var curve asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(spki.Algorithm.Parameters.FullBytes, &curve); err != nil {
		t.Fatalf("curve parameters: %v", err)
	}
	if !spki.Algorithm.Algorithm.Equal(oidPublicKeyECDSA) || !curve.Equal(oidSECP256K1) {
		t.Fatalf("unexpected algorithm %v / curve %v", spki.Algorithm.Algorithm, curve)
	}
	public, err := secp256k1.ParsePubKey(spki.PublicKey.Bytes)
	if err != nil {
		t.Fatalf("parse secp256k1 point: %v", err)
	}

	message := []byte("transfer 10 tokens")
	digest := sha256.Sum256(message)
	for _, tc := range []struct {
		name        string
		message     []byte
		messageType string
	}{
		{"raw", message, "RAW"},
		{"digest", digest[:], "DIGEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := mustCall(t, svc, "Sign", map[string]any{
				"KeyId": keyID, "Message": tc.message, "MessageType": tc.messageType, "SigningAlgorithm": "ECDSA_SHA_256",
			})
			signature := blob(t, out, "Signature")
			parsed, err := secpecdsa.ParseDERSignature(signature)
			if err != nil {
				t.Fatalf("signature is not DER: %v", err)
			}
			if !parsed.Verify(digest[:], public) {
				t.Fatal("signature does not verify against the GetPublicKey key")
			}
			verified := mustCall(t, svc, "Verify", map[string]any{
				"KeyId": keyID, "Message": tc.message, "MessageType": tc.messageType, "Signature": signature, "SigningAlgorithm": "ECDSA_SHA_256",
			})
			if verified["SignatureValid"] != true {
				t.Fatalf("Verify rejected a valid signature: %v", verified)
			}
		})
	}
}

func TestSecp256k1VerifyAcceptsHighSSignatures(t *testing.T) {
	material, err := generateMaterial(keySpecs[specSECGP256K1])
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseAsymmetricKey(keySpecs[specSECGP256K1], material)
	if err != nil {
		t.Fatal(err)
	}
	alg := signingAlgorithms["ECDSA_SHA_256"]
	input := digest(crypto.SHA256, []byte("hello"))
	low, _ := key.(signer).Sign(alg, input)

	sig, err := secpecdsa.ParseDERSignature(low)
	if err != nil {
		t.Fatal(err)
	}
	r, s := sig.R(), sig.S()
	s.Negate() // N - s: the high-S twin of the same signature
	high := asn1Signature(t, r.Bytes(), s.Bytes())
	if !key.(signer).Verify(alg, input, high) {
		t.Fatal("high-S signature should verify")
	}
	if key.(signer).Verify(alg, digest(crypto.SHA256, []byte("other")), high) {
		t.Fatal("signature verified for a different message")
	}
}

func asn1Signature(t *testing.T, r, s [32]byte) []byte {
	t.Helper()
	type sig struct{ R, S asn1.RawValue }
	enc := func(b [32]byte) asn1.RawValue {
		v := bytes.TrimLeft(b[:], "\x00")
		if v[0]&0x80 != 0 {
			v = append([]byte{0}, v...)
		}
		return asn1.RawValue{Tag: asn1.TagInteger, Bytes: v}
	}
	der, err := asn1.Marshal(sig{enc(r), enc(s)})
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestEd25519SignaturesFollowAWSMessageTypeRules(t *testing.T) {
	svc := newTestService(t)
	keyID := createKey(t, svc, specEdwards25519, usageSignVerify)

	der := blob(t, mustCall(t, svc, "GetPublicKey", map[string]any{"KeyId": keyID}), "PublicKey")
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	public, ok := parsed.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("public key is %T, want ed25519", parsed)
	}

	message := []byte("authority decision 42")
	pure := blob(t, mustCall(t, svc, "Sign", map[string]any{
		"KeyId": keyID, "Message": message, "MessageType": "RAW", "SigningAlgorithm": "ED25519_SHA_512",
	}), "Signature")
	if !ed25519.Verify(public, message, pure) {
		t.Fatal("ED25519_SHA_512 signature does not verify offline")
	}

	clientDigest := sha512.Sum512(message)
	prehashed := blob(t, mustCall(t, svc, "Sign", map[string]any{
		"KeyId": keyID, "Message": clientDigest[:], "MessageType": "DIGEST", "SigningAlgorithm": "ED25519_PH_SHA_512",
	}), "Signature")
	// KMS applies the Ed25519ph SHA-512 prehash to the submitted digest.
	kmsPrehash := sha512.Sum512(clientDigest[:])
	if err := ed25519.VerifyWithOptions(public, kmsPrehash[:], prehashed, &ed25519.Options{Hash: crypto.SHA512}); err != nil {
		t.Fatalf("ED25519_PH_SHA_512 signature does not verify offline: %v", err)
	}
	mustCall(t, svc, "Verify", map[string]any{
		"KeyId": keyID, "Message": clientDigest[:], "MessageType": "DIGEST", "Signature": prehashed, "SigningAlgorithm": "ED25519_PH_SHA_512",
	})

	_, err = call(t, svc, "Sign", map[string]any{"KeyId": keyID, "Message": clientDigest[:], "MessageType": "DIGEST", "SigningAlgorithm": "ED25519_SHA_512"})
	expectCode(t, err, "ValidationException")
	_, err = call(t, svc, "Sign", map[string]any{"KeyId": keyID, "Message": message, "MessageType": "RAW", "SigningAlgorithm": "ED25519_PH_SHA_512"})
	expectCode(t, err, "ValidationException")
}

func TestStandardLibrarySignaturesVerifyOffline(t *testing.T) {
	for _, tc := range []struct {
		spec, algorithm string
		check           func(public any, digest, signature []byte) bool
	}{
		{specRSA2048, "RSASSA_PSS_SHA_256", func(p any, d, s []byte) bool {
			return rsa.VerifyPSS(p.(*rsa.PublicKey), crypto.SHA256, d, s, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		}},
		{specRSA2048, "RSASSA_PKCS1_V1_5_SHA_512", func(p any, d, s []byte) bool {
			return rsa.VerifyPKCS1v15(p.(*rsa.PublicKey), crypto.SHA512, d, s) == nil
		}},
		{specNISTP256, "ECDSA_SHA_256", func(p any, d, s []byte) bool { return ecdsa.VerifyASN1(p.(*ecdsa.PublicKey), d, s) }},
		{specNISTP384, "ECDSA_SHA_384", func(p any, d, s []byte) bool { return ecdsa.VerifyASN1(p.(*ecdsa.PublicKey), d, s) }},
		{specNISTP521, "ECDSA_SHA_512", func(p any, d, s []byte) bool { return ecdsa.VerifyASN1(p.(*ecdsa.PublicKey), d, s) }},
	} {
		t.Run(tc.spec+"/"+tc.algorithm, func(t *testing.T) {
			svc := newTestService(t)
			keyID := createKey(t, svc, tc.spec, usageSignVerify)
			public, err := x509.ParsePKIXPublicKey(blob(t, mustCall(t, svc, "GetPublicKey", map[string]any{"KeyId": keyID}), "PublicKey"))
			if err != nil {
				t.Fatalf("parse public key: %v", err)
			}
			message := []byte("payload")
			signature := blob(t, mustCall(t, svc, "Sign", map[string]any{"KeyId": keyID, "Message": message, "SigningAlgorithm": tc.algorithm}), "Signature")
			if !tc.check(public, digest(signingAlgorithms[tc.algorithm].hash, message), signature) {
				t.Fatal("signature does not verify offline")
			}
		})
	}
}

func TestVerifyRejectsTamperedMessage(t *testing.T) {
	svc := newTestService(t)
	keyID := createKey(t, svc, specSECGP256K1, usageSignVerify)
	signature := blob(t, mustCall(t, svc, "Sign", map[string]any{"KeyId": keyID, "Message": []byte("a"), "SigningAlgorithm": "ECDSA_SHA_256"}), "Signature")
	_, err := call(t, svc, "Verify", map[string]any{"KeyId": keyID, "Message": []byte("b"), "Signature": signature, "SigningAlgorithm": "ECDSA_SHA_256"})
	expectCode(t, err, "KMSInvalidSignatureException")
}

func TestCreateKeyValidatesSpecAndUsage(t *testing.T) {
	svc := newTestService(t)
	for _, tc := range []struct {
		name, spec, usage, code string
	}{
		{"signing spec needs SIGN_VERIFY", specSECGP256K1, "", "ValidationException"},
		{"symmetric cannot sign", specSymmetricDefault, usageSignVerify, "ValidationException"},
		{"unknown spec", "ECC_MADE_UP", usageSignVerify, "ValidationException"},
		{"hmac not implemented", "HMAC_256", usageGenerateVerifyMAC, "NotImplementedException"},
		{"key agreement not implemented", specNISTP256, usageKeyAgreement, "NotImplementedException"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := call(t, svc, "CreateKey", map[string]any{"KeySpec": tc.spec, "KeyUsage": tc.usage})
			expectCode(t, err, tc.code)
		})
	}
}

func TestDescribeKeyListsAlgorithmsForTheKeyUsage(t *testing.T) {
	svc := newTestService(t)
	signing := mustCall(t, svc, "DescribeKey", map[string]any{"KeyId": createKey(t, svc, specEdwards25519, usageSignVerify)})["KeyMetadata"].(map[string]any)
	if _, ok := signing["EncryptionAlgorithms"]; ok {
		t.Fatalf("signing key should not list encryption algorithms: %v", signing)
	}
	if got := signing["SigningAlgorithms"].([]any); len(got) != 2 || got[0] != "ED25519_SHA_512" || got[1] != "ED25519_PH_SHA_512" {
		t.Fatalf("unexpected signing algorithms: %v", got)
	}
	symmetric := mustCall(t, svc, "DescribeKey", map[string]any{"KeyId": createKey(t, svc, "", "")})["KeyMetadata"].(map[string]any)
	if _, ok := symmetric["SigningAlgorithms"]; ok || symmetric["KeySpec"] != specSymmetricDefault {
		t.Fatalf("unexpected symmetric metadata: %v", symmetric)
	}
}

func TestSigningOperationsRejectIncompatibleKeys(t *testing.T) {
	svc := newTestService(t)
	symmetricID := createKey(t, svc, "", "")
	secpID := createKey(t, svc, specSECGP256K1, usageSignVerify)

	_, err := call(t, svc, "Sign", map[string]any{"KeyId": symmetricID, "Message": []byte("m"), "SigningAlgorithm": "ECDSA_SHA_256"})
	expectCode(t, err, "InvalidKeyUsageException")
	_, err = call(t, svc, "Sign", map[string]any{"KeyId": secpID, "Message": []byte("m"), "SigningAlgorithm": "ECDSA_SHA_384"})
	expectCode(t, err, "InvalidKeyUsageException")
	_, err = call(t, svc, "GetPublicKey", map[string]any{"KeyId": symmetricID})
	expectCode(t, err, "UnsupportedOperationException")
	_, err = call(t, svc, "Encrypt", map[string]any{"KeyId": secpID, "Plaintext": []byte("m")})
	expectCode(t, err, "InvalidKeyUsageException")
	_, err = call(t, svc, "GenerateDataKey", map[string]any{"KeyId": secpID, "KeySpec": "AES_256"})
	expectCode(t, err, "InvalidKeyUsageException")
	_, err = call(t, svc, "Sign", map[string]any{"KeyId": secpID, "Message": []byte("m"), "SigningAlgorithm": "ECDSA_SHA_256", "DryRun": true})
	expectCode(t, err, "DryRunOperationException")
}

func TestGenerateDataKeyRoundTripsThroughDecrypt(t *testing.T) {
	svc := newTestService(t)
	keyID := createKey(t, svc, "", "")
	context := map[string]string{"bucket": "audit", "purpose": "checkpoint"}

	out := mustCall(t, svc, "GenerateDataKey", map[string]any{"KeyId": keyID, "KeySpec": "AES_256", "EncryptionContext": context})
	plaintext := blob(t, out, "Plaintext")
	if len(plaintext) != 32 {
		t.Fatalf("data key is %d bytes, want 32", len(plaintext))
	}
	ciphertext := blob(t, out, "CiphertextBlob")
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext contains the data key in the clear")
	}
	decrypted := blob(t, mustCall(t, svc, "Decrypt", map[string]any{"CiphertextBlob": ciphertext, "EncryptionContext": context}), "Plaintext")
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatal("Decrypt did not return the data key")
	}
	_, err := call(t, svc, "Decrypt", map[string]any{"CiphertextBlob": ciphertext, "EncryptionContext": map[string]string{"bucket": "other"}})
	expectCode(t, err, "InvalidCiphertextException")

	without := mustCall(t, svc, "GenerateDataKeyWithoutPlaintext", map[string]any{"KeyId": keyID, "NumberOfBytes": 24})
	if _, ok := without["Plaintext"]; ok {
		t.Fatal("GenerateDataKeyWithoutPlaintext returned a plaintext")
	}
	recovered := blob(t, mustCall(t, svc, "Decrypt", map[string]any{"CiphertextBlob": blob(t, without, "CiphertextBlob")}), "Plaintext")
	if len(recovered) != 24 {
		t.Fatalf("recovered data key is %d bytes, want 24", len(recovered))
	}
}

func TestEncryptDecryptBindsKeyAndContext(t *testing.T) {
	svc := newTestService(t)
	keyID := createKey(t, svc, "", "")
	otherID := createKey(t, svc, "", "")

	ciphertext := blob(t, mustCall(t, svc, "Encrypt", map[string]any{"KeyId": keyID, "Plaintext": []byte("secret")}), "CiphertextBlob")
	if bytes.Contains(ciphertext, []byte("secret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if got := blob(t, mustCall(t, svc, "Decrypt", map[string]any{"CiphertextBlob": ciphertext}), "Plaintext"); string(got) != "secret" {
		t.Fatalf("Decrypt returned %q", got)
	}
	_, err := call(t, svc, "Decrypt", map[string]any{"CiphertextBlob": ciphertext, "KeyId": otherID})
	expectCode(t, err, "IncorrectKeyException")
	tampered := append([]byte{}, ciphertext...)
	tampered[len(tampered)-1] ^= 1
	_, err = call(t, svc, "Decrypt", map[string]any{"CiphertextBlob": tampered})
	expectCode(t, err, "InvalidCiphertextException")
}

func TestDecryptAcceptsLegacyCiphertextAndKeysWithoutMaterial(t *testing.T) {
	svc := newTestService(t)
	keyID := createKey(t, svc, "", "")
	record, err := svc.loadKey(keyID)
	if err != nil {
		t.Fatal(err)
	}
	record.Material = nil // as stored by Stratus before key material existed
	if err := svc.putKey(record); err != nil {
		t.Fatal(err)
	}

	legacy := []byte("stratuskms:v1:" + keyID + ":aGVsbG8=")
	if got := blob(t, mustCall(t, svc, "Decrypt", map[string]any{"CiphertextBlob": legacy}), "Plaintext"); string(got) != "hello" {
		t.Fatalf("legacy Decrypt returned %q", got)
	}
	ciphertext := blob(t, mustCall(t, svc, "Encrypt", map[string]any{"KeyId": keyID, "Plaintext": []byte("fresh")}), "CiphertextBlob")
	if got := blob(t, mustCall(t, svc, "Decrypt", map[string]any{"CiphertextBlob": ciphertext}), "Plaintext"); string(got) != "fresh" {
		t.Fatalf("Decrypt after lazy material returned %q", got)
	}
}

func TestRSAEncryptionNeedsKeyIDToDecrypt(t *testing.T) {
	svc := newTestService(t)
	keyID := createKey(t, svc, specRSA2048, usageEncryptDecrypt)
	ciphertext := blob(t, mustCall(t, svc, "Encrypt", map[string]any{"KeyId": keyID, "Plaintext": []byte("wrapped"), "EncryptionAlgorithm": "RSAES_OAEP_SHA_256"}), "CiphertextBlob")

	_, err := call(t, svc, "Decrypt", map[string]any{"CiphertextBlob": ciphertext, "EncryptionAlgorithm": "RSAES_OAEP_SHA_256"})
	expectCode(t, err, "ValidationException")
	got := blob(t, mustCall(t, svc, "Decrypt", map[string]any{"CiphertextBlob": ciphertext, "KeyId": keyID, "EncryptionAlgorithm": "RSAES_OAEP_SHA_256"}), "Plaintext")
	if string(got) != "wrapped" {
		t.Fatalf("RSA Decrypt returned %q", got)
	}
	_, err = call(t, svc, "Encrypt", map[string]any{"KeyId": keyID, "Plaintext": []byte("x")})
	expectCode(t, err, "InvalidKeyUsageException")
}

func TestDataKeySize(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, tc := range []struct {
		spec  string
		bytes *int
		want  int
		ok    bool
	}{
		{"AES_256", nil, 32, true},
		{"AES_128", nil, 16, true},
		{"", n(64), 64, true},
		{"AES_256", n(32), 0, false},
		{"", nil, 0, false},
		{"", n(0), 0, false},
		{"", n(1025), 0, false},
		{"AES_512", nil, 0, false},
	} {
		got, err := dataKeySize(tc.spec, tc.bytes)
		if tc.ok != (err == nil) || got != tc.want {
			t.Errorf("dataKeySize(%q, %v) = %d, %v", tc.spec, tc.bytes, got, err)
		}
	}
}

func TestSigningInputDigestLength(t *testing.T) {
	alg := signingAlgorithms["ECDSA_SHA_256"]
	if _, err := alg.signingInput(make([]byte, 31), "DIGEST"); err == nil {
		t.Fatal("31-byte digest should be rejected for ECDSA_SHA_256")
	}
	if _, err := alg.signingInput([]byte("m"), "EXTERNAL_MU"); err == nil {
		t.Fatal("EXTERNAL_MU should be rejected for ECDSA")
	}
	got, err := alg.signingInput([]byte("m"), "")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("m"))
	if !bytes.Equal(got, want[:]) {
		t.Fatal("RAW should be the default and hash the message")
	}
}
