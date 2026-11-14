package limits

import (
	"net/http"

	"github.com/stratus/internal/apierror"
)

// queryServices answer over the AWS query protocol, whose throttling code
// is "Throttling" rather than "ThrottlingException".
var queryServices = map[string]bool{
	"sts": true, "iam": true, "sns": true, "sqs": true, "cloudformation": true,
	"monitoring": true, "rds": true, "elasticache": true, "elbv2": true, "ses": true, "ec2": true,
}

// ThrottleError is the error AWS returns when a request is throttled by
// the service the breach names. AWS SDKs recognise every one of these as a
// throttle and retry it with backoff.
func ThrottleError(b Breach) *apierror.Error {
	message := "Rate exceeded"
	if b.Kind == KindConcurrency {
		message = "Rate Exceeded."
	}
	message += " (Stratus limits profile, rule " + b.RuleID + ")"

	switch {
	case b.Service == "s3":
		return &apierror.Error{StatusCode: http.StatusServiceUnavailable, Code: "SlowDown", Message: "Please reduce your request rate. (Stratus limits profile, rule " + b.RuleID + ")"}
	case b.Service == "lambda":
		return &apierror.Error{StatusCode: http.StatusTooManyRequests, Code: "TooManyRequestsException", Message: message}
	case queryServices[b.Service]:
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "Throttling", Message: message}
	default:
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ThrottlingException", Message: message}
	}
}
