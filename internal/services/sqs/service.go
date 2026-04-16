package sqs

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/store"
)

const (
	namespace      = "http://queue.amazonaws.com/doc/2012-11-05/"
	queuesBucket   = "sqs-queues"
	messagesBucket = "sqs-messages"
	accountID      = "000000000000"
)

var queueNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

type Service struct {
	metadata store.Store
	lambda   *lambdasvc.Service
	now      func() time.Time
	mu       sync.Mutex
}

type queueRecord struct {
	Name       string            `json:"name"`
	CreatedAt  time.Time         `json:"created_at"`
	Attributes map[string]string `json:"attributes"`
}

type messageRecord struct {
	QueueName             string    `json:"queue_name"`
	MessageID             string    `json:"message_id"`
	Body                  string    `json:"body"`
	MD5OfBody             string    `json:"md5_of_body"`
	SentAt                time.Time `json:"sent_at"`
	FirstReceivedAt       time.Time `json:"first_received_at,omitempty"`
	ReceiveCount          int       `json:"receive_count"`
	ReceiptHandle         string    `json:"receipt_handle,omitempty"`
	VisibilityDeadlineUTC time.Time `json:"visibility_deadline_utc,omitempty"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

type createQueueResponse struct {
	XMLName          xml.Name         `xml:"CreateQueueResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           queueURLResult   `xml:"CreateQueueResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type getQueueURLResponse struct {
	XMLName          xml.Name         `xml:"GetQueueUrlResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           queueURLResult   `xml:"GetQueueUrlResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type listQueuesResponse struct {
	XMLName          xml.Name         `xml:"ListQueuesResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           listQueuesResult `xml:"ListQueuesResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type getQueueAttributesResponse struct {
	XMLName          xml.Name                 `xml:"GetQueueAttributesResponse"`
	XMLNS            string                   `xml:"xmlns,attr"`
	Result           getQueueAttributesResult `xml:"GetQueueAttributesResult"`
	ResponseMetadata responseMetadata         `xml:"ResponseMetadata"`
}

type sendMessageResponse struct {
	XMLName          xml.Name          `xml:"SendMessageResponse"`
	XMLNS            string            `xml:"xmlns,attr"`
	Result           sendMessageResult `xml:"SendMessageResult"`
	ResponseMetadata responseMetadata  `xml:"ResponseMetadata"`
}

type receiveMessageResponse struct {
	XMLName          xml.Name             `xml:"ReceiveMessageResponse"`
	XMLNS            string               `xml:"xmlns,attr"`
	Result           receiveMessageResult `xml:"ReceiveMessageResult"`
	ResponseMetadata responseMetadata     `xml:"ResponseMetadata"`
}

type deleteMessageResponse struct {
	XMLName          xml.Name         `xml:"DeleteMessageResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           struct{}         `xml:"DeleteMessageResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type deleteQueueResponse struct {
	XMLName          xml.Name         `xml:"DeleteQueueResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           struct{}         `xml:"DeleteQueueResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type setQueueAttributesResponse struct {
	XMLName          xml.Name         `xml:"SetQueueAttributesResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           struct{}         `xml:"SetQueueAttributesResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type changeMessageVisibilityResponse struct {
	XMLName          xml.Name         `xml:"ChangeMessageVisibilityResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           struct{}         `xml:"ChangeMessageVisibilityResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type queueURLResult struct {
	QueueURL string `xml:"QueueUrl"`
}

type listQueuesResult struct {
	QueueURLs []string `xml:"QueueUrl"`
}

type getQueueAttributesResult struct {
	Attributes []attributeEntry `xml:"Attribute"`
}

type attributeEntry struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

type sendMessageResult struct {
	MessageID string `xml:"MessageId"`
	MD5OfBody string `xml:"MD5OfMessageBody"`
}

type receiveMessageResult struct {
	Messages []receivedMessage `xml:"Message"`
}

type receivedMessage struct {
	MessageID     string           `xml:"MessageId"`
	ReceiptHandle string           `xml:"ReceiptHandle"`
	MD5OfBody     string           `xml:"MD5OfBody"`
	Body          string           `xml:"Body"`
	Attributes    []attributeEntry `xml:"Attribute,omitempty"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) SetLambda(lambda *lambdasvc.Service) {
	s.lambda = lambda
}

func (s *Service) SendMessageToARN(queueArn string, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	queueName := queueNameFromARN(queueArn)
	queue, err := s.loadQueue(queueName)
	if err != nil {
		return err
	}
	record := messageRecord{
		QueueName: queue.Name,
		MessageID: uuid.NewString(),
		Body:      body,
		MD5OfBody: md5Hex(body),
		SentAt:    s.now().UTC(),
	}
	if err := s.putMessage(record); err != nil {
		return internal(err)
	}
	if s.lambda != nil {
		s.lambda.DispatchSQSEvent(context.Background(), queueARN(queue.Name), record.MessageID, uuid.NewString(), record.Body, map[string]string{
			"ApproximateReceiveCount": "1",
			"SentTimestamp":           strconv.FormatInt(record.SentAt.UnixMilli(), 10),
		})
	}
	return nil
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	if isJSONTargetRequest(r) {
		return s.handleJSON(w, r, operation, requestID)
	}

	switch operation {
	case "CreateQueue":
		return s.createQueue(w, r, requestID)
	case "GetQueueUrl":
		return s.getQueueURL(w, r, requestID)
	case "ListQueues":
		return s.listQueues(w, r, requestID)
	case "GetQueueAttributes":
		return s.getQueueAttributes(w, r, requestID)
	case "SetQueueAttributes":
		return s.setQueueAttributes(w, r, requestID)
	case "SendMessage":
		return s.sendMessage(w, r, requestID)
	case "ReceiveMessage":
		return s.receiveMessage(w, r, requestID)
	case "ChangeMessageVisibility":
		return s.changeMessageVisibility(w, r, requestID)
	case "DeleteMessage":
		return s.deleteMessage(w, r, requestID)
	case "DeleteQueue":
		return s.deleteQueue(w, r, requestID)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "sqs operation is not implemented",
		}
	}
}

func isJSONTargetRequest(r *http.Request) bool {
	target := r.Header.Get("X-Amz-Target")
	return strings.HasPrefix(target, "AmazonSQS.") || strings.Contains(r.Header.Get("Content-Type"), "application/x-amz-json-1.0")
}

func (s *Service) handleJSON(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	form, err := jsonRequestToForm(r, operation)
	if err != nil {
		return err
	}

	xmlReq := r.Clone(r.Context())
	xmlReq.Header = r.Header.Clone()
	xmlReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	xmlReq.Body = io.NopCloser(bytes.NewBufferString(form.Encode()))
	xmlReq.ContentLength = int64(len(form.Encode()))

	recorder := httptest.NewRecorder()
	switch operation {
	case "CreateQueue":
		if err := s.createQueue(recorder, xmlReq, requestID); err != nil {
			return err
		}
		var resp createQueueResponse
		if err := xml.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			return internal(err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"QueueUrl": resp.Result.QueueURL})
		return nil
	case "GetQueueUrl":
		if err := s.getQueueURL(recorder, xmlReq, requestID); err != nil {
			return err
		}
		var resp getQueueURLResponse
		if err := xml.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			return internal(err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"QueueUrl": resp.Result.QueueURL})
		return nil
	case "ListQueues":
		if err := s.listQueues(recorder, xmlReq, requestID); err != nil {
			return err
		}
		var resp listQueuesResponse
		if err := xml.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			return internal(err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"QueueUrls": resp.Result.QueueURLs})
		return nil
	case "GetQueueAttributes":
		if err := s.getQueueAttributes(recorder, xmlReq, requestID); err != nil {
			return err
		}
		var resp getQueueAttributesResponse
		if err := xml.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			return internal(err)
		}
		attrs := map[string]string{}
		for _, attr := range resp.Result.Attributes {
			attrs[attr.Name] = attr.Value
		}
		writeJSON(w, http.StatusOK, map[string]any{"Attributes": attrs})
		return nil
	case "ListQueueTags":
		s.mu.Lock()
		defer s.mu.Unlock()

		if _, err := s.queueFromForm(r, form); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"Tags": map[string]string{}})
		return nil
	case "SetQueueAttributes":
		if err := s.setQueueAttributes(recorder, xmlReq, requestID); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	case "SendMessage":
		if err := s.sendMessage(recorder, xmlReq, requestID); err != nil {
			return err
		}
		var resp sendMessageResponse
		if err := xml.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			return internal(err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"MD5OfMessageBody": resp.Result.MD5OfBody,
			"MessageId":        resp.Result.MessageID,
		})
		return nil
	case "ReceiveMessage":
		if err := s.receiveMessage(recorder, xmlReq, requestID); err != nil {
			return err
		}
		var resp receiveMessageResponse
		if err := xml.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			return internal(err)
		}
		items := make([]map[string]any, 0, len(resp.Result.Messages))
		for _, message := range resp.Result.Messages {
			attrs := map[string]string{}
			for _, attr := range message.Attributes {
				attrs[attr.Name] = attr.Value
			}
			items = append(items, map[string]any{
				"Attributes":    attrs,
				"Body":          message.Body,
				"MD5OfBody":     message.MD5OfBody,
				"MessageId":     message.MessageID,
				"ReceiptHandle": message.ReceiptHandle,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"Messages": items})
		return nil
	case "ChangeMessageVisibility":
		if err := s.changeMessageVisibility(recorder, xmlReq, requestID); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	case "DeleteMessage":
		if err := s.deleteMessage(recorder, xmlReq, requestID); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	case "DeleteQueue":
		if err := s.deleteQueue(recorder, xmlReq, requestID); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "sqs json operation is not implemented",
		}
	}
}

func jsonRequestToForm(r *http.Request, operation string) (url.Values, error) {
	var payload map[string]any
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
			return nil, badRequest("InvalidParameterValue", "request body is not valid JSON")
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}

	form := url.Values{}
	form.Set("Action", operation)
	switch operation {
	case "CreateQueue":
		setStringField(form, "QueueName", payload["QueueName"])
		appendAttributeMap(form, payload["Attributes"])
	case "GetQueueUrl":
		setStringField(form, "QueueName", payload["QueueName"])
	case "ListQueues":
		setStringField(form, "QueueNamePrefix", payload["QueueNamePrefix"])
	case "GetQueueAttributes":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
		appendStringList(form, "AttributeName", payload["AttributeNames"])
	case "ListQueueTags":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
	case "SetQueueAttributes":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
		appendAttributeMap(form, payload["Attributes"])
	case "SendMessage":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
		setStringField(form, "MessageBody", payload["MessageBody"])
		setStringField(form, "DelaySeconds", payload["DelaySeconds"])
	case "ReceiveMessage":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
		setStringField(form, "MaxNumberOfMessages", payload["MaxNumberOfMessages"])
		setStringField(form, "VisibilityTimeout", payload["VisibilityTimeout"])
		setStringField(form, "WaitTimeSeconds", payload["WaitTimeSeconds"])
		appendStringList(form, "AttributeName", payload["AttributeNames"])
	case "ChangeMessageVisibility":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
		setStringField(form, "ReceiptHandle", payload["ReceiptHandle"])
		setStringField(form, "VisibilityTimeout", payload["VisibilityTimeout"])
	case "DeleteMessage":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
		setStringField(form, "ReceiptHandle", payload["ReceiptHandle"])
	case "DeleteQueue":
		setStringField(form, "QueueUrl", payload["QueueUrl"])
	}
	return form, nil
}

func appendAttributeMap(form url.Values, raw any) {
	attrs, ok := raw.(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for idx, key := range keys {
		form.Set(fmt.Sprintf("Attribute.%d.Name", idx+1), key)
		form.Set(fmt.Sprintf("Attribute.%d.Value", idx+1), scalarString(attrs[key]))
	}
}

func appendStringList(form url.Values, name string, raw any) {
	list, ok := raw.([]any)
	if !ok {
		return
	}
	for idx, item := range list {
		form.Set(fmt.Sprintf("%s.%d", name, idx+1), scalarString(item))
	}
}

func setStringField(form url.Values, key string, value any) {
	if rendered := scalarString(value); rendered != "" {
		form.Set(key, rendered)
	}
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return fmt.Sprint(typed)
	}
}

func (s *Service) createQueue(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	name := form.Get("QueueName")
	if err := validateQueueName(name); err != nil {
		return err
	}

	attrs, err := requestedAttributes(form)
	if err != nil {
		return err
	}
	attrs, err = normalizeAttributes(attrs)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, err := s.loadQueue(name); err == nil {
		if !queueAttributesEqual(existing.Attributes, attrs) {
			return badRequest("QueueNameExists", "A queue already exists with the same name and a different value for one or more attributes.")
		}
		writeXML(w, http.StatusOK, createQueueResponse{
			XMLNS: namespace,
			Result: queueURLResult{
				QueueURL: queueURL(r, name),
			},
			ResponseMetadata: responseMetadata{RequestID: requestID},
		})
		return nil
	}

	record := queueRecord{
		Name:       name,
		CreatedAt:  s.now().UTC(),
		Attributes: attrs,
	}
	if err := s.putQueue(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, createQueueResponse{
		XMLNS: namespace,
		Result: queueURLResult{
			QueueURL: queueURL(r, name),
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) getQueueURL(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	name := form.Get("QueueName")
	if err := validateQueueName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadQueue(name); err != nil {
		return err
	}

	writeXML(w, http.StatusOK, getQueueURLResponse{
		XMLNS: namespace,
		Result: queueURLResult{
			QueueURL: queueURL(r, name),
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) listQueues(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	prefix := form.Get("QueueNamePrefix")

	var urls []string
	if err := s.metadata.Scan(queuesBucket, "", func(_, v []byte) error {
		var record queueRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if prefix == "" || strings.HasPrefix(record.Name, prefix) {
			urls = append(urls, queueURL(r, record.Name))
		}
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Strings(urls)

	writeXML(w, http.StatusOK, listQueuesResponse{
		XMLNS: namespace,
		Result: listQueuesResult{
			QueueURLs: urls,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) getQueueAttributes(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return err
	}

	requested := requestedAttributeNames(form)
	attrs, err := s.queueAttributes(r, queue, requested)
	if err != nil {
		return err
	}

	writeXML(w, http.StatusOK, getQueueAttributesResponse{
		XMLNS: namespace,
		Result: getQueueAttributesResult{
			Attributes: attrs,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) setQueueAttributes(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}

	updates, err := requestedAttributes(form)
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		return badRequest("MissingParameter", "The request must contain queue attributes.")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return err
	}

	merged := cloneStringMap(queue.Attributes)
	for name, value := range updates {
		merged[name] = value
	}
	merged, err = normalizeAttributes(merged)
	if err != nil {
		return err
	}
	queue.Attributes = merged
	if err := s.putQueue(queue); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, setQueueAttributesResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) sendMessage(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return err
	}
	body := form.Get("MessageBody")
	if body == "" {
		return badRequest("MissingParameter", "The request must contain the parameter MessageBody.")
	}
	if hasIndexedPrefix(form, "MessageAttribute.") {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "message attributes are not implemented",
		}
	}

	delaySeconds := attributeInt(queue.Attributes, "DelaySeconds", 0)
	if raw := form.Get("DelaySeconds"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return badRequest("InvalidParameterValue", "DelaySeconds must be a non-negative integer.")
		}
		delaySeconds = value
	}

	record := messageRecord{
		QueueName: queue.Name,
		MessageID: uuid.NewString(),
		Body:      body,
		MD5OfBody: md5Hex(body),
		SentAt:    s.now().UTC(),
	}
	if delaySeconds > 0 {
		record.VisibilityDeadlineUTC = record.SentAt.Add(time.Duration(delaySeconds) * time.Second)
	}
	if err := s.putMessage(record); err != nil {
		return internal(err)
	}
	if s.lambda != nil {
		receiptHandle := record.ReceiptHandle
		if receiptHandle == "" {
			receiptHandle = uuid.NewString()
		}
		s.lambda.DispatchSQSEvent(context.Background(), queueARN(queue.Name), record.MessageID, receiptHandle, record.Body, map[string]string{
			"ApproximateReceiveCount": "1",
			"SentTimestamp":           strconv.FormatInt(record.SentAt.UnixMilli(), 10),
		})
	}

	writeXML(w, http.StatusOK, sendMessageResponse{
		XMLNS: namespace,
		Result: sendMessageResult{
			MessageID: record.MessageID,
			MD5OfBody: record.MD5OfBody,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) receiveMessage(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}

	maxMessages := 1
	if raw := form.Get("MaxNumberOfMessages"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 10 {
			return badRequest("InvalidParameterValue", "MaxNumberOfMessages must be between 1 and 10.")
		}
		maxMessages = value
	}

	visibilityTimeout := -1
	if raw := form.Get("VisibilityTimeout"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return badRequest("InvalidParameterValue", "VisibilityTimeout must be a non-negative integer.")
		}
		visibilityTimeout = value
	}

	waitSeconds := -1
	if raw := form.Get("WaitTimeSeconds"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 20 {
			return badRequest("InvalidParameterValue", "WaitTimeSeconds must be between 0 and 20.")
		}
		waitSeconds = value
	}
	requested := requestedAttributeNames(form)
	var deadline time.Time

	for {
		s.mu.Lock()
		queue, err := s.queueFromForm(r, form)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		effectiveVisibilityTimeout := visibilityTimeout
		if effectiveVisibilityTimeout < 0 {
			effectiveVisibilityTimeout = attributeInt(queue.Attributes, "VisibilityTimeout", 30)
		}
		if waitSeconds < 0 {
			waitSeconds = attributeInt(queue.Attributes, "ReceiveMessageWaitTimeSeconds", 0)
		}
		result, err := s.receiveVisibleMessages(queue, maxMessages, effectiveVisibilityTimeout, requested)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if len(result) > 0 || waitSeconds == 0 {
			writeXML(w, http.StatusOK, receiveMessageResponse{
				XMLNS: namespace,
				Result: receiveMessageResult{
					Messages: result,
				},
				ResponseMetadata: responseMetadata{RequestID: requestID},
			})
			return nil
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(time.Duration(waitSeconds) * time.Second)
		}
		if time.Now().After(deadline) {
			writeXML(w, http.StatusOK, receiveMessageResponse{
				XMLNS:            namespace,
				Result:           receiveMessageResult{Messages: []receivedMessage{}},
				ResponseMetadata: responseMetadata{RequestID: requestID},
			})
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Service) changeMessageVisibility(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	receiptHandle := form.Get("ReceiptHandle")
	if receiptHandle == "" {
		return badRequest("MissingParameter", "The request must contain the parameter ReceiptHandle.")
	}
	timeoutRaw := form.Get("VisibilityTimeout")
	if timeoutRaw == "" {
		return badRequest("MissingParameter", "The request must contain the parameter VisibilityTimeout.")
	}
	timeout, err := strconv.Atoi(timeoutRaw)
	if err != nil || timeout < 0 {
		return badRequest("InvalidParameterValue", "VisibilityTimeout must be a non-negative integer.")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return err
	}
	records, err := s.loadMessages(queue.Name)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, record := range records {
		if record.ReceiptHandle != receiptHandle {
			continue
		}
		record.VisibilityDeadlineUTC = now.Add(time.Duration(timeout) * time.Second)
		if err := s.putMessage(record); err != nil {
			return internal(err)
		}
		writeXML(w, http.StatusOK, changeMessageVisibilityResponse{
			XMLNS:            namespace,
			ResponseMetadata: responseMetadata{RequestID: requestID},
		})
		return nil
	}
	return badRequest("ReceiptHandleIsInvalid", "The input receipt handle is invalid.")
}

func (s *Service) deleteMessage(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return err
	}
	receiptHandle := form.Get("ReceiptHandle")
	if receiptHandle == "" {
		return badRequest("MissingParameter", "The request must contain the parameter ReceiptHandle.")
	}

	records, err := s.loadMessages(queue.Name)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.ReceiptHandle == receiptHandle {
			if err := s.metadata.Delete(messagesBucket, messageKey(queue.Name, record.MessageID)); err != nil {
				return internal(err)
			}
			writeXML(w, http.StatusOK, deleteMessageResponse{
				XMLNS:            namespace,
				ResponseMetadata: responseMetadata{RequestID: requestID},
			})
			return nil
		}
	}

	return badRequest("ReceiptHandleIsInvalid", "The input receipt handle is invalid.")
}

func (s *Service) deleteQueue(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return err
	}

	if err := s.metadata.Delete(queuesBucket, queue.Name); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(messagesBucket, queue.Name+"|"); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, deleteQueueResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) queueFromForm(r *http.Request, form url.Values) (queueRecord, error) {
	name, err := queueNameFromForm(r, form)
	if err != nil {
		return queueRecord{}, err
	}
	return s.loadQueue(name)
}

func (s *Service) receiveVisibleMessages(queue queueRecord, maxMessages, visibilityTimeout int, requested []string) ([]receivedMessage, error) {
	records, err := s.loadMessages(queue.Name)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	redrive := parseRedrivePolicy(queue.Attributes["RedrivePolicy"])

	sort.Slice(records, func(i, j int) bool {
		return records[i].SentAt.Before(records[j].SentAt)
	})

	result := make([]receivedMessage, 0, maxMessages)
	for _, record := range records {
		if len(result) == maxMessages {
			break
		}
		if !record.VisibilityDeadlineUTC.IsZero() && record.VisibilityDeadlineUTC.After(now) {
			continue
		}

		record.ReceiveCount++
		if redrive.Enabled && record.ReceiveCount > redrive.MaxReceiveCount {
			if err := s.moveMessageToDLQ(record, redrive.TargetQueueName); err != nil {
				return nil, err
			}
			continue
		}
		if record.FirstReceivedAt.IsZero() {
			record.FirstReceivedAt = now
		}
		record.ReceiptHandle = uuid.NewString()
		record.VisibilityDeadlineUTC = now.Add(time.Duration(visibilityTimeout) * time.Second)
		if err := s.putMessage(record); err != nil {
			return nil, internal(err)
		}
		result = append(result, receivedMessage{
			MessageID:     record.MessageID,
			ReceiptHandle: record.ReceiptHandle,
			MD5OfBody:     record.MD5OfBody,
			Body:          record.Body,
			Attributes:    messageAttributes(record, requested),
		})
	}
	return result, nil
}

func (s *Service) queueAttributes(r *http.Request, queue queueRecord, requested []string) ([]attributeEntry, error) {
	all, err := s.loadMessages(queue.Name)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()

	visible := 0
	notVisible := 0
	delayed := 0
	for _, message := range all {
		if message.VisibilityDeadlineUTC.IsZero() || !message.VisibilityDeadlineUTC.After(now) {
			visible++
			continue
		}
		notVisible++
		if message.ReceiveCount == 0 {
			delayed++
		}
	}

	attrs := map[string]string{
		"CreatedTimestamp":                      strconv.FormatInt(queue.CreatedAt.Unix(), 10),
		"ApproximateNumberOfMessages":           strconv.Itoa(visible),
		"ApproximateNumberOfMessagesNotVisible": strconv.Itoa(notVisible),
		"ApproximateNumberOfMessagesDelayed":    strconv.Itoa(delayed),
		"QueueArn":                              fmt.Sprintf("arn:aws:sqs:us-east-1:%s:%s", accountID, queue.Name),
	}
	for k, v := range queue.Attributes {
		attrs[k] = v
	}

	var names []string
	if len(requested) == 0 || contains(requested, "All") {
		for name := range attrs {
			names = append(names, name)
		}
		sort.Strings(names)
	} else {
		names = requested
	}

	out := make([]attributeEntry, 0, len(names))
	for _, name := range names {
		if value, ok := attrs[name]; ok {
			out = append(out, attributeEntry{Name: name, Value: value})
		}
	}
	return out, nil
}

func (s *Service) loadQueue(name string) (queueRecord, error) {
	raw, err := s.metadata.Get(queuesBucket, name)
	if err != nil {
		return queueRecord{}, internal(err)
	}
	if raw == nil {
		return queueRecord{}, badRequest("AWS.SimpleQueueService.NonExistentQueue", "The specified queue does not exist.")
	}
	var record queueRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return queueRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putQueue(record queueRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(queuesBucket, record.Name, raw)
}

func (s *Service) loadMessages(queueName string) ([]messageRecord, error) {
	var out []messageRecord
	if err := s.metadata.Scan(messagesBucket, queueName+"|", func(_, v []byte) error {
		var record messageRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		out = append(out, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	return out, nil
}

func (s *Service) putMessage(record messageRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(messagesBucket, messageKey(record.QueueName, record.MessageID), raw)
}

func parseForm(r *http.Request) (url.Values, error) {
	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return nil, badRequest("InvalidParameterValue", "request body is not valid form data")
	}
	return form, nil
}

func requestedAttributes(form url.Values) (map[string]string, error) {
	attrs := map[string]string{}
	for idx := 1; ; idx++ {
		name := form.Get(fmt.Sprintf("Attribute.%d.Name", idx))
		if name == "" {
			break
		}
		attrs[name] = form.Get(fmt.Sprintf("Attribute.%d.Value", idx))
	}
	if hasIndexedPrefix(form, "Tag.") {
		return nil, &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "queue tags are not implemented",
		}
	}
	return attrs, nil
}

func requestedAttributeNames(form url.Values) []string {
	var names []string
	for idx := 1; ; idx++ {
		name := form.Get(fmt.Sprintf("AttributeName.%d", idx))
		if name == "" {
			break
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		if name := form.Get("AttributeName"); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func normalizeAttributes(input map[string]string) (map[string]string, error) {
	attrs := map[string]string{
		"DelaySeconds":                  "0",
		"MaximumMessageSize":            "262144",
		"MessageRetentionPeriod":        "345600",
		"ReceiveMessageWaitTimeSeconds": "0",
		"VisibilityTimeout":             "30",
	}
	for name, value := range input {
		switch name {
		case "DelaySeconds", "MaximumMessageSize", "MessageRetentionPeriod", "ReceiveMessageWaitTimeSeconds", "VisibilityTimeout":
			if _, err := strconv.Atoi(value); err != nil {
				return nil, badRequest("InvalidAttributeValue", name+" must be an integer.")
			}
			attrs[name] = value
		case "RedrivePolicy":
			if _, err := decodeRedrivePolicy(value); err != nil {
				return nil, badRequest("InvalidAttributeValue", "RedrivePolicy is not valid JSON.")
			}
			attrs[name] = value
		case "FifoQueue":
			if strings.EqualFold(value, "true") {
				return nil, &apierror.Error{
					StatusCode: http.StatusNotImplemented,
					Code:       "NotImplemented",
					Message:    "FIFO queues are not implemented",
				}
			}
		default:
			return nil, &apierror.Error{
				StatusCode: http.StatusNotImplemented,
				Code:       "NotImplemented",
				Message:    "queue attribute " + name + " is not implemented",
			}
		}
	}
	return attrs, nil
}

func queueNameFromForm(r *http.Request, form url.Values) (string, error) {
	if raw := form.Get("QueueUrl"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return "", badRequest("InvalidAddress", "QueueUrl is not valid.")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 0 || parts[len(parts)-1] == "" {
			return "", badRequest("InvalidAddress", "QueueUrl is not valid.")
		}
		return parts[len(parts)-1], nil
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-1], nil
	}
	return "", badRequest("MissingParameter", "The request must contain the parameter QueueUrl.")
}

func validateQueueName(name string) error {
	if name == "" {
		return badRequest("MissingParameter", "The request must contain the parameter QueueName.")
	}
	if strings.HasSuffix(name, ".fifo") {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "FIFO queues are not implemented",
		}
	}
	if !queueNameRe.MatchString(name) {
		return badRequest("InvalidParameterValue", "QueueName must be 1 to 80 characters of alphanumeric, hyphen, or underscore.")
	}
	return nil
}

func queueURL(r *http.Request, name string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/%s", scheme, r.Host, accountID, name)
}

func queueARN(name string) string {
	return fmt.Sprintf("arn:aws:sqs:us-east-1:%s:%s", accountID, name)
}

func queueNameFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) == 0 {
		return arn
	}
	return parts[len(parts)-1]
}

func messageKey(queueName, messageID string) string {
	return queueName + "|" + messageID
}

func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func queueAttributesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func contains(items []string, needle string) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}

func attributeInt(attrs map[string]string, name string, fallback int) int {
	if raw := attrs[name]; raw != "" {
		value, err := strconv.Atoi(raw)
		if err == nil {
			return value
		}
	}
	return fallback
}

type redrivePolicy struct {
	Enabled         bool
	MaxReceiveCount int
	TargetQueueName string
}

func parseRedrivePolicy(raw string) redrivePolicy {
	policy, err := decodeRedrivePolicy(raw)
	if err != nil {
		return redrivePolicy{}
	}
	return policy
}

func decodeRedrivePolicy(raw string) (redrivePolicy, error) {
	if raw == "" {
		return redrivePolicy{}, nil
	}
	var payload struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
		MaxReceiveCount     string `json:"maxReceiveCount"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return redrivePolicy{}, err
	}
	maxReceiveCount, err := strconv.Atoi(payload.MaxReceiveCount)
	if err != nil || maxReceiveCount < 1 {
		return redrivePolicy{}, fmt.Errorf("invalid maxReceiveCount")
	}
	parts := strings.Split(payload.DeadLetterTargetArn, ":")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return redrivePolicy{}, fmt.Errorf("invalid dead letter target arn")
	}
	return redrivePolicy{
		Enabled:         true,
		MaxReceiveCount: maxReceiveCount,
		TargetQueueName: parts[len(parts)-1],
	}, nil
}

