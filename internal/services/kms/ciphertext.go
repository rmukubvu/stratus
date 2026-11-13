package kms

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
)

// Symmetric ciphertext blobs are opaque to callers but must carry the key ID,
// because Decrypt may omit KeyId. Layout of the current version:
//
//	"stratuskms:v2:" <key id> ":" <12-byte nonce> <AES-256-GCM sealed data>
//
// The GCM additional data binds the key ID and the encryption context, so a
// blob only decrypts under the same key and the same context, as in AWS.
// Version 1 blobs (base64 plaintext, no context) are still accepted so that
// snapshots taken before v2 keep working.
const (
	ciphertextPrefixV1 = "stratuskms:v1:"
	ciphertextPrefixV2 = "stratuskms:v2:"
)

var errInvalidCiphertext = errors.New("invalid ciphertext")

// symmetricCiphertext is a parsed ciphertext blob.
type symmetricCiphertext struct {
	keyID   string
	legacy  bool
	payload []byte
}

func parseSymmetricCiphertext(blob []byte) (symmetricCiphertext, error) {
	var prefix string
	switch {
	case bytes.HasPrefix(blob, []byte(ciphertextPrefixV2)):
		prefix = ciphertextPrefixV2
	case bytes.HasPrefix(blob, []byte(ciphertextPrefixV1)):
		prefix = ciphertextPrefixV1
	default:
		return symmetricCiphertext{}, errInvalidCiphertext
	}
	rest := blob[len(prefix):]
	sep := bytes.IndexByte(rest, ':')
	if sep <= 0 {
		return symmetricCiphertext{}, errInvalidCiphertext
	}
	return symmetricCiphertext{
		keyID:   string(rest[:sep]),
		legacy:  prefix == ciphertextPrefixV1,
		payload: rest[sep+1:],
	}, nil
}

func sealSymmetric(keyID string, material, plaintext []byte, context map[string]string) ([]byte, error) {
	aead, err := newAEAD(material)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	blob := make([]byte, 0, len(ciphertextPrefixV2)+len(keyID)+1+len(nonce)+len(plaintext)+aead.Overhead())
	blob = append(blob, ciphertextPrefixV2...)
	blob = append(blob, keyID...)
	blob = append(blob, ':')
	blob = append(blob, nonce...)
	return aead.Seal(blob, nonce, plaintext, additionalData(keyID, context)), nil
}

func (c symmetricCiphertext) open(material []byte, context map[string]string) ([]byte, error) {
	if c.legacy {
		plaintext, err := base64.StdEncoding.DecodeString(string(c.payload))
		if err != nil {
			return nil, errInvalidCiphertext
		}
		return plaintext, nil
	}
	aead, err := newAEAD(material)
	if err != nil {
		return nil, err
	}
	if len(c.payload) < aead.NonceSize()+aead.Overhead() {
		return nil, errInvalidCiphertext
	}
	nonce, sealed := c.payload[:aead.NonceSize()], c.payload[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, additionalData(c.keyID, context))
	if err != nil {
		return nil, errInvalidCiphertext
	}
	return plaintext, nil
}

func newAEAD(material []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(material)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// additionalData canonically encodes the key ID and encryption context:
// length-prefixed fields with context pairs sorted by key, so map order and
// ambiguous concatenations cannot change the result.
func additionalData(keyID string, context map[string]string) []byte {
	keys := make([]string, 0, len(context))
	for k := range context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	writeField(&b, keyID)
	for _, k := range keys {
		writeField(&b, k)
		writeField(&b, context[k])
	}
	return []byte(b.String())
}

func writeField(b *strings.Builder, field string) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(field)))
	b.Write(size[:])
	b.WriteString(field)
}
