package contract

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSESCapturesSentEmailInTheInbox sends through SES v1, SES v2 and the
// AWS CLI, then reads everything back from the control API inbox.
func TestSESCapturesSentEmailInTheInbox(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"

	// The sandbox rule applies: unverified senders are rejected.
	status, body := awsCall(t, http.MethodPost, h.baseURL+"/", "ses", "application/x-www-form-urlencoded", url.Values{
		"Action": {"SendEmail"}, "Source": {"noreply@shop.example"}, "Destination.ToAddresses.member.1": {"ada@b.example"},
		"Message.Subject.Data": {"Hi"}, "Message.Body.Text.Data": {"x"},
	}.Encode())
	if status != http.StatusBadRequest || !strings.Contains(body, "<Code>MessageRejected</Code>") || !strings.Contains(body, "verify-email-identity") {
		t.Fatalf("unverified sender: %d %s", status, body)
	}

	queryCall(t, h.baseURL, "ses", url.Values{"Action": {"VerifyDomainIdentity"}, "Domain": {"shop.example"}})
	sent := queryCall(t, h.baseURL, "ses", url.Values{
		"Action": {"SendEmail"}, "Source": {"noreply@shop.example"}, "Destination.ToAddresses.member.1": {"ada@b.example"},
		"Message.Subject.Data": {"Welcome Ada"}, "Message.Body.Text.Data": {"Thanks for signing up"}, "Message.Body.Html.Data": {"<p>Thanks</p>"},
	})
	id := between(sent, "<MessageId>", "</MessageId>")
	if id == "" {
		t.Fatalf("SendEmail should return a MessageId: %s", sent)
	}

	status, body = awsCall(t, http.MethodPost, h.baseURL+"/v2/email/outbound-emails", "ses", "application/json",
		`{"FromEmailAddress":"orders@shop.example","Destination":{"ToAddresses":["bob@b.example"]},"Content":{"Simple":{"Subject":{"Data":"Order 42"},"Body":{"Text":{"Data":"Shipped"}}}}}`)
	if status != http.StatusOK || !strings.Contains(body, "MessageId") {
		t.Fatalf("sesv2 send: %d %s", status, body)
	}

	// The real CLI signs for "ses" and parses the XML.
	runAWS(t, h.baseURL, "ses", "send-email", "--from", "noreply@shop.example", "--destination", "ToAddresses=cli@b.example",
		"--message", "Subject={Data=From the CLI},Body={Text={Data=hello}}")
	if out := runAWS(t, h.baseURL, "ses", "list-identities"); !strings.Contains(string(out), "shop.example") {
		t.Fatalf("list-identities: %s", out)
	}
	if out := runAWS(t, h.baseURL, "sesv2", "get-account"); !strings.Contains(string(out), "SendQuota") {
		t.Fatalf("sesv2 get-account: %s", out)
	}

	var inbox struct {
		Count int `json:"count"`
		Items []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			API     string `json:"api"`
			Body    struct {
				Text string `json:"text"`
				HTML string `json:"html"`
			} `json:"body"`
		} `json:"items"`
	}
	_ = json.Unmarshal([]byte(mustStatus(t, http.MethodGet, control+"/ses/messages", "", http.StatusOK)), &inbox)
	if inbox.Count != 3 || inbox.Items[0].Subject != "Welcome Ada" || inbox.Items[0].Body.HTML != "<p>Thanks</p>" || inbox.Items[1].API != "sesv2" || inbox.Items[2].Subject != "From the CLI" {
		t.Fatalf("inbox should hold all three sends, oldest first: %+v", inbox)
	}
	_ = json.Unmarshal([]byte(mustStatus(t, http.MethodGet, control+"/ses/messages?to=bob@b.example", "", http.StatusOK)), &inbox)
	if inbox.Count != 1 || inbox.Items[0].Subject != "Order 42" {
		t.Fatalf("recipient filter: %+v", inbox)
	}
	if one := mustStatus(t, http.MethodGet, control+"/ses/messages/"+id, "", http.StatusOK); !strings.Contains(one, "Thanks for signing up") {
		t.Fatalf("message by id: %s", one)
	}
	mustStatus(t, http.MethodGet, control+"/ses/messages/nope", "", http.StatusNotFound)

	var cleared struct {
		Cleared int `json:"cleared"`
	}
	_ = json.Unmarshal([]byte(mustStatus(t, http.MethodDelete, control+"/ses/messages", "", http.StatusOK)), &cleared)
	if cleared.Cleared != 3 {
		t.Fatalf("clear: %+v", cleared)
	}
}