func (s *Service) moveMessageToDLQ(record messageRecord, targetQueueName string) error {
	if _, err := s.loadQueue(targetQueueName); err != nil {
		return err
	}
	if err := s.metadata.Delete(messagesBucket, messageKey(record.QueueName, record.MessageID)); err != nil {
		return internal(err)
	}
	record.QueueName = targetQueueName
	record.ReceiveCount = 0
	record.FirstReceivedAt = time.Time{}
	record.ReceiptHandle = ""
	record.VisibilityDeadlineUTC = time.Time{}
	return s.putMessage(record)
}

func messageAttributes(record messageRecord, requested []string) []attributeEntry {
	all := []attributeEntry{
		{Name: "ApproximateReceiveCount", Value: strconv.Itoa(record.ReceiveCount)},
		{Name: "ApproximateFirstReceiveTimestamp", Value: strconv.FormatInt(record.FirstReceivedAt.UnixMilli(), 10)},
		{Name: "SentTimestamp", Value: strconv.FormatInt(record.SentAt.UnixMilli(), 10)},
	}
	if len(requested) == 0 {
		return nil
	}
	if contains(requested, "All") {
		return all
	}
	var out []attributeEntry
	for _, entry := range all {
		if contains(requested, entry.Name) {
			out = append(out, entry)
		}
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func hasIndexedPrefix(form url.Values, prefix string) bool {
	for key := range form {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
