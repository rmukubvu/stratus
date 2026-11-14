package sqs

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/stratus/internal/apierror"
)

// Hard limits SQS applies to every request, whatever the account's quotas.
// They are part of the API contract, so Stratus always enforces them.
const (
	// maxBatchEntries is the most entries one batch request may carry.
	maxBatchEntries = 10
	// maxPayloadBytes caps a single message and the sum of the bodies in a
	// SendMessageBatch request. It is also the largest MaximumMessageSize
	// a queue may set.
	maxPayloadBytes = 1 << 20
	// defaultMaximumMessageSize is a new queue's MaximumMessageSize.
	defaultMaximumMessageSize = 262144
	// maxDelaySeconds is the longest DelaySeconds, on a queue or a message.
	maxDelaySeconds = 900
	// maxVisibilityTimeout is the longest visibility timeout (12 hours).
	maxVisibilityTimeout = 43200
)

// attributeRange is the inclusive range an integer queue attribute accepts.
type attributeRange struct{ min, max int }

var queueAttributeRanges = map[string]attributeRange{
	"DelaySeconds":                  {0, maxDelaySeconds},
	"MaximumMessageSize":            {1024, maxPayloadBytes},
	"MessageRetentionPeriod":        {60, 1209600},
	"ReceiveMessageWaitTimeSeconds": {0, 20},
	"VisibilityTimeout":             {0, maxVisibilityTimeout},
}

// validateIntegerAttribute checks an integer queue attribute against its
// range.
func validateIntegerAttribute(name, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return badRequest("InvalidAttributeValue", name+" must be an integer.")
	}
	if r, ok := queueAttributeRanges[name]; ok && (n < r.min || n > r.max) {
		return badRequest("InvalidAttributeValue",
			fmt.Sprintf("Invalid value for the parameter %s. Reason: must be between %d and %d.", name, r.min, r.max))
	}
	return nil
}

// validateMessageBody checks a message body against the queue's
// MaximumMessageSize.
func validateMessageBody(queue queueRecord, body string) error {
	if body == "" {
		return badRequest("MissingParameter", "The request must contain the parameter MessageBody.")
	}
	limit := attributeInt(queue.Attributes, "MaximumMessageSize", defaultMaximumMessageSize)
	if len(body) > limit {
		return badRequest("InvalidParameterValue",
			fmt.Sprintf("One or more parameters are invalid. Reason: Message must be shorter than %d bytes.", limit))
	}
	return nil
}

// parseDelaySeconds reads a message's DelaySeconds, falling back to the
// queue's when raw is empty.
func parseDelaySeconds(queue queueRecord, raw string) (int, error) {
	if raw == "" {
		return attributeInt(queue.Attributes, "DelaySeconds", 0), nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > maxDelaySeconds {
		return 0, badRequest("InvalidParameterValue",
			fmt.Sprintf("Value %s for parameter DelaySeconds is invalid. Reason: must be between 0 and %d, if provided.", raw, maxDelaySeconds))
	}
	return value, nil
}

// parseVisibilityTimeout reads a visibility timeout given to
// ChangeMessageVisibility.
func parseVisibilityTimeout(raw string) (int, error) {
	if raw == "" {
		return 0, badRequest("MissingParameter", "The request must contain the parameter VisibilityTimeout.")
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || value > maxVisibilityTimeout {
		return 0, badRequest("InvalidParameterValue",
			fmt.Sprintf("Value %s for parameter VisibilityTimeout is invalid. Reason: must be between 0 and %d.", raw, maxVisibilityTimeout))
	}
	return value, nil
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: message}
}
