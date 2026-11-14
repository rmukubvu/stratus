package sqs

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stratus/internal/store/bbolt"
)

const queueURL = "http://localhost:4566/000000000000/orders"

type harness struct {
	t   *testing.T
	svc *Service
}

func newHarness(t *testing.T, attrs ...string) *harness {
	t.Helper()
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	h := &harness{t: t, svc: NewService(metadata)}

	form := url.Values{"QueueName": {"orders"}}
	for i := 0; i+1 < len(attrs); i += 2 {
		n := i/2 + 1
		form.Set(fmt.Sprintf("Attribute.%d.Name", n), attrs[i])
		form.Set(fmt.Sprintf("Attribute.%d.Value", n), attrs[i+1])
	}
	if _, err := h.query("CreateQueue", form); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	return h
}

// query calls an operation over the query protocol and returns the body.
func (h *harness) query(op string, form url.Values) (string, error) {
	h.t.Helper()
	form.Set("Action", op)
	if form.Get("QueueName") == "" && form.Get("QueueUrl") == "" {
		form.Set("QueueUrl", queueURL)
	}
	req := httptest.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	err := h.svc.Handle(rec, req, op, "req-1")
	return rec.Body.String(), err
}

// json calls an operation over the JSON protocol and decodes the body.
func (h *harness) json(op string, payload map[string]any) (map[string]any, error) {
	h.t.Helper()
	payload["QueueUrl"] = queueURL
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "http://localhost:4566/", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS."+op)
	rec := httptest.NewRecorder()
	if err := h.svc.Handle(rec, req, op, "req-1"); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("%s: decode %q: %v", op, rec.Body.String(), err)
	}
	return out, nil
}

// receive takes up to ten visible messages and returns their receipt
// handles.
func (h *harness) receive() []string {
	h.t.Helper()
	body, err := h.query("ReceiveMessage", url.Values{"MaxNumberOfMessages": {"10"}})
	if err != nil {
		h.t.Fatalf("receive: %v", err)
	}
	var resp receiveMessageResponse
	if err := xml.Unmarshal([]byte(body), &resp); err != nil {
		h.t.Fatalf("decode receive: %v", err)
	}
	handles := make([]string, 0, len(resp.Result.Messages))
	for _, m := range resp.Result.Messages {
		handles = append(handles, m.ReceiptHandle)
	}
	return handles
}

type batchXMLReply struct {
	XMLName   xml.Name
	RequestID string `xml:"ResponseMetadata>RequestId"`
	Result    struct {
		XMLName xml.Name
		Entries []struct {
			XMLName   xml.Name
			ID        string `xml:"Id"`
			MessageID string `xml:"MessageId"`
			MD5       string `xml:"MD5OfMessageBody"`
		} `xml:",any"`
	} `xml:",any"`
}

func decodeBatchXML(t *testing.T, body string) batchXMLReply {
	t.Helper()
	var reply batchXMLReply
	if err := xml.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return reply
}

func TestSendMessageBatchQueryReportsEachEntry(t *testing.T) {
	h := newHarness(t, "MaximumMessageSize", "1024")
	body, err := h.query("SendMessageBatch", url.Values{
		"SendMessageBatchRequestEntry.1.Id":          {"first"},
		"SendMessageBatchRequestEntry.1.MessageBody": {"hello"},
		"SendMessageBatchRequestEntry.2.Id":          {"too-big"},
		"SendMessageBatchRequestEntry.2.MessageBody": {strings.Repeat("x", 1025)},
		"SendMessageBatchRequestEntry.3.Id":          {"third"},
		"SendMessageBatchRequestEntry.3.MessageBody": {"world"},
	})
	if err != nil {
		t.Fatalf("send batch: %v", err)
	}
	reply := decodeBatchXML(t, body)
	if reply.XMLName.Local != "SendMessageBatchResponse" || reply.Result.XMLName.Local != "SendMessageBatchResult" || reply.RequestID != "req-1" {
		t.Fatalf("unexpected envelope: %s", body)
	}
	var ok, failed []string
	for _, e := range reply.Result.Entries {
		switch e.XMLName.Local {
		case "SendMessageBatchResultEntry":
			if e.MessageID == "" || e.MD5 == "" {
				t.Fatalf("entry %s lacks MessageId or MD5: %s", e.ID, body)
			}
			ok = append(ok, e.ID)
		case "BatchResultErrorEntry":
			failed = append(failed, e.ID)
		}
	}
	if strings.Join(ok, ",") != "first,third" || strings.Join(failed, ",") != "too-big" {
		t.Fatalf("successful %v, failed %v", ok, failed)
	}
	if !strings.Contains(body, "<SenderFault>true</SenderFault>") || !strings.Contains(body, "<Code>InvalidParameterValue</Code>") {
		t.Fatalf("failure entry lacks fault details: %s", body)
	}
	if got := len(h.receive()); got != 2 {
		t.Fatalf("received %d messages, want 2", got)
	}
}

