package kafka

import (
	"fmt"
	"net/http"

	"github.com/stratus/internal/apierror"
)

func badRequest(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "BadRequestException", Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: fmt.Sprintf(format, args...)}
}

func notImplemented(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusConflict, Code: "ConflictException", Message: fmt.Sprintf(format, args...)}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerErrorException", Message: err.Error()}
}
