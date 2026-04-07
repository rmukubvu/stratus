package sqs

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
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

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateQueue":
		return s.createQueue(w, r, requestID)
	case "GetQueueUrl":
		return s.getQueueURL(w, r, requestID)
	case "ListQueues":
		return s.listQueues(w, r, requestID)
	case "GetQueueAttributes":
		return s.getQueueAttributes(w, r, requestID)
	case "SendMessage":
		return s.sendMessage(w, r, requestID)
	case "ReceiveMessage":
		return s.receiveMessage(w, r, requestID)
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

func (s *Service) sendMessage(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
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
	queue, err := s.queueFromForm(r, form)
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

	visibilityTimeout := attributeInt(queue.Attributes, "VisibilityTimeout", 30)
	if raw := form.Get("VisibilityTimeout"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return badRequest("InvalidParameterValue", "VisibilityTimeout must be a non-negative integer.")
		}
		visibilityTimeout = value
	}

	records, err := s.loadMessages(queue.Name)
	if err != nil {
		return err
	}
	now := s.now().UTC()

	sort.Slice(records, func(i, j int) bool {
		return records[i].SentAt.Before(records[j].SentAt)
	})

	var result []receivedMessage
	for _, record := range records {
		if len(result) == maxMessages {
			break
		}
		if !record.VisibilityDeadlineUTC.IsZero() && record.VisibilityDeadlineUTC.After(now) {
			continue
		}
		record.ReceiveCount++
		if record.FirstReceivedAt.IsZero() {
			record.FirstReceivedAt = now
		}
		record.ReceiptHandle = uuid.NewString()
		record.VisibilityDeadlineUTC = now.Add(time.Duration(visibilityTimeout) * time.Second)
		if err := s.putMessage(record); err != nil {
			return internal(err)
		}

		result = append(result, receivedMessage{
			MessageID:     record.MessageID,
			ReceiptHandle: record.ReceiptHandle,
			MD5OfBody:     record.MD5OfBody,
			Body:          record.Body,
			Attributes: []attributeEntry{
				{Name: "ApproximateReceiveCount", Value: strconv.Itoa(record.ReceiveCount)},
				{Name: "ApproximateFirstReceiveTimestamp", Value: strconv.FormatInt(record.FirstReceivedAt.UnixMilli(), 10)},
				{Name: "SentTimestamp", Value: strconv.FormatInt(record.SentAt.UnixMilli(), 10)},
			},
		})
	}

	writeXML(w, http.StatusOK, receiveMessageResponse{
		XMLNS: namespace,
		Result: receiveMessageResult{
			Messages: result,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) deleteMessage(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
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

func (s *Service) queueAttributes(r *http.Request, queue queueRecord, requested []string) ([]attributeEntry, error) {
	all, err := s.loadMessages(queue.Name)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()

	visible := 0
	notVisible := 0
	for _, message := range all {
		if message.VisibilityDeadlineUTC.IsZero() || !message.VisibilityDeadlineUTC.After(now) {
			visible++
			continue
		}
		notVisible++
	}

	attrs := map[string]string{
		"CreatedTimestamp":                      strconv.FormatInt(queue.CreatedAt.Unix(), 10),
		"ApproximateNumberOfMessages":           strconv.Itoa(visible),
		"ApproximateNumberOfMessagesNotVisible": strconv.Itoa(notVisible),
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
	if err := r.ParseForm(); err != nil {
		return nil, badRequest("InvalidParameterValue", "request body is not valid form data")
	}
	return r.Form, nil
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
		"MessageRetentionPeriod":        "345600",
		"ReceiveMessageWaitTimeSeconds": "0",
		"VisibilityTimeout":             "30",
	}
	for name, value := range input {
		switch name {
		case "DelaySeconds", "MessageRetentionPeriod", "ReceiveMessageWaitTimeSeconds", "VisibilityTimeout":
			if _, err := strconv.Atoi(value); err != nil {
				return nil, badRequest("InvalidAttributeValue", name+" must be an integer.")
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
