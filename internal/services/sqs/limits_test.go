package sqs

import (
	"errors"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
)

func errorCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an API error", err)
	}
	return apiErr.Code
}

func entriesWithIDs(ids ...string) []batchEntry {
	out := make([]batchEntry, len(ids))
	for i, id := range ids {
		out[i] = batchEntry{ID: id, MessageBody: "m"}
	}
	return out
}

func TestValidateBatchEntries(t *testing.T) {
	send := batchOperations["SendMessageBatch"]
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = "id" + strings.Repeat("x", i)
	}
	cases := []struct {
		name    string
		entries []batchEntry
		want    string
	}{
		{"one entry", entriesWithIDs("a"), ""},
		{"ten entries", entriesWithIDs(eleven[:10]...), ""},
		{"empty", nil, "AWS.SimpleQueueService.EmptyBatchRequest"},
		{"eleven entries", entriesWithIDs(eleven...), "AWS.SimpleQueueService.TooManyEntriesInBatchRequest"},
		{"missing id", entriesWithIDs(""), "AWS.SimpleQueueService.InvalidBatchEntryId"},
		{"bad characters", entriesWithIDs("a.b"), "AWS.SimpleQueueService.InvalidBatchEntryId"},
		{"id too long", entriesWithIDs(strings.Repeat("a", 81)), "AWS.SimpleQueueService.InvalidBatchEntryId"},
		{"repeated id", entriesWithIDs("a", "b", "a"), "AWS.SimpleQueueService.BatchEntryIdsNotDistinct"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorCode(t, validateBatchEntries(send, tc.entries)); got != tc.want {
				t.Fatalf("code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateSendBatch(t *testing.T) {
	half := strings.Repeat("x", maxPayloadBytes/2)
	if err := validateSendBatch([]batchEntry{{ID: "a", MessageBody: half}, {ID: "b", MessageBody: half}}); err != nil {
		t.Fatalf("exactly the limit: %v", err)
	}
	err := validateSendBatch([]batchEntry{{ID: "a", MessageBody: half}, {ID: "b", MessageBody: half + "x"}})
	if got := errorCode(t, err); got != "AWS.SimpleQueueService.BatchRequestTooLong" {
		t.Fatalf("over the limit: code = %q", got)
	}
	err = validateSendBatch([]batchEntry{{ID: "a", MessageBody: "m", HasAttributes: true}})
	if got := errorCode(t, err); got != "NotImplemented" {
		t.Fatalf("message attributes: code = %q", got)
	}
}

func TestValidateIntegerAttribute(t *testing.T) {
	cases := []struct {
		name, value string
		ok          bool
	}{
		{"MaximumMessageSize", "1024", true},
		{"MaximumMessageSize", "1048576", true},
		{"MaximumMessageSize", "1023", false},
		{"MaximumMessageSize", "1048577", false},
		{"DelaySeconds", "900", true},
		{"DelaySeconds", "901", false},
		{"VisibilityTimeout", "43200", true},
		{"VisibilityTimeout", "-1", false},
		{"ReceiveMessageWaitTimeSeconds", "21", false},
		{"MessageRetentionPeriod", "59", false},
		{"VisibilityTimeout", "soon", false},
	}
	for _, tc := range cases {
		err := validateIntegerAttribute(tc.name, tc.value)
		if (err == nil) != tc.ok {
			t.Errorf("%s=%s: err = %v, want ok=%v", tc.name, tc.value, err, tc.ok)
		}
		if err != nil && errorCode(t, err) != "InvalidAttributeValue" {
			t.Errorf("%s=%s: code = %q", tc.name, tc.value, errorCode(t, err))
		}
	}
}

func TestValidateMessageBody(t *testing.T) {
	queue := queueRecord{Attributes: map[string]string{"MaximumMessageSize": "1024"}}
	if err := validateMessageBody(queue, strings.Repeat("x", 1024)); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if got := errorCode(t, validateMessageBody(queue, strings.Repeat("x", 1025))); got != "InvalidParameterValue" {
		t.Fatalf("over the limit: code = %q", got)
	}
	if got := errorCode(t, validateMessageBody(queue, "")); got != "MissingParameter" {
		t.Fatalf("empty body: code = %q", got)
	}
	unset := queueRecord{Attributes: map[string]string{}}
	if err := validateMessageBody(unset, strings.Repeat("x", defaultMaximumMessageSize)); err != nil {
		t.Fatalf("default limit: %v", err)
	}
}

func TestParseDelaySeconds(t *testing.T) {
	queue := queueRecord{Attributes: map[string]string{"DelaySeconds": "5"}}
	if got, err := parseDelaySeconds(queue, ""); err != nil || got != 5 {
		t.Fatalf("queue default: got %d, %v", got, err)
	}
	if got, err := parseDelaySeconds(queue, "900"); err != nil || got != 900 {
		t.Fatalf("max: got %d, %v", got, err)
	}
	for _, raw := range []string{"901", "-1", "x"} {
		if _, err := parseDelaySeconds(queue, raw); errorCode(t, err) != "InvalidParameterValue" {
			t.Fatalf("%q: err = %v", raw, err)
		}
	}
}

func TestParseVisibilityTimeout(t *testing.T) {
	if got, err := parseVisibilityTimeout("43200"); err != nil || got != 43200 {
		t.Fatalf("max: got %d, %v", got, err)
	}
	if _, err := parseVisibilityTimeout(""); errorCode(t, err) != "MissingParameter" {
		t.Fatalf("missing: err = %v", err)
	}
	if _, err := parseVisibilityTimeout("43201"); errorCode(t, err) != "InvalidParameterValue" {
		t.Fatalf("too long: err = %v", err)
	}
}
