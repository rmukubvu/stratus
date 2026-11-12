package httpapi

import (
	"errors"
	"net/http"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
)

// ErrorResponder is an error that writes its own response body, for a
// service whose error shape the shared writer cannot express. It should
// also unwrap to an *apierror.Error so the request log records its code.
type ErrorResponder interface {
	error
	WriteErrorResponse(w http.ResponseWriter)
}

func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := &apierror.Error{
		StatusCode: http.StatusInternalServerError,
		Code:       "InternalFailure",
		Message:    "internal server error",
	}

	var typedErr *apierror.Error
	if errors.As(err, &typedErr) {
		apiErr = typedErr
	}

	metadata := MetadataFromContext(r.Context())

	switch {
	case metadata.Classification.Protocol == ProtocolQuery && metadata.Classification.Service == "ec2":
		WriteXML(w, apiErr.StatusCode, awscompat.EC2ErrorResponse{
			Errors:    []awscompat.QueryError{{Code: apiErr.Code, Message: apiErr.Message}},
			RequestID: metadata.RequestID,
		})
	case metadata.Classification.Protocol == ProtocolJSON || metadata.Classification.Protocol == ProtocolREST:
		WriteJSON(w, apiErr.StatusCode, map[string]string{
			"__type":  apiErr.Code,
			"message": apiErr.Message,
		})
	case metadata.Classification.Protocol == ProtocolS3:
		WriteXML(w, apiErr.StatusCode, awscompat.S3ErrorResponse{
			Code:      apiErr.Code,
			Message:   apiErr.Message,
			Resource:  apiErr.Resource,
			RequestID: metadata.RequestID,
		})
	default:
		WriteXML(w, apiErr.StatusCode, awscompat.QueryErrorResponse{
			Error: awscompat.QueryError{
				Code:    apiErr.Code,
				Message: apiErr.Message,
			},
			RequestID: metadata.RequestID,
		})
	}
}