// TestSchedulerFastForwardDeliversToSQS creates an hourly schedule, moves
// the scheduler clock an hour ahead and finds the message in the queue.
func TestSchedulerFastForwardDeliversToSQS(t *testing.T) {
	h := startHarness(t)
	defer h.Close()
	control := h.baseURL + "/_stratus/control/v1"
	queryCall(t, h.baseURL, "sqs", url.Values{"Action": {"CreateQueue"}, "QueueName": {"jobs"}})
	queueURL := h.baseURL + "/000000000000/jobs"

	status, body := awsCall(t, http.MethodPost, h.baseURL+"/schedules/nightly", "scheduler", "application/json",
		`{"ScheduleExpression":"rate(1 hour)","FlexibleTimeWindow":{"Mode":"OFF"},"Target":{"Arn":"arn:aws:sqs:us-east-1:000000000000:jobs","RoleArn":"arn:aws:iam::000000000000:role/scheduler","Input":"{\"job\":\"nightly\",\"at\":\"<aws.scheduler.scheduled-time>\"}"}}`)
	if status != http.StatusOK || !strings.Contains(body, "schedule/default/nightly") {
		t.Fatalf("create schedule: %d %s", status, body)
	}
	status, body = awsCall(t, http.MethodGet, h.baseURL+"/schedules/nightly", "scheduler", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"State":"ENABLED"`) || !strings.Contains(body, `"CreationDate":`) {
		t.Fatalf("get schedule: %d %s", status, body)
	}
	if out := runAWS(t, h.baseURL, "scheduler", "list-schedules"); !strings.Contains(string(out), "nightly") {
		t.Fatalf("the CLI should list the schedule: %s", out)
	}

	received := func() string {
		return queryCall(t, h.baseURL, "sqs", url.Values{"Action": {"ReceiveMessage"}, "QueueUrl": {queueURL}, "MaxNumberOfMessages": {"10"}})
	}
	if got := received(); strings.Contains(got, "<Message>") {
		t.Fatalf("nothing is due yet: %s", got)
	}

	var advanced struct {
		Offset string `json:"offset"`
		Fired  []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"fired"`
	}
	_ = json.Unmarshal([]byte(mustStatus(t, http.MethodPost, control+"/scheduler/advance", `{"by":"1h"}`, http.StatusOK)), &advanced)
	if advanced.Offset != "1h0m0s" || len(advanced.Fired) != 1 || advanced.Fired[0].Name != "nightly" || advanced.Fired[0].Error != "" {
		t.Fatalf("advance: %+v", advanced)
	}
	got := received()
	if strings.Count(got, "<Message>") != 1 || !strings.Contains(got, "&#34;job&#34;:&#34;nightly&#34;") || strings.Contains(got, "<aws.scheduler") {
		t.Fatalf("the queue should hold the rendered input: %s", got)
	}

	if run := mustStatus(t, http.MethodPost, control+"/scheduler/run", `{"name":"nightly"}`, http.StatusOK); !strings.Contains(run, "execution_id") {
		t.Fatalf("run: %s", run)
	}
	if got := received(); strings.Count(got, "<Message>") != 1 {
		t.Fatalf("a manual run delivers once more: %s", got)
	}

	status, body = awsCall(t, http.MethodPost, h.baseURL+"/schedules/bad", "scheduler", "application/json",
		`{"ScheduleExpression":"hourly","FlexibleTimeWindow":{"Mode":"OFF"},"Target":{"Arn":"arn:aws:sqs:us-east-1:000000000000:jobs"}}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "ValidationException") {
		t.Fatalf("bad expression: %d %s", status, body)
	}

	// Resetting the service also returns its clock to real time.
	mustStatus(t, http.MethodPost, control+"/reset", `{"services":["scheduler"]}`, http.StatusOK)
	var clock struct {
		Offset string `json:"offset"`
	}
	_ = json.Unmarshal([]byte(mustStatus(t, http.MethodGet, control+"/scheduler/clock", "", http.StatusOK)), &clock)
	if clock.Offset != "0s" {
		t.Fatalf("clock after reset: %+v", clock)
	}
	status, _ = awsCall(t, http.MethodGet, h.baseURL+"/schedules/nightly", "scheduler", "", "")
	if status != http.StatusNotFound {
		t.Fatalf("reset should remove the schedule, got %d", status)
	}
}
