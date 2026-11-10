package route53

import (
	"fmt"
	"net/http"

	"github.com/stratus/internal/apierror"
)

func invalidInput(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidInput", Message: fmt.Sprintf(format, args...)}
}

func invalidChangeBatch(format string, args ...any) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidChangeBatch", Message: fmt.Sprintf(format, args...)}
}

func noSuchHostedZone(id string) error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NoSuchHostedZone", Message: "No hosted zone found with ID: " + id}
}

func noSuchChange(id string) error {
	return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NoSuchChange", Message: "Could not find resource with ID: " + id}
}

func hostedZoneNotEmpty(id string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "HostedZoneNotEmpty", Message: "The specified hosted zone " + id + " contains non-required resource record sets and so cannot be deleted."}
}

func hostedZoneAlreadyExists(callerReference string) error {
	return &apierror.Error{StatusCode: http.StatusConflict, Code: "HostedZoneAlreadyExists", Message: "A hosted zone has already been created with the specified caller reference: " + callerReference}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}
