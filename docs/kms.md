# KMS

Stratus keeps real key material for every KMS key, so ciphertexts and
signatures behave the way they do in AWS: a signature from `Sign` verifies
offline against the key from `GetPublicKey`, and a ciphertext only decrypts
under the key and encryption context it was made with.

Key material lives in the Stratus state store, so it survives restarts and is
captured by snapshots. That also means anyone who can read the data directory
can read the keys; it is an emulator, not a key vault.

## Key specs

| KeySpec | KeyUsage | Algorithms |
|---|---|---|
| `SYMMETRIC_DEFAULT` (default) | `ENCRYPT_DECRYPT` | `SYMMETRIC_DEFAULT` (AES-256-GCM) |
| `RSA_2048`, `RSA_3072`, `RSA_4096` | `ENCRYPT_DECRYPT` | `RSAES_OAEP_SHA_1`, `RSAES_OAEP_SHA_256` |
| `RSA_2048`, `RSA_3072`, `RSA_4096` | `SIGN_VERIFY` | `RSASSA_PSS_SHA_256/384/512`, `RSASSA_PKCS1_V1_5_SHA_256/384/512` |
| `ECC_NIST_P256` / `P384` / `P521` | `SIGN_VERIFY` | `ECDSA_SHA_256` / `384` / `512` |
| `ECC_SECG_P256K1` (secp256k1) | `SIGN_VERIFY` | `ECDSA_SHA_256` |
| `ECC_NIST_EDWARDS25519` (Ed25519) | `SIGN_VERIFY` | `ED25519_SHA_512`, `ED25519_PH_SHA_512` |

As in AWS, `KeyUsage` defaults to `ENCRYPT_DECRYPT`, so a signing key must be
created with `--key-usage SIGN_VERIFY`:

```sh
aws kms create-key --key-spec ECC_SECG_P256K1 --key-usage SIGN_VERIFY
```

An incompatible spec and usage pair is a `ValidationException`. HMAC, ML-DSA,
ML-KEM, SM2 and `KEY_AGREEMENT` keys are valid in AWS but not backed by Stratus
yet; `CreateKey` returns `NotImplementedException` for them.

## Signing

`Sign`, `Verify` and `GetPublicKey` follow the AWS rules:

- `Message` is 1 to 4096 bytes. `MessageType` `RAW` (the default) hashes it
  with the algorithm's hash; `DIGEST` takes it as that hash and requires the
  exact length (32 bytes for `ECDSA_SHA_256`).
- ECDSA signatures are DER (RFC 3279). secp256k1 signatures are low-S;
  `Verify` also accepts high-S signatures.
- `ED25519_SHA_512` is pure Ed25519 and requires `MessageType RAW`.
  `ED25519_PH_SHA_512` is Ed25519ph and requires `MessageType DIGEST` with a
  64-byte SHA-512 digest; as AWS documents, KMS still applies the Ed25519ph
  SHA-512 prehash to that digest, so the input is hashed twice.
- `GetPublicKey` returns the DER X.509 SubjectPublicKeyInfo. For secp256k1
  that is `id-ecPublicKey` with the `secp256k1` curve OID (1.3.132.0.10) and an
  uncompressed point.
- A signature that does not match is `KMSInvalidSignatureException`; a
  symmetric key or an algorithm the key does not offer is
  `InvalidKeyUsageException`; `GetPublicKey` on a symmetric key is
  `UnsupportedOperationException`.

## Encryption and data keys

`Encrypt` and `Decrypt` use AES-256-GCM. `EncryptionContext` is bound to the
ciphertext: decrypting with a different context, or naming a different
`KeyId`, fails with `InvalidCiphertextException` or `IncorrectKeyException`.
Ciphertexts from earlier Stratus versions still decrypt.

`GenerateDataKey` and `GenerateDataKeyWithoutPlaintext` take exactly one of
`KeySpec` (`AES_256`, `AES_128`) or `NumberOfBytes` (1 to 1024) and need a
symmetric key. The `CiphertextBlob` they return is an ordinary ciphertext, so
`Decrypt` with the same encryption context returns the data key, which is
what envelope encryption libraries expect. `GenerateRandom` returns 1 to 1024
random bytes.

RSA `ENCRYPT_DECRYPT` keys use RSAES-OAEP. Their ciphertexts carry no key ID,
so `Decrypt` needs `KeyId` and `EncryptionAlgorithm`.

Every cryptographic operation honours `DryRun` by returning
`DryRunOperationException` once the request has been validated.
