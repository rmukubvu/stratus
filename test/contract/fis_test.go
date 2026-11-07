package contract

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// awsCall sends a request signed (in shape only) for service, the way AWS
// SDKs do, so Stratus routes it by its SigV4 signing name.
func awsCall(t *testing.T, method, endpoint, service, contentType, payload string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20261107/us-east-1/"+service+"/aws4_request, SignedHeaders=host, Signature=x")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, endpoint, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func queryCall(t *testing.T, base, service string, form url.Values) string {
	t.Helper()
	status, body := awsCall(t, http.MethodPost, base+"/", service, "application/x-www-form-urlencoded", form.Encode())
	if status != http.StatusOK {
		t.Fatalf("%s %s: %d %s", service, form.Get("Action"), status, body)
	}
	return body
}

// TestDuplicateDeliveryFromSNSToSQS checks that an event-scope chaos rule
// reaches inside SNS fan-out: one publish lands in the queue twice.
func TestDuplicateDeliveryFromSNSToSQS(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	queryCall(t, h.baseURL, "sqs", url.Values{"Action": {"CreateQueue"}, "QueueName": {"audit"}})
	topic := between(queryCall(t, h.baseURL, "sns", url.Values{"Action": {"CreateTopic"}, "Name": {"orders"}}), "<TopicArn>", "</TopicArn>")
	queryCall(t, h.baseURL, "sns", url.Values{"Action": {"Subscribe"}, "TopicArn": {topic}, "Protocol": {"sqs"},
		"Endpoint": {"arn:aws:sqs:us-east-1:000000000000:audit"}})

	mustStatus(t, http.MethodPut, h.baseURL+"/_stratus/control/v1/chaos", `{
		"seed": 1,
		"rules": [{"id": "dupe", "source": "sns", "service": "sqs", "resource": "audit", "action": "duplicate"}]
	}`, http.StatusCreated)
	queryCall(t, h.baseURL, "sns", url.Values{"Action": {"Publish"}, "TopicArn": {topic}, "Message": {"order-1"}})

	queueURL := h.baseURL + "/000000000000/audit"
	got := queryCall(t, h.baseURL, "sqs", url.Values{"Action": {"ReceiveMessage"}, "QueueUrl": {queueURL}, "MaxNumberOfMessages": {"10"}})
	if n := strings.Count(got, "<Message>"); n != 2 {
		t.Fatalf("want the message delivered twice, got %d: %s", n, got)
	}

	raw := mustStatus(t, http.MethodGet, h.baseURL+"/_stratus/control/v1/chaos", "", http.StatusOK)
	var report struct {
		RecentHits []struct{ Scope, Source, SourceARN string } `json:"recent_hits"`
	}
	_ = json.Unmarshal([]byte(raw), &report)
	if len(report.RecentHits) != 1 || report.RecentHits[0].Scope != "event" || report.RecentHits[0].Source != "sns" {
		t.Fatalf("the hit should record the delivery: %s", raw)
	}
}

// TestFISExperimentDrivesChaos runs an FIS template through the REST API
// and checks its fault reaches DynamoDB requests.
func TestFISExperimentDrivesChaos(t *testing.T) {
	h := startHarness(t)
	defer h.Close()

	status, body := awsCall(t, http.MethodPost, h.baseURL+"/experimentTemplates", "fis", "application/json", `{
		"description": "throttle orders writes",
		"targets": {"roles": {"resourceType": "aws:iam:role", "resourceArns": ["arn:aws:iam::000000000000:role/app"], "selectionMode": "ALL"}},
		"actions": {"throttle": {"actionId": "aws:fis:inject-api-throttle-error", "targets": {"Roles": "roles"},
			"parameters": {"service": "dynamodb", "operations": "PutItem", "percentage": "100", "duration": "PT5M"}}},
		"stopConditions": [{"source": "none"}],
		"roleArn": "arn:aws:iam::000000000000:role/fis"
	}`)
	if status != http.StatusOK {
		t.Fatalf("create template: %d %s", status, body)
	}
	var created struct {
		ExperimentTemplate struct{ ID string } `json:"experimentTemplate"`
	}
	_ = json.Unmarshal([]byte(body), &created)

	status, body = awsCall(t, http.MethodPost, h.baseURL+"/experiments", "fis", "application/json",
		`{"experimentTemplateId": "`+created.ExperimentTemplate.ID+`", "tags": {"stratus:seed": "42"}}`)
	if status != http.StatusOK || !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("start experiment: %d %s", status, body)
	}
	var started struct {
		Experiment struct{ ID string } `json:"experiment"`
	}
	_ = json.Unmarshal([]byte(body), &started)

	req, _ := http.NewRequest(http.MethodPost, h.baseURL+"/", strings.NewReader(`{"TableName": "orders", "Item": {}}`))
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810.PutItem")
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("X-Stratus-Fault") != "throttle" {
		t.Fatalf("PutItem: status %d, fault %q", resp.StatusCode, resp.Header.Get("X-Stratus-Fault"))
	}

	status, body = awsCall(t, http.MethodDelete, h.baseURL+"/experiments/"+started.Experiment.ID, "fis", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"status":"stopped"`) {
		t.Fatalf("stop experiment: %d %s", status, body)
	}
	// The FIS paths must not shadow S3: an unsigned request is still S3.
	mustStatus(t, http.MethodPut, h.baseURL+"/experiments", "", http.StatusOK)
}

func between(s, open, close string) string {
	_, rest, _ := strings.Cut(s, open)
	v, _, _ := strings.Cut(rest, close)
	return v
}
