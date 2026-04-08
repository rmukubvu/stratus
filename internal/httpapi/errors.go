package httpapi

import (
	"errors"
	"net/http"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
)

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

	switch metadata.Classification.Protocol {
	case ProtocolJSON, ProtocolREST:
		WriteJSON(w, apiErr.StatusCode, map[string]string{
			"__type":  apiErr.Code,
			"message": apiErr.Message,
		})
	case ProtocolS3:
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
