package kms

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"hash"
	"math/big"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

const symmetricKeySize = 32 // AES-256

// asymmetricKey is the private half of an asymmetric KMS key.
type asymmetricKey interface {
	// PublicKeyDER returns the DER-encoded X.509 SubjectPublicKeyInfo, the
	// format GetPublicKey returns.
	PublicKeyDER() ([]byte, error)
}

// signer is an asymmetricKey that can produce and check signatures. input is
// what signingAlgorithm.signingInput returned.
type signer interface {
	asymmetricKey
	Sign(alg signingAlgorithm, input []byte) ([]byte, error)
	Verify(alg signingAlgorithm, input, signature []byte) bool
}

// decrypter is an asymmetricKey that supports public-key encryption.
type decrypter interface {
	asymmetricKey
	Encrypt(alg string, plaintext []byte) ([]byte, error)
	Decrypt(alg string, ciphertext []byte) ([]byte, error)
}

// generateMaterial creates fresh key material for a spec. Symmetric keys are
// raw AES-256 bytes, secp256k1 keys are the 32-byte private scalar and every
// other asymmetric key is PKCS #8 DER.
func generateMaterial(spec keySpec) ([]byte, error) {
	switch spec.name {
	case specSymmetricDefault:
		material := make([]byte, symmetricKeySize)
		if _, err := rand.Read(material); err != nil {
			return nil, err
		}
		return material, nil
	case specRSA2048, specRSA3072, specRSA4096:
		key, err := rsa.GenerateKey(rand.Reader, rsaBits(spec.name))
		if err != nil {
			return nil, err
		}
		return x509.MarshalPKCS8PrivateKey(key)
	case specNISTP256, specNISTP384, specNISTP521:
		key, err := ecdsa.GenerateKey(nistCurve(spec.name), rand.Reader)
		if err != nil {
			return nil, err
		}
		return x509.MarshalPKCS8PrivateKey(key)
	case specEdwards25519:
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		return x509.MarshalPKCS8PrivateKey(key)
	case specSECGP256K1:
		key, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			return nil, err
		}
		return key.Serialize(), nil
	}
	return nil, fmt.Errorf("no key material generator for %s", spec.name)
}