func TestSendMessageBatchJSON(t *testing.T) {
	h := newHarness(t)
	out, err := h.json("SendMessageBatch", map[string]any{"Entries": []map[string]any{
		{"Id": "a", "MessageBody": "one"},
		{"Id": "b", "MessageBody": "two", "DelaySeconds": 901},
	}})
	if err != nil {
		t.Fatalf("send batch: %v", err)
	}
	successful := out["Successful"].([]any)
	failed := out["Failed"].([]any)
	if len(successful) != 1 || len(failed) != 1 {
		t.Fatalf("unexpected result: %v", out)
	}
	if s := successful[0].(map[string]any); s["Id"] != "a" || s["MessageId"] == "" || s["MD5OfMessageBody"] != md5Hex("one") {
		t.Fatalf("unexpected success entry: %v", s)
	}
	if f := failed[0].(map[string]any); f["Id"] != "b" || f["Code"] != "InvalidParameterValue" || f["SenderFault"] != true {
		t.Fatalf("unexpected failure entry: %v", f)
	}
}

func TestBatchRequestErrorsFailTheWholeRequest(t *testing.T) {
	h := newHarness(t)
	entries := make([]map[string]any, 11)
	for i := range entries {
		entries[i] = map[string]any{"Id": fmt.Sprintf("m%d", i), "MessageBody": "x"}
	}
	_, err := h.json("SendMessageBatch", map[string]any{"Entries": entries})
	if got := errorCode(t, err); got != "AWS.SimpleQueueService.TooManyEntriesInBatchRequest" {
		t.Fatalf("eleven entries: code = %q", got)
	}
	_, err = h.json("DeleteMessageBatch", map[string]any{"Entries": []map[string]any{}})
	if got := errorCode(t, err); got != "AWS.SimpleQueueService.EmptyBatchRequest" {
		t.Fatalf("no entries: code = %q", got)
	}
	_, err = h.query("ChangeMessageVisibilityBatch", url.Values{
		"ChangeMessageVisibilityBatchRequestEntry.1.Id": {"dup"},
		"ChangeMessageVisibilityBatchRequestEntry.2.Id": {"dup"},
	})
	if got := errorCode(t, err); got != "AWS.SimpleQueueService.BatchEntryIdsNotDistinct" {
		t.Fatalf("repeated ids: code = %q", got)
	}
	if got := len(h.receive()); got != 0 {
		t.Fatalf("a rejected batch stored %d messages", got)
	}
}

func TestSendMessageBatchToMissingQueue(t *testing.T) {
	h := newHarness(t)
	_, err := h.query("SendMessageBatch", url.Values{
		"QueueUrl":                                   {"http://localhost:4566/000000000000/missing"},
		"SendMessageBatchRequestEntry.1.Id":          {"a"},
		"SendMessageBatchRequestEntry.1.MessageBody": {"x"},
	})
	if got := errorCode(t, err); got != "AWS.SimpleQueueService.NonExistentQueue" {
		t.Fatalf("code = %q", got)
	}
}

