package kms

import (
	"crypto"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
)

const (
	messageTypeRaw    = "RAW"
	messageTypeDigest = "DIGEST"
)

// signatureScheme is the primitive family behind a signing algorithm.
type signatureScheme int

const (
	schemeRSAPSS signatureScheme = iota
	schemeRSAPKCS1
	schemeECDSA
	schemeEd25519
	schemeEd25519ph
)

// signingAlgorithm is one SigningAlgorithm value and how it hashes input.
type signingAlgorithm struct {
	name   string
	scheme signatureScheme
	hash   crypto.Hash
}

var signingAlgorithms = map[string]signingAlgorithm{
	"RSASSA_PSS_SHA_256":        {"RSASSA_PSS_SHA_256", schemeRSAPSS, crypto.SHA256},
	"RSASSA_PSS_SHA_384":        {"RSASSA_PSS_SHA_384", schemeRSAPSS, crypto.SHA384},
	"RSASSA_PSS_SHA_512":        {"RSASSA_PSS_SHA_512", schemeRSAPSS, crypto.SHA512},
	"RSASSA_PKCS1_V1_5_SHA_256": {"RSASSA_PKCS1_V1_5_SHA_256", schemeRSAPKCS1, crypto.SHA256},
	"RSASSA_PKCS1_V1_5_SHA_384": {"RSASSA_PKCS1_V1_5_SHA_384", schemeRSAPKCS1, crypto.SHA384},
	"RSASSA_PKCS1_V1_5_SHA_512": {"RSASSA_PKCS1_V1_5_SHA_512", schemeRSAPKCS1, crypto.SHA512},
	"ECDSA_SHA_256":             {"ECDSA_SHA_256", schemeECDSA, crypto.SHA256},
	"ECDSA_SHA_384":             {"ECDSA_SHA_384", schemeECDSA, crypto.SHA384},
	"ECDSA_SHA_512":             {"ECDSA_SHA_512", schemeECDSA, crypto.SHA512},
	"ED25519_SHA_512":           {"ED25519_SHA_512", schemeEd25519, crypto.SHA512},
	"ED25519_PH_SHA_512":        {"ED25519_PH_SHA_512", schemeEd25519ph, crypto.SHA512},
}

// signingInput returns the bytes handed to the signature primitive for a
// Sign or Verify request, applying the MessageType rules AWS documents:
//
//   - RAW hashes the message with the algorithm's hash; DIGEST takes the
//     message as that hash and requires its exact length.
//   - ED25519_SHA_512 (pure Ed25519) needs RAW and signs the message itself.
//   - ED25519_PH_SHA_512 (Ed25519ph) needs DIGEST, a 64-byte SHA-512 digest,
//     and KMS still applies the Ed25519ph SHA-512 prehash to it, so the
//     primitive receives SHA-512(digest).
func (a signingAlgorithm) signingInput(message []byte, messageType string) ([]byte, error) {
	if messageType == "" {
		messageType = messageTypeRaw
	}
	if messageType != messageTypeRaw && messageType != messageTypeDigest {
		return nil, validation(fmt.Sprintf("MessageType %s is not valid for signing algorithm %s", messageType, a.name))
	}
	switch a.scheme {
	case schemeEd25519:
		if messageType != messageTypeRaw {
			return nil, validation("ED25519_SHA_512 requires MessageType RAW")
		}
		return message, nil
	case schemeEd25519ph:
		if messageType != messageTypeDigest {
			return nil, validation("ED25519_PH_SHA_512 requires MessageType DIGEST")
		}
		if len(message) != sha512.Size {
			return nil, validation(fmt.Sprintf("Digest is invalid length for algorithm %s.", a.name))
		}
		sum := sha512.Sum512(message)
		return sum[:], nil
	}
	if messageType == messageTypeDigest {
		if len(message) != a.hash.Size() {
			return nil, validation(fmt.Sprintf("Digest is invalid length for algorithm %s.", a.name))
		}
		return message, nil
	}
	return digest(a.hash, message), nil
}

func digest(h crypto.Hash, message []byte) []byte {
	switch h {
	case crypto.SHA256:
		sum := sha256.Sum256(message)
		return sum[:]
	case crypto.SHA384:
		sum := sha512.Sum384(message)
		return sum[:]
	default:
		sum := sha512.Sum512(message)
		return sum[:]
	}
}
