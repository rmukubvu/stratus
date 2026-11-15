package authn

import (
	"encoding/xml"
	"net/http"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/sigv4"
)

// family groups AWS services by how they report authentication errors.
type family int

const (
	familyQuery family = iota // STS, SQS, SNS, IAM, ... and Route 53's REST-XML
	familyEC2
	familyJSON // AWS JSON 1.0/1.1: DynamoDB, KMS, ...
	familyREST // REST-JSON: Lambda, API Gateway, ...
	familyS3
)

func familyOf(c httpapi.Classification) family {
	switch {
	case c.Protocol == httpapi.ProtocolS3:
		return familyS3
	case c.Protocol == httpapi.ProtocolJSON:
		return familyJSON
	case c.Protocol == httpapi.ProtocolREST:
		return familyREST
	case c.Service == "ec2":
		return familyEC2
	}
	return familyQuery
}

// awsError is the status, code and message one service family returns.
type awsError struct {
	status  int
	code    string
	message string
}

const (
	invalidTokenMessage = "The security token included in the request is invalid."
	mismatchMessage     = "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method. Consult the service documentation for details."
)

// translate turns a verification failure into the error the service would
// return. Messages keep AWS's wording first, then add what Stratus knows
// about the cause, so both a person and a coding agent can act on it.
func translate(f *sigv4.Failure, c httpapi.Classification) error {
	fam := familyOf(c)
	e := pick(f, fam)
	err := &apierror.Error{StatusCode: e.status, Code: e.code, Message: e.message}
	if fam == familyS3 && f.Kind == sigv4.KindSignatureMismatch && f.StringToSign != "" {
		return &s3SignatureError{err: err, failure: f}
	}
	return err
}