// parseAsymmetricKey rebuilds the private key stored by generateMaterial.
func parseAsymmetricKey(spec keySpec, material []byte) (asymmetricKey, error) {
	if spec.name == specSECGP256K1 {
		if len(material) != 32 {
			return nil, errors.New("secp256k1 key material must be 32 bytes")
		}
		return secp256k1Key{secp256k1.PrivKeyFromBytes(material)}, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(material)
	if err != nil {
		return nil, err
	}
	switch key := parsed.(type) {
	case *rsa.PrivateKey:
		return rsaKey{key}, nil
	case *ecdsa.PrivateKey:
		return nistKey{key}, nil
	case ed25519.PrivateKey:
		return edwardsKey{key}, nil
	}
	return nil, fmt.Errorf("unsupported key material for %s", spec.name)
}

func rsaBits(spec string) int {
	switch spec {
	case specRSA3072:
		return 3072
	case specRSA4096:
		return 4096
	default:
		return 2048
	}
}

func nistCurve(spec string) elliptic.Curve {
	switch spec {
	case specNISTP384:
		return elliptic.P384()
	case specNISTP521:
		return elliptic.P521()
	default:
		return elliptic.P256()
	}
}

// rsaKey backs RSA_2048/3072/4096 for both SIGN_VERIFY and ENCRYPT_DECRYPT.
type rsaKey struct{ key *rsa.PrivateKey }

func (k rsaKey) PublicKeyDER() ([]byte, error) { return x509.MarshalPKIXPublicKey(&k.key.PublicKey) }

func (k rsaKey) Sign(alg signingAlgorithm, input []byte) ([]byte, error) {
	if alg.scheme == schemeRSAPSS {
		return rsa.SignPSS(rand.Reader, k.key, alg.hash, input, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	}
	return rsa.SignPKCS1v15(rand.Reader, k.key, alg.hash, input)
}

func (k rsaKey) Verify(alg signingAlgorithm, input, signature []byte) bool {
	if alg.scheme == schemeRSAPSS {
		return rsa.VerifyPSS(&k.key.PublicKey, alg.hash, input, signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
	}
	return rsa.VerifyPKCS1v15(&k.key.PublicKey, alg.hash, input, signature) == nil
}

func (k rsaKey) Encrypt(alg string, plaintext []byte) ([]byte, error) {
	return rsa.EncryptOAEP(oaepHash(alg), rand.Reader, &k.key.PublicKey, plaintext, nil)
}

func (k rsaKey) Decrypt(alg string, ciphertext []byte) ([]byte, error) {
	return rsa.DecryptOAEP(oaepHash(alg), nil, k.key, ciphertext, nil)
}

func oaepHash(alg string) hash.Hash {
	if alg == "RSAES_OAEP_SHA_1" {
		return sha1.New()
	}
	return sha256.New()
}

// nistKey backs ECC_NIST_P256/P384/P521; signatures are DER (RFC 3279).
type nistKey struct{ key *ecdsa.PrivateKey }

func (k nistKey) PublicKeyDER() ([]byte, error) { return x509.MarshalPKIXPublicKey(&k.key.PublicKey) }

func (k nistKey) Sign(_ signingAlgorithm, input []byte) ([]byte, error) {
	return ecdsa.SignASN1(rand.Reader, k.key, input)
}

func (k nistKey) Verify(_ signingAlgorithm, input, signature []byte) bool {
	return ecdsa.VerifyASN1(&k.key.PublicKey, input, signature)
}

// edwardsKey backs ECC_NIST_EDWARDS25519; signatures are the raw 64 bytes.
type edwardsKey struct{ key ed25519.PrivateKey }

func (k edwardsKey) PublicKeyDER() ([]byte, error) { return x509.MarshalPKIXPublicKey(k.key.Public()) }

func (k edwardsKey) Sign(alg signingAlgorithm, input []byte) ([]byte, error) {
	return k.key.Sign(nil, input, edwardsOptions(alg))
}

func (k edwardsKey) Verify(alg signingAlgorithm, input, signature []byte) bool {
	public := k.key.Public().(ed25519.PublicKey)
	return ed25519.VerifyWithOptions(public, input, signature, edwardsOptions(alg)) == nil
}

func edwardsOptions(alg signingAlgorithm) *ed25519.Options {
	if alg.scheme == schemeEd25519ph {
		return &ed25519.Options{Hash: crypto.SHA512}
	}
	return &ed25519.Options{Hash: crypto.Hash(0)}
}

// secp256k1Key backs ECC_SECG_P256K1. The Go standard library has no
// secp256k1, so the curve comes from decred's implementation; signatures are
// DER like the NIST curves.
type secp256k1Key struct{ key *secp256k1.PrivateKey }

var (
	oidPublicKeyECDSA = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidSECP256K1      = asn1.ObjectIdentifier{1, 3, 132, 0, 10}
)

type subjectPublicKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	PublicKey asn1.BitString
}

func (k secp256k1Key) PublicKeyDER() ([]byte, error) {
	curve, err := asn1.Marshal(oidSECP256K1)
	if err != nil {
		return nil, err
	}
	point := k.key.PubKey().SerializeUncompressed()
	return asn1.Marshal(subjectPublicKeyInfo{
		Algorithm: pkix.AlgorithmIdentifier{Algorithm: oidPublicKeyECDSA, Parameters: asn1.RawValue{FullBytes: curve}},
		PublicKey: asn1.BitString{Bytes: point, BitLength: len(point) * 8},
	})
}

func (k secp256k1Key) Sign(_ signingAlgorithm, input []byte) ([]byte, error) {
	return secpecdsa.Sign(k.key, input).Serialize(), nil
}

// Verify accepts any valid DER signature, including high-S ones that other
// signers produce, by normalising S before checking.
func (k secp256k1Key) Verify(_ signingAlgorithm, input, signature []byte) bool {
	var parsed struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(signature, &parsed)
	if err != nil || len(rest) != 0 {
		return false
	}
	var r, s secp256k1.ModNScalar
	if !setScalar(&r, parsed.R) || !setScalar(&s, parsed.S) {
		return false
	}
	if s.IsOverHalfOrder() {
		s.Negate()
	}
	return secpecdsa.NewSignature(&r, &s).Verify(input, k.key.PubKey())
}

// setScalar loads v into dst, rejecting zero and values outside [1, N).
func setScalar(dst *secp256k1.ModNScalar, v *big.Int) bool {
	if v == nil || v.Sign() <= 0 || v.BitLen() > 256 {
		return false
	}
	var buf [32]byte
	v.FillBytes(buf[:])
	if overflow := dst.SetBytes(&buf); overflow != 0 {
		return false
	}
	return !dst.IsZero()
}
