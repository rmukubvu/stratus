package sigv4

// Kind says why a request failed verification. Kinds are protocol neutral;
// the caller maps each to the error code the AWS service in question
// returns (S3's SignatureDoesNotMatch, DynamoDB's InvalidSignatureException
// and so on).
type Kind string

const (
	// KindMissingAuthentication is a request with no signature at all.
	KindMissingAuthentication Kind = "missing_authentication"
	// KindIncompleteSignature is an Authorization header or presigned query
	// that lacks a required part or has one in the wrong format.
	KindIncompleteSignature Kind = "incomplete_signature"
	// KindUnsupportedAlgorithm is a request signed with something other
	// than SigV4, such as SigV2.
	KindUnsupportedAlgorithm Kind = "unsupported_algorithm"
	// KindUnknownAccessKey is an access key Stratus does not know, or one
	// that was deactivated.
	KindUnknownAccessKey Kind = "unknown_access_key"
	// KindInvalidToken is a missing, wrong or unexpected session token.
	KindInvalidToken Kind = "invalid_token"
	// KindExpiredToken is temporary credentials past their expiration.
	KindExpiredToken Kind = "expired_token"
	// KindRequestTimeSkewed is a request signed more than the allowed skew
	// (15 minutes) away from the server's clock.
	KindRequestTimeSkewed Kind = "request_time_skewed"
	// KindPresignExpired is a presigned URL used after X-Amz-Expires.
	KindPresignExpired Kind = "presign_expired"
	// KindSignatureMismatch is a signature that does not match the one
	// computed from the request and the secret key.
	KindSignatureMismatch Kind = "signature_mismatch"
	// KindUnsignedHeaders is an S3 request carrying x-amz-* headers it did
	// not sign.
	KindUnsignedHeaders Kind = "unsigned_headers"
	// KindMissingContentHash is an S3 request without x-amz-content-sha256.
	KindMissingContentHash Kind = "missing_content_hash"
	// KindInvalidContentHash is an x-amz-content-sha256 value that is
	// neither a SHA-256 hex digest nor a known placeholder.
	KindInvalidContentHash Kind = "invalid_content_hash"
	// KindContentHashMismatch is a body whose SHA-256 differs from the
	// x-amz-content-sha256 the request signed.
	KindContentHashMismatch Kind = "content_hash_mismatch"
	// KindMalformedChunk is an aws-chunked body that cannot be parsed.
	KindMalformedChunk Kind = "malformed_chunk"
)

// Failure is a request that did not verify. It carries what Stratus
// computed, so a caller can see exactly where its signing went wrong.
type Failure struct {
	Kind    Kind
	Message string
	// Presigned is true when the request was authenticated by query
	// parameters rather than an Authorization header.
	Presigned bool

	AccessKeyID string
	// SignatureProvided, CanonicalRequest and StringToSign are set for
	// KindSignatureMismatch: what the client sent and what Stratus
	// expected it to have signed.
	SignatureProvided string
	CanonicalRequest  string
	StringToSign      string
}

func (f *Failure) Error() string { return string(f.Kind) + ": " + f.Message }

func fail(kind Kind, message string) *Failure {
	return &Failure{Kind: kind, Message: message}
}
