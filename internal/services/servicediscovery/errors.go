package servicediscovery

import (
	"fmt"
	"net/http"

	"github.com/stratus/internal/apierror"
)

func clientError(code, format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: fmt.Sprintf(format, args...)}
}

func invalidInput(format string, args ...any) error {
	return clientError("InvalidInput", format, args...)
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}
