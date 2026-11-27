package sqs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stratus/internal/store/bbolt"
)

func newQueueService(t *testing.T) *Service {
	t.Helper()
	meta, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	return NewService(meta)
}

func callJSON(t *testing.T, svc *Service, op, body string) (map[string]any, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Amz-Target", "AmazonSQS."+op)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	w := httptest.NewRecorder()
	if err := svc.Handle(w, req, op, "req"); err != nil {
		return nil, err
	}
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out, nil
}

func callQuery(t *testing.T, svc *Service, op string, params ...string) (string, error) {
	t.Helper()
	form := url.Values{"Action": {op}}
	for i := 0; i+1 < len(params); i += 2 {
		form.Set(params[i], params[i+1])
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	if err := svc.Handle(w, req, op, "req"); err != nil {
		return "", err
	}
	return w.Body.String(), nil
}

func TestQueueTagsSetAtCreationAndByTagQueueReadBackOverJSON(t *testing.T) {
	svc := newQueueService(t)
	out, err := callJSON(t, svc, "CreateQueue", `{"QueueName":"orders","tags":{"team":"payments"},"Attributes":{"SqsManagedSseEnabled":"true"}}`)
	if err != nil {
		t.Fatal(err)
	}
	queueURL := out["QueueUrl"].(string)
	if _, err := callJSON(t, svc, "TagQueue", `{"QueueUrl":"`+queueURL+`","Tags":{"env":"dev"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := callJSON(t, svc, "UntagQueue", `{"QueueUrl":"`+queueURL+`","TagKeys":["team"]}`); err != nil {
		t.Fatal(err)
	}
	out, err = callJSON(t, svc, "ListQueueTags", `{"QueueUrl":"`+queueURL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(out["Tags"]); string(raw) != `{"env":"dev"}` {
		t.Errorf("tags = %s", raw)
	}
	attrs, err := callJSON(t, svc, "GetQueueAttributes", `{"QueueUrl":"`+queueURL+`","AttributeNames":["All"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := attrs["Attributes"].(map[string]any)["SqsManagedSseEnabled"]; got != "true" {
		t.Errorf("SqsManagedSseEnabled = %v", got)
	}
}

func TestQueueTagsOverTheQueryProtocol(t *testing.T) {
	svc := newQueueService(t)
	if _, err := callQuery(t, svc, "CreateQueue", "QueueName", "orders", "Tag.1.Key", "team", "Tag.1.Value", "payments"); err != nil {
		t.Fatal(err)
	}
	out, err := callQuery(t, svc, "ListQueueTags", "QueueUrl", "http://localhost:4566/000000000000/orders")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<ListQueueTagsResult><Tag><Key>team</Key><Value>payments</Value></Tag></ListQueueTagsResult>") {
		t.Errorf("ListQueueTags =\n%s", out)
	}
	_, err = callQuery(t, svc, "TagQueue", "QueueUrl", "http://localhost:4566/000000000000/orders", "Tag.1.Key", "aws:x", "Tag.1.Value", "y")
	if got := errorCode(t, err); got != "InvalidParameterValue" {
		t.Errorf("reserved key: %s", got)
	}
}

func TestEncryptionAttributesAreValidated(t *testing.T) {
	cases := map[string]string{
		"SqsManagedSseEnabled":         "maybe",
		"KmsDataKeyReusePeriodSeconds": "30",
	}
	for name, value := range cases {
		if _, err := normalizeAttributes(map[string]string{name: value}); errorCode(t, err) != "InvalidAttributeValue" {
			t.Errorf("%s=%s: %v", name, value, err)
		}
	}
	attrs, err := normalizeAttributes(map[string]string{"KmsMasterKeyId": "alias/aws/sqs", "KmsDataKeyReusePeriodSeconds": "300"})
	if err != nil || attrs["KmsMasterKeyId"] != "alias/aws/sqs" {
		t.Errorf("attrs = %v, %v", attrs, err)
	}
}

func TestARedrivePolicyMayGiveMaxReceiveCountAsANumber(t *testing.T) {
	for _, raw := range []string{
		`{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:dlq","maxReceiveCount":5}`,
		`{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:dlq","maxReceiveCount":"5"}`,
	} {
		if p, err := decodeRedrivePolicy(raw); err != nil || p.MaxReceiveCount != 5 {
			t.Errorf("%s: %+v, %v", raw, p, err)
		}
	}
}
