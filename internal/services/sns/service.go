package sns

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/store"
)

const (
	namespace           = "http://sns.amazonaws.com/doc/2010-03-31/"
	topicsBucket        = "sns-topics"
	subscriptionsBucket = "sns-subscriptions"
	accountID           = "000000000000"
	region              = "us-east-1"
)

var topicNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}(\.fifo)?$`)

type Service struct {
	metadata     store.Store
	queueTarget  QueuePublisher
	lambdaTarget LambdaInvoker
	now          func() time.Time
	mu           sync.Mutex
}

type QueuePublisher interface {
	SendMessageToARN(queueARN, body string) error
}

type LambdaInvoker interface {
	InvokeAsyncByName(ctx context.Context, name string, payload []byte) error
}

type CreateTopicInput struct {
	Attributes map[string]string
	Name       string
}

type topicRecord struct {
	Attributes map[string]string `json:"attributes,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Name       string            `json:"name"`
	TopicArn   string            `json:"topic_arn"`
}

type subscriptionRecord struct {
	Attributes      map[string]string `json:"attributes,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	Endpoint        string            `json:"endpoint"`
	Protocol        string            `json:"protocol"`
	SubscriptionArn string            `json:"subscription_arn"`
	TopicArn        string            `json:"topic_arn"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

type topicArnResult struct {
	TopicArn string `xml:"TopicArn"`
}

type createTopicResponse struct {
	XMLName          xml.Name         `xml:"CreateTopicResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           topicArnResult   `xml:"CreateTopicResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type deleteTopicResponse struct {
	XMLName          xml.Name         `xml:"DeleteTopicResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type listTopicsResponse struct {
	XMLName          xml.Name         `xml:"ListTopicsResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           listTopicsResult `xml:"ListTopicsResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type listTopicsResult struct {
	Topics []topicMember `xml:"Topics>member"`
}

type topicMember struct {
	TopicArn string `xml:"TopicArn"`
}

type publishResponse struct {
	XMLName          xml.Name         `xml:"PublishResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           publishResult    `xml:"PublishResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type publishResult struct {
	MessageID string `xml:"MessageId"`
}

type getTopicAttributesResponse struct {
	XMLName          xml.Name                 `xml:"GetTopicAttributesResponse"`
	XMLNS            string                   `xml:"xmlns,attr"`
	Result           getTopicAttributesResult `xml:"GetTopicAttributesResult"`
	ResponseMetadata responseMetadata         `xml:"ResponseMetadata"`
}

type getTopicAttributesResult struct {
	Attributes []attributeMember `xml:"Attributes>entry"`
}

type attributeMember struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

type setTopicAttributesResponse struct {
	XMLName          xml.Name         `xml:"SetTopicAttributesResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) SetQueuePublisher(target QueuePublisher) {
	s.queueTarget = target
}

func (s *Service) SetLambdaInvoker(target LambdaInvoker) {
	s.lambdaTarget = target
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateTopic":
		return s.createTopic(w, r, requestID)
	case "ListTopics":
		return s.listTopics(w, requestID)
	case "GetTopicAttributes":
		return s.getTopicAttributes(w, r, requestID)
	case "SetTopicAttributes":
		return s.setTopicAttributes(w, r, requestID)
	case "Publish":
		return s.publish(w, r, requestID)
	case "Subscribe":
		return s.subscribe(w, r, requestID)
	case "ListSubscriptions":
		return s.listSubscriptions(w, requestID)
	case "ListSubscriptionsByTopic":
		return s.listSubscriptionsByTopic(w, r, requestID)
	case "GetSubscriptionAttributes":
		return s.getSubscriptionAttributes(w, r, requestID)
	case "SetSubscriptionAttributes":
		return s.setSubscriptionAttributes(w, r, requestID)
	case "Unsubscribe":
		return s.unsubscribe(w, r, requestID)
	case "DeleteTopic":
		return s.deleteTopic(w, r, requestID)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "sns operation is not implemented",
		}
	}
}

func (s *Service) createTopic(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	name := form.Get("Name")
	if err := validateTopicName(name); err != nil {
		return err
	}
	attrs := parseAttributeEntries(form)
	if strings.HasSuffix(name, ".fifo") || strings.EqualFold(attrs["FifoTopic"], "true") {
		return notImplemented("FIFO topics are not supported")
	}
	if raw := form.Get("Tags.member.1.Key"); raw != "" {
		return notImplemented("SNS topic tags are not supported")
	}

	if topic, err := s.loadTopicByName(name); err == nil {
		writeXML(w, http.StatusOK, createTopicResponse{
			XMLNS: namespace,
			Result: topicArnResult{
				TopicArn: topic.TopicArn,
			},
			ResponseMetadata: responseMetadata{RequestID: requestID},
		})
		return nil
	}

	record := topicRecord{
		Attributes: normalizeAttributes(attrs),
		CreatedAt:  s.now().UTC(),
		Name:       name,
		TopicArn:   topicARN(name),
	}
	if err := s.putTopic(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, createTopicResponse{
		XMLNS: namespace,
		Result: topicArnResult{
			TopicArn: record.TopicArn,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) CreateTopic(input CreateTopicInput) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateTopicName(input.Name); err != nil {
		return "", err
	}
	if strings.HasSuffix(input.Name, ".fifo") || strings.EqualFold(input.Attributes["FifoTopic"], "true") {
		return "", notImplemented("FIFO topics are not supported")
	}
	if topic, err := s.loadTopicByName(input.Name); err == nil {
		return topic.TopicArn, nil
	}
	record := topicRecord{
		Attributes: normalizeAttributes(input.Attributes),
		CreatedAt:  s.now().UTC(),
		Name:       input.Name,
		TopicArn:   topicARN(input.Name),
	}
	if err := s.putTopic(record); err != nil {
		return "", internal(err)
	}
	return record.TopicArn, nil
}

func (s *Service) DeleteTopic(topicARN string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, err := s.loadTopicByARN(topicARN)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(topicsBucket, record.Name); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) PublishToTopic(topicARN, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if message == "" {
		return badRequest("InvalidParameter", "Message is required")
	}
	if _, err := s.loadTopicByARN(topicARN); err != nil {
		return err
	}
	return s.deliverTopicMessage(topicARN, message, nil)
}

func (s *Service) subscribe(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	topic, err := s.loadTopicByARN(form.Get("TopicArn"))
	if err != nil {
		return err
	}
	protocol := form.Get("Protocol")
	endpoint := form.Get("Endpoint")
	if endpoint == "" || protocol == "" {
		return badRequest("InvalidParameter", "Protocol and Endpoint are required")
	}
	if protocol != "sqs" && protocol != "lambda" {
		return notImplemented("only sqs and lambda subscriptions are supported")
	}
	attributes := map[string]string{
		"RawMessageDelivery": "false",
	}
	if filter := form.Get("Attributes.entry.1.value"); form.Get("Attributes.entry.1.key") == "FilterPolicy" && filter != "" {
		attributes["FilterPolicy"] = filter
	}
	record := subscriptionRecord{
		Attributes:      attributes,
		CreatedAt:       s.now().UTC(),
		Endpoint:        endpoint,
		Protocol:        protocol,
		SubscriptionArn: subscriptionARN(),
		TopicArn:        topic.TopicArn,
	}
	if err := s.putSubscription(record); err != nil {
		return internal(err)
	}
	type subscribeResponse struct {
		XMLName xml.Name `xml:"SubscribeResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			SubscriptionArn string `xml:"SubscriptionArn"`
		} `xml:"SubscribeResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	writeXML(w, http.StatusOK, subscribeResponse{
		XMLNS: namespace,
		Result: struct {
			SubscriptionArn string `xml:"SubscriptionArn"`
		}{SubscriptionArn: record.SubscriptionArn},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) listSubscriptions(w http.ResponseWriter, requestID string) error {
	items, err := s.loadSubscriptions("")
	if err != nil {
		return err
	}
	type subscriptionMember struct {
		Endpoint        string `xml:"Endpoint"`
		Protocol        string `xml:"Protocol"`
		SubscriptionArn string `xml:"SubscriptionArn"`
		TopicArn        string `xml:"TopicArn"`
	}
	type listSubscriptionsResponse struct {
		XMLName xml.Name `xml:"ListSubscriptionsResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			Subscriptions []subscriptionMember `xml:"Subscriptions>member"`
		} `xml:"ListSubscriptionsResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	payload := listSubscriptionsResponse{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, item := range items {
		payload.Result.Subscriptions = append(payload.Result.Subscriptions, subscriptionMember{
			Endpoint:        item.Endpoint,
			Protocol:        item.Protocol,
			SubscriptionArn: item.SubscriptionArn,
			TopicArn:        item.TopicArn,
		})
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) listSubscriptionsByTopic(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	topicArn := form.Get("TopicArn")
	if _, err := s.loadTopicByARN(topicArn); err != nil {
		return err
	}
	items, err := s.loadSubscriptions(topicArn)
	if err != nil {
		return err
	}
	type subscriptionMember struct {
		Endpoint        string `xml:"Endpoint"`
		Protocol        string `xml:"Protocol"`
		SubscriptionArn string `xml:"SubscriptionArn"`
		TopicArn        string `xml:"TopicArn"`
	}
	type listSubscriptionsResponse struct {
		XMLName xml.Name `xml:"ListSubscriptionsByTopicResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			Subscriptions []subscriptionMember `xml:"Subscriptions>member"`
		} `xml:"ListSubscriptionsByTopicResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	payload := listSubscriptionsResponse{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, item := range items {
		payload.Result.Subscriptions = append(payload.Result.Subscriptions, subscriptionMember{
			Endpoint:        item.Endpoint,
			Protocol:        item.Protocol,
			SubscriptionArn: item.SubscriptionArn,
			TopicArn:        item.TopicArn,
		})
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) getSubscriptionAttributes(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadSubscription(form.Get("SubscriptionArn"))
	if err != nil {
		return err
	}
	attrs := []attributeMember{
		{Key: "Endpoint", Value: record.Endpoint},
		{Key: "Protocol", Value: record.Protocol},
		{Key: "SubscriptionArn", Value: record.SubscriptionArn},
		{Key: "TopicArn", Value: record.TopicArn},
	}
	for _, key := range sortedAttributeKeys(record.Attributes) {
		attrs = append(attrs, attributeMember{Key: key, Value: record.Attributes[key]})
	}
	type getSubscriptionAttributesResponse struct {
		XMLName xml.Name `xml:"GetSubscriptionAttributesResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			Attributes []attributeMember `xml:"Attributes>entry"`
		} `xml:"GetSubscriptionAttributesResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	writeXML(w, http.StatusOK, getSubscriptionAttributesResponse{
		XMLNS: namespace,
		Result: struct {
			Attributes []attributeMember `xml:"Attributes>entry"`
		}{Attributes: attrs},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) setSubscriptionAttributes(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadSubscription(form.Get("SubscriptionArn"))
	if err != nil {
		return err
	}
	name := form.Get("AttributeName")
	if name == "" {
		return badRequest("InvalidParameter", "AttributeName is required")
	}
	if record.Attributes == nil {
		record.Attributes = map[string]string{}
	}
	record.Attributes[name] = form.Get("AttributeValue")
	if err := s.putSubscription(record); err != nil {
		return internal(err)
	}
	type response struct {
		XMLName          xml.Name         `xml:"SetSubscriptionAttributesResponse"`
		XMLNS            string           `xml:"xmlns,attr"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	writeXML(w, http.StatusOK, response{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}})
	return nil
}