func pick(f *sigv4.Failure, fam family) awsError {
	detail := f.Message
	byFamily := func(query, ec2, json, rest, s3 awsError) awsError {
		switch fam {
		case familyS3:
			return s3
		case familyJSON:
			return json
		case familyREST:
			return rest
		case familyEC2:
			return ec2
		}
		return query
	}
	switch f.Kind {
	case sigv4.KindMissingAuthentication:
		return byFamily(
			awsError{403, "MissingAuthenticationToken", "Request is missing Authentication Token (strict signature mode refuses unsigned requests)"},
			awsError{401, "AuthFailure", "AWS was not able to validate the provided access credentials: the request is not signed"},
			awsError{400, "MissingAuthenticationTokenException", "Missing Authentication Token"},
			awsError{403, "MissingAuthenticationTokenException", "Missing Authentication Token"},
			awsError{403, "AccessDenied", "Access Denied: anonymous requests are refused in strict signature mode; sign the request or use a presigned URL"},
		)
	case sigv4.KindIncompleteSignature:
		s3 := awsError{400, "AuthorizationHeaderMalformed", detail}
		if f.Presigned {
			s3 = awsError{400, "AuthorizationQueryParametersError", detail}
		}
		return byFamily(
			awsError{400, "IncompleteSignature", detail},
			awsError{400, "IncompleteSignature", detail},
			awsError{400, "IncompleteSignatureException", detail},
			awsError{403, "IncompleteSignatureException", detail},
			s3,
		)
	case sigv4.KindUnsupportedAlgorithm:
		return byFamily(
			awsError{400, "IncompleteSignature", detail},
			awsError{400, "IncompleteSignature", detail},
			awsError{400, "IncompleteSignatureException", detail},
			awsError{403, "IncompleteSignatureException", detail},
			awsError{400, "InvalidRequest", detail},
		)
	case sigv4.KindUnknownAccessKey:
		return byFamily(
			awsError{403, "InvalidClientTokenId", invalidTokenMessage + " " + detail},
			awsError{401, "AuthFailure", "AWS was not able to validate the provided access credentials. " + detail},
			awsError{400, "UnrecognizedClientException", invalidTokenMessage + " " + detail},
			awsError{403, "UnrecognizedClientException", invalidTokenMessage + " " + detail},
			awsError{403, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records. " + detail},
		)
	case sigv4.KindInvalidToken:
		return byFamily(
			awsError{403, "InvalidClientTokenId", invalidTokenMessage + " " + detail},
			awsError{401, "AuthFailure", "AWS was not able to validate the provided access credentials. " + detail},
			awsError{400, "UnrecognizedClientException", invalidTokenMessage + " " + detail},
			awsError{403, "UnrecognizedClientException", invalidTokenMessage + " " + detail},
			awsError{400, "InvalidToken", "The provided token is malformed or otherwise invalid. " + detail},
		)
	case sigv4.KindExpiredToken:
		return byFamily(
			awsError{403, "ExpiredToken", "The security token included in the request is expired"},
			awsError{400, "RequestExpired", "Request has expired: the security token included in the request is expired"},
			awsError{400, "ExpiredTokenException", "The security token included in the request is expired"},
			awsError{403, "ExpiredTokenException", "The security token included in the request is expired"},
			awsError{400, "ExpiredToken", "The provided token has expired."},
		)
	case sigv4.KindRequestTimeSkewed:
		return byFamily(
			awsError{403, "SignatureDoesNotMatch", detail},
			awsError{400, "RequestExpired", "Request has expired. " + detail},
			awsError{400, "InvalidSignatureException", detail},
			awsError{403, "InvalidSignatureException", detail},
			awsError{403, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large. " + detail},
		)
	case sigv4.KindPresignExpired:
		return byFamily(
			awsError{400, "RequestExpired", detail},
			awsError{400, "RequestExpired", detail},
			awsError{400, "RequestExpired", detail},
			awsError{403, "RequestExpired", detail},
			awsError{403, "AccessDenied", detail},
		)
	case sigv4.KindSignatureMismatch, sigv4.KindContentHashMismatch:
		message := mismatchMessage + explainMismatch(f)
		s3 := awsError{403, "SignatureDoesNotMatch", mismatchMessage}
		if f.CanonicalRequest == "" {
			s3.message = detail
		}
		if f.Kind == sigv4.KindContentHashMismatch {
			s3 = awsError{400, "XAmzContentSHA256Mismatch", detail}
			message = detail
		}
		return byFamily(
			awsError{403, "SignatureDoesNotMatch", message},
			awsError{401, "AuthFailure", "AWS was not able to validate the provided access credentials. " + message},
			awsError{400, "InvalidSignatureException", message},
			awsError{403, "InvalidSignatureException", message},
			s3,
		)
	case sigv4.KindUnsignedHeaders:
		return awsError{403, "AccessDenied", detail}
	case sigv4.KindMissingContentHash:
		return awsError{400, "InvalidRequest", detail}
	case sigv4.KindInvalidContentHash:
		return awsError{400, "InvalidArgument", detail}
	case sigv4.KindMalformedChunk:
		return awsError{400, "IncompleteBody", detail}
	}
	return awsError{403, "AccessDenied", detail}
}

// explainMismatch appends what Stratus expected, in the form AWS itself
// uses for query and JSON services.
func explainMismatch(f *sigv4.Failure) string {
	if f.CanonicalRequest == "" {
		if f.StringToSign != "" {
			return "\n\n" + f.Message + "\n\nThe String-to-Sign should have been\n'" + f.StringToSign + "'\n"
		}
		return ""
	}
	return "\n\nThe Canonical String for this request should have been\n'" + f.CanonicalRequest +
		"'\n\nThe String-to-Sign should have been\n'" + f.StringToSign + "'\n"
}

// s3SignatureError is S3's SignatureDoesNotMatch, which carries the
// canonical request and string to sign as extra XML elements.
type s3SignatureError struct {
	err     *apierror.Error
	failure *sigv4.Failure
}

func (e *s3SignatureError) Error() string { return e.err.Error() }
func (e *s3SignatureError) Unwrap() error { return e.err }

type s3SignatureBody struct {
	XMLName           xml.Name `xml:"Error"`
	Code              string   `xml:"Code"`
	Message           string   `xml:"Message"`
	AWSAccessKeyID    string   `xml:"AWSAccessKeyId"`
	StringToSign      string   `xml:"StringToSign"`
	SignatureProvided string   `xml:"SignatureProvided"`
	CanonicalRequest  string   `xml:"CanonicalRequest"`
}

// WriteErrorResponse implements httpapi.ErrorResponder.
func (e *s3SignatureError) WriteErrorResponse(w http.ResponseWriter) {
	httpapi.WriteXML(w, e.err.StatusCode, s3SignatureBody{
		Code:              e.err.Code,
		Message:           e.err.Message,
		AWSAccessKeyID:    e.failure.AccessKeyID,
		StringToSign:      e.failure.StringToSign,
		SignatureProvided: e.failure.SignatureProvided,
		CanonicalRequest:  e.failure.CanonicalRequest,
	})
}