func TestDeleteMessageBatch(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{"a", "b"} {
		if _, err := h.query("SendMessage", url.Values{"MessageBody": {body}}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	handles := h.receive()
	if len(handles) != 2 {
		t.Fatalf("received %d messages, want 2", len(handles))
	}
	out, err := h.json("DeleteMessageBatch", map[string]any{"Entries": []map[string]any{
		{"Id": "ok", "ReceiptHandle": handles[0]},
		{"Id": "bogus", "ReceiptHandle": "not-a-handle"},
		{"Id": "again", "ReceiptHandle": handles[0]},
	}})
	if err != nil {
		t.Fatalf("delete batch: %v", err)
	}
	if got := len(out["Successful"].([]any)); got != 1 {
		t.Fatalf("successful = %d, want 1: %v", got, out)
	}
	failed := out["Failed"].([]any)
	if len(failed) != 2 || failed[0].(map[string]any)["Code"] != "ReceiptHandleIsInvalid" {
		t.Fatalf("unexpected failures: %v", failed)
	}

	// Make the remaining message visible again: only one should be left.
	if _, err := h.query("ChangeMessageVisibility", url.Values{"ReceiptHandle": {handles[1]}, "VisibilityTimeout": {"0"}}); err != nil {
		t.Fatalf("change visibility: %v", err)
	}
	if got := len(h.receive()); got != 1 {
		t.Fatalf("%d messages left, want 1", got)
	}
}

func TestChangeMessageVisibilityBatch(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{"a", "b"} {
		if _, err := h.query("SendMessage", url.Values{"MessageBody": {body}}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	handles := h.receive()
	body, err := h.query("ChangeMessageVisibilityBatch", url.Values{
		"ChangeMessageVisibilityBatchRequestEntry.1.Id":                {"now"},
		"ChangeMessageVisibilityBatchRequestEntry.1.ReceiptHandle":     {handles[0]},
		"ChangeMessageVisibilityBatchRequestEntry.1.VisibilityTimeout": {"0"},
		"ChangeMessageVisibilityBatchRequestEntry.2.Id":                {"too-long"},
		"ChangeMessageVisibilityBatchRequestEntry.2.ReceiptHandle":     {handles[1]},
		"ChangeMessageVisibilityBatchRequestEntry.2.VisibilityTimeout": {"43201"},
	})
	if err != nil {
		t.Fatalf("change visibility batch: %v", err)
	}
	reply := decodeBatchXML(t, body)
	names := map[string]string{}
	for _, e := range reply.Result.Entries {
		names[e.ID] = e.XMLName.Local
	}
	if names["now"] != "ChangeMessageVisibilityBatchResultEntry" || names["too-long"] != "BatchResultErrorEntry" {
		t.Fatalf("unexpected entries %v in %s", names, body)
	}
	if got := len(h.receive()); got != 1 {
		t.Fatalf("%d messages visible again, want 1", got)
	}
}

func TestSendMessageEnforcesMaximumMessageSize(t *testing.T) {
	h := newHarness(t, "MaximumMessageSize", "1024")
	if _, err := h.query("SendMessage", url.Values{"MessageBody": {strings.Repeat("x", 1024)}}); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	_, err := h.query("SendMessage", url.Values{"MessageBody": {strings.Repeat("x", 1025)}})
	if got := errorCode(t, err); got != "InvalidParameterValue" {
		t.Fatalf("over the limit: code = %q", got)
	}
}

func TestCreateQueueRejectsOutOfRangeAttributes(t *testing.T) {
	h := newHarness(t)
	_, err := h.query("CreateQueue", url.Values{
		"QueueName":         {"huge"},
		"Attribute.1.Name":  {"MaximumMessageSize"},
		"Attribute.1.Value": {"2000000"},
	})
	if got := errorCode(t, err); got != "InvalidAttributeValue" {
		t.Fatalf("code = %q", got)
	}
}
