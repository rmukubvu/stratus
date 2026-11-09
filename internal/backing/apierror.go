package backing

import (
	"errors"
	"net/http"

	"github.com/stratus/internal/apierror"
)

// APIError translates a Runner failure into the AWS-shaped error a service
// returns to its caller. what names the resource, for example
// "DB instance app-db", so the message says which engine failed and why.
func APIError(err error, what string) *apierror.Error {
	var missing *ImageMissingError
	var notReady *NotReadyError
	switch {
	case errors.Is(err, ErrUnavailable):
		return &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceUnavailable",
			Message:    what + " needs a container engine: Docker is not reachable",
		}
	case errors.As(err, &missing):
		return &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceUnavailable",
			Message:    what + " cannot start: " + missing.Error(),
		}
	case errors.As(err, &notReady):
		return &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "InternalFailure",
			Message:    what + " failed to start: " + notReady.Error(),
		}
	default:
		return &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "InternalFailure",
			Message:    what + ": " + err.Error(),
		}
	}
}
