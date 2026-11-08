package fis

import (
	"fmt"
	"net/http"

	"github.com/stratus/internal/apierror"
)

func validation(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusConflict, Code: "ConflictException", Message: fmt.Sprintf(format, args...)}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerException", Message: err.Error()}
}