func (s *Service) unsubscribe(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadSubscription(form.Get("SubscriptionArn"))
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(subscriptionsBucket, record.SubscriptionArn); err != nil {
		return internal(err)
	}
	type response struct {
		XMLName          xml.Name         `xml:"UnsubscribeResponse"`
		XMLNS            string           `xml:"xmlns,attr"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	writeXML(w, http.StatusOK, response{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}})
	return nil
}

func (s *Service) listTopics(w http.ResponseWriter, requestID string) error {
	var topics []topicMember
	if err := s.metadata.Scan(topicsBucket, "", func(_, v []byte) error {
		var record topicRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		topics = append(topics, topicMember{TopicArn: record.TopicArn})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].TopicArn < topics[j].TopicArn })

	writeXML(w, http.StatusOK, listTopicsResponse{
		XMLNS: namespace,
		Result: listTopicsResult{
			Topics: topics,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) getTopicAttributes(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadTopicByARN(form.Get("TopicArn"))
	if err != nil {
		return err
	}

	attrs := []attributeMember{
		{Key: "DisplayName", Value: record.Attributes["DisplayName"]},
		{Key: "Owner", Value: accountID},
		{Key: "TopicArn", Value: record.TopicArn},
	}
	for _, key := range sortedAttributeKeys(record.Attributes) {
		if key == "DisplayName" {
			continue
		}
		attrs = append(attrs, attributeMember{Key: key, Value: record.Attributes[key]})
	}

	writeXML(w, http.StatusOK, getTopicAttributesResponse{
		XMLNS: namespace,
		Result: getTopicAttributesResult{
			Attributes: attrs,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) setTopicAttributes(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadTopicByARN(form.Get("TopicArn"))
	if err != nil {
		return err
	}
	name := form.Get("AttributeName")
	if name == "" {
		return badRequest("InvalidParameter", "AttributeName is required")
	}
	if name == "FifoTopic" || name == "ContentBasedDeduplication" {
		return notImplemented("FIFO topic attributes are not supported")
	}
	if record.Attributes == nil {
		record.Attributes = map[string]string{}
	}
	record.Attributes[name] = form.Get("AttributeValue")
	if err := s.putTopic(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, setTopicAttributesResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) publish(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	if form.Get("TargetArn") != "" || form.Get("PhoneNumber") != "" {
		return notImplemented("only direct TopicArn publish is supported")
	}
	record, err := s.loadTopicByARN(form.Get("TopicArn"))
	if err != nil {
		return err
	}
	if form.Get("Message") == "" {
		return badRequest("InvalidParameter", "Message is required")
	}
	attrs := parseMessageAttributeEntries(form)
	if err := s.deliverTopicMessage(record.TopicArn, form.Get("Message"), attrs); err != nil {
		return err
	}

	writeXML(w, http.StatusOK, publishResponse{
		XMLNS: namespace,
		Result: publishResult{
			MessageID: uuid.NewString(),
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) deleteTopic(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadTopicByARN(form.Get("TopicArn"))
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(topicsBucket, record.Name); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, deleteTopicResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) deliverTopicMessage(topicARN, message string, attrs map[string]string) error {
	subs, err := s.loadSubscriptions(topicARN)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !subscriptionMatches(sub, attrs) {
			continue
		}
		payload := message
		if strings.EqualFold(sub.Attributes["RawMessageDelivery"], "true") {
			payload = message
		} else {
			body, err := json.Marshal(map[string]any{
				"Message":          message,
				"MessageId":        uuid.NewString(),
				"SignatureVersion": "1",
				"Timestamp":        s.now().UTC().Format(time.RFC3339),
				"TopicArn":         topicARN,
				"Type":             "Notification",
			})
			if err != nil {
				return internal(err)
			}
			payload = string(body)
		}
		switch sub.Protocol {
		case "sqs":
			if s.queueTarget != nil {
				if err := s.queueTarget.SendMessageToARN(sub.Endpoint, payload); err != nil {
					return err
				}
			}
		case "lambda":
			if s.lambdaTarget != nil {
				name, _ := lambdaNameFromARN(sub.Endpoint)
				if err := s.lambdaTarget.InvokeAsyncByName(context.Background(), name, []byte(message)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) loadTopicByName(name string) (topicRecord, error) {
	raw, err := s.metadata.Get(topicsBucket, name)
	if err != nil {
		return topicRecord{}, internal(err)
	}
	if raw == nil {
		return topicRecord{}, notFound(name)
	}
	var record topicRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return topicRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadSubscription(arn string) (subscriptionRecord, error) {
	raw, err := s.metadata.Get(subscriptionsBucket, arn)
	if err != nil {
		return subscriptionRecord{}, internal(err)
	}
	if raw == nil {
		return subscriptionRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFound", Message: "Subscription does not exist"}
	}
	var record subscriptionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return subscriptionRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadSubscriptions(topicARN string) ([]subscriptionRecord, error) {
	items := make([]subscriptionRecord, 0)
	if err := s.metadata.Scan(subscriptionsBucket, "", func(_, v []byte) error {
		var record subscriptionRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if topicARN != "" && record.TopicArn != topicARN {
			return nil
		}
		items = append(items, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SubscriptionArn < items[j].SubscriptionArn })
	return items, nil
}

func (s *Service) putSubscription(record subscriptionRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(subscriptionsBucket, record.SubscriptionArn, raw)
}

func (s *Service) loadTopicByARN(topicARN string) (topicRecord, error) {
	if topicARN == "" {
		return topicRecord{}, badRequest("InvalidParameter", "TopicArn is required")
	}
	parts := strings.Split(topicARN, ":")
	if len(parts) < 6 {
		return topicRecord{}, notFound(topicARN)
	}
	return s.loadTopicByName(parts[len(parts)-1])
}

func (s *Service) putTopic(record topicRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(topicsBucket, record.Name, raw)
}

func topicARN(name string) string {
	return fmt.Sprintf("arn:aws:sns:%s:%s:%s", region, accountID, name)
}

func validateTopicName(name string) error {
	if !topicNameRe.MatchString(name) {
		return badRequest("InvalidParameter", "Topic Name is invalid")
	}
	return nil
}

func parseForm(r *http.Request) (url.Values, error) {
	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return nil, badRequest("InvalidParameter", "request body is not valid form data")
	}
	return form, nil
}

func parseAttributeEntries(form url.Values) map[string]string {
	attrs := map[string]string{}
	for key, values := range form {
		if !strings.HasPrefix(key, "Attributes.entry.") || !strings.HasSuffix(key, ".key") || len(values) == 0 {
			continue
		}
		prefix := strings.TrimSuffix(key, ".key")
		valueKey := prefix + ".value"
		if len(form[valueKey]) == 0 {
			continue
		}
		attrs[values[0]] = form[valueKey][0]
	}
	return attrs
}

func parseMessageAttributeEntries(form url.Values) map[string]string {
	attrs := map[string]string{}
	for idx := 1; ; idx++ {
		name := form.Get(fmt.Sprintf("MessageAttributes.entry.%d.Name", idx))
		if name == "" {
			break
		}
		value := form.Get(fmt.Sprintf("MessageAttributes.entry.%d.Value.StringValue", idx))
		attrs[name] = value
	}
	return attrs
}

func normalizeAttributes(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortedAttributeKeys(attrs map[string]string) []string {
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func subscriptionMatches(record subscriptionRecord, attrs map[string]string) bool {
	filter := record.Attributes["FilterPolicy"]
	if filter == "" {
		return true
	}
	var policy map[string][]string
	if err := json.Unmarshal([]byte(filter), &policy); err != nil {
		return true
	}
	for key, expected := range policy {
		value := attrs[key]
		matched := false
		for _, candidate := range expected {
			if value == candidate {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func lambdaNameFromARN(arn string) (string, bool) {
	parts := strings.Split(arn, ":function:")
	if len(parts) != 2 {
		return "", false
	}
	name := parts[1]
	if idx := strings.Index(name, ":"); idx >= 0 {
		name = name[:idx]
	}
	return name, true
}

func subscriptionARN() string {
	return fmt.Sprintf("arn:aws:sns:%s:%s:%s", region, accountID, uuid.NewString())
}

func notFound(topic string) error {
	return &apierror.Error{
		StatusCode: http.StatusNotFound,
		Code:       "NotFound",
		Message:    fmt.Sprintf("Topic does not exist (%s)", topic),
	}
}

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalError", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}
