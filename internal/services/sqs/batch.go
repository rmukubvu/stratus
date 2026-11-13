package sqs

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The three batch operations share one shape: up to ten entries, each with
// a caller-chosen Id, and a response that lists each entry as successful
// or failed. A failed entry does not fail the request; only a malformed
// batch as a whole does.

var batchEntryIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// batchEntry is one entry of a batch request. Fields a given operation
// does not use stay empty.
type batchEntry struct {
	ID                string
	MessageBody       string
	DelaySeconds      string
	ReceiptHandle     string
	VisibilityTimeout string
	// HasAttributes is true when the entry sets message attributes, which
	// Stratus does not implement.
	HasAttributes bool
}

// batchSuccess is an entry that succeeded. MessageID and MD5OfMessageBody
// are only set by SendMessageBatch.
type batchSuccess struct {
	ID               string
	MessageID        string
	MD5OfMessageBody string
}

// batchFailure is an entry that failed, in the shape of SQS's
// BatchResultErrorEntry.
type batchFailure struct {
	ID          string `xml:"Id" json:"Id"`
	SenderFault bool   `xml:"SenderFault" json:"SenderFault"`
	Code        string `xml:"Code" json:"Code"`
	Message     string `xml:"Message" json:"Message"`
}

// batchResult is the outcome of a batch, in request order.
type batchResult struct {
	Successful []batchSuccess
	Failed     []batchFailure
}

func (r *batchResult) succeed(s batchSuccess) { r.Successful = append(r.Successful, s) }

// fail records an entry as failed with the code and message of err.
func (r *batchResult) fail(id string, err error) {
	code, message := "InternalError", err.Error()
	senderFault := false
	if apiErr := asAPIError(err); apiErr != nil {
		code, message = apiErr.Code, apiErr.Message
		senderFault = apiErr.StatusCode < 500
	}
	r.Failed = append(r.Failed, batchFailure{ID: id, SenderFault: senderFault, Code: code, Message: message})
}

// batchOperation describes one batch operation: how its entries are named
// on the wire and what it does to a queue.
type batchOperation struct {
	name string
	// formPrefix names the entries in a query request, e.g.
	// SendMessageBatchRequestEntry.1.Id.
	formPrefix string
	// resultEntry is the XML element of a successful entry.
	resultEntry string
	// validate checks the request as a whole, before the queue is touched.
	validate func([]batchEntry) error
	// apply runs the entries against the queue. The service lock is held.
	apply func(*Service, queueRecord, []batchEntry) (batchResult, error)
}

var batchOperations = map[string]batchOperation{
	"SendMessageBatch": {
		name:        "SendMessageBatch",
		formPrefix:  "SendMessageBatchRequestEntry",
		resultEntry: "SendMessageBatchResultEntry",
		validate:    validateSendBatch,
		apply:       (*Service).sendMessageBatch,
	},
	"DeleteMessageBatch": {
		name:        "DeleteMessageBatch",
		formPrefix:  "DeleteMessageBatchRequestEntry",
		resultEntry: "DeleteMessageBatchResultEntry",
		apply:       (*Service).deleteMessageBatch,
	},
	"ChangeMessageVisibilityBatch": {
		name:        "ChangeMessageVisibilityBatch",
		formPrefix:  "ChangeMessageVisibilityBatchRequestEntry",
		resultEntry: "ChangeMessageVisibilityBatchResultEntry",
		apply:       (*Service).changeMessageVisibilityBatch,
	},
}

// validateBatchEntries applies the rules every batch shares: one to ten
// entries, each with a well-formed Id no other entry uses.
func validateBatchEntries(op batchOperation, entries []batchEntry) error {
	if len(entries) == 0 {
		return badRequest("AWS.SimpleQueueService.EmptyBatchRequest",
			fmt.Sprintf("There should be at least one %s in the request.", op.formPrefix))
	}
	if len(entries) > maxBatchEntries {
		return badRequest("AWS.SimpleQueueService.TooManyEntriesInBatchRequest",
			fmt.Sprintf("Maximum number of entries per request are %d. You have sent %d.", maxBatchEntries, len(entries)))
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !batchEntryIDRe.MatchString(e.ID) {
			return badRequest("AWS.SimpleQueueService.InvalidBatchEntryId",
				"A batch entry id can only contain alphanumeric characters, hyphens and underscores. It can be at most 80 letters long.")
		}
		if seen[e.ID] {
			return badRequest("AWS.SimpleQueueService.BatchEntryIdsNotDistinct", fmt.Sprintf("Id %s repeated.", e.ID))
		}
		seen[e.ID] = true
	}
	if op.validate != nil {
		return op.validate(entries)
	}
	return nil
}

// validateSendBatch caps the total size of the bodies in a
// SendMessageBatch request.
func validateSendBatch(entries []batchEntry) error {
	total := 0
	for _, e := range entries {
		if e.HasAttributes {
			return notImplemented("message attributes are not implemented")
		}
		total += len(e.MessageBody)
	}
	if total > maxPayloadBytes {
		return badRequest("AWS.SimpleQueueService.BatchRequestTooLong",
			fmt.Sprintf("Batch requests cannot be longer than %d bytes. You have sent %d bytes.", maxPayloadBytes, total))
	}
	return nil
}

func (s *Service) sendMessageBatch(queue queueRecord, entries []batchEntry) (batchResult, error) {
	var result batchResult
	for _, e := range entries {
		record, err := s.sendOne(queue, e.MessageBody, e.DelaySeconds)
		if err != nil {
			if isInternal(err) {
				return batchResult{}, err
			}
			result.fail(e.ID, err)
			continue
		}
		result.succeed(batchSuccess{ID: e.ID, MessageID: record.MessageID, MD5OfMessageBody: record.MD5OfBody})
	}
	return result, nil
}

func (s *Service) deleteMessageBatch(queue queueRecord, entries []batchEntry) (batchResult, error) {
	inflight, err := s.inflightMessages(queue.Name)
	if err != nil {
		return batchResult{}, err
	}
	var result batchResult
	for _, e := range entries {
		if err := s.deleteOne(inflight, e.ReceiptHandle); err != nil {
			if isInternal(err) {
				return batchResult{}, err
			}
			result.fail(e.ID, err)
			continue
		}
		result.succeed(batchSuccess{ID: e.ID})
	}
	return result, nil
}

func (s *Service) changeMessageVisibilityBatch(queue queueRecord, entries []batchEntry) (batchResult, error) {
	inflight, err := s.inflightMessages(queue.Name)
	if err != nil {
		return batchResult{}, err
	}
	var result batchResult
	for _, e := range entries {
		err := s.changeVisibilityOne(inflight, e.ReceiptHandle, e.VisibilityTimeout)
		if err != nil {
			if isInternal(err) {
				return batchResult{}, err
			}
			result.fail(e.ID, err)
			continue
		}
		result.succeed(batchSuccess{ID: e.ID})
	}
	return result, nil
}

// handleBatch serves a batch operation over the query or the JSON protocol.
func (s *Service) handleBatch(w http.ResponseWriter, r *http.Request, op batchOperation, requestID string) error {
	asJSON := isJSONTargetRequest(r)
	decode := decodeBatchForm
	if asJSON {
		decode = decodeBatchJSON
	}
	form, entries, err := decode(r, op)
	if err != nil {
		return err
	}
	if err := validateBatchEntries(op, entries); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return err
	}
	result, err := op.apply(s, queue, entries)
	if err != nil {
		return err
	}
	if asJSON {
		writeJSON(w, http.StatusOK, batchJSON(result))
	} else {
		writeXML(w, http.StatusOK, batchXML(op, result, requestID))
	}
	return nil
}

// decodeBatchForm reads a query-protocol batch request. It returns the
// form, so the queue can be resolved from QueueUrl or the request path.
func decodeBatchForm(r *http.Request, op batchOperation) (url.Values, []batchEntry, error) {
	form, err := parseForm(r)
	if err != nil {
		return nil, nil, err
	}
	prefix := op.formPrefix + "."
	indexes := map[int]bool{}
	for key := range form {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		raw, _, _ := strings.Cut(rest, ".")
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			indexes[n] = true
		}
	}
	ordered := make([]int, 0, len(indexes))
	for n := range indexes {
		ordered = append(ordered, n)
	}
	sort.Ints(ordered)

	entries := make([]batchEntry, 0, len(ordered))
	for _, n := range ordered {
		field := func(name string) string { return form.Get(fmt.Sprintf("%s%d.%s", prefix, n, name)) }
		entries = append(entries, batchEntry{
			ID:                field("Id"),
			MessageBody:       field("MessageBody"),
			DelaySeconds:      field("DelaySeconds"),
			ReceiptHandle:     field("ReceiptHandle"),
			VisibilityTimeout: field("VisibilityTimeout"),
			HasAttributes:     hasIndexedPrefix(form, fmt.Sprintf("%s%d.MessageAttribute.", prefix, n)),
		})
	}
	return form, entries, nil
}

// decodeBatchJSON reads a JSON-protocol batch request.
func decodeBatchJSON(r *http.Request, _ batchOperation) (url.Values, []batchEntry, error) {
	var payload struct {
		QueueURL string           `json:"QueueUrl"`
		Entries  []map[string]any `json:"Entries"`
	}
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
			return nil, nil, badRequest("InvalidParameterValue", "request body is not valid JSON")
		}
	}
	form := url.Values{}
	if payload.QueueURL != "" {
		form.Set("QueueUrl", payload.QueueURL)
	}
	entries := make([]batchEntry, 0, len(payload.Entries))
	for _, raw := range payload.Entries {
		_, hasAttributes := raw["MessageAttributes"]
		entries = append(entries, batchEntry{
			ID:                scalarString(raw["Id"]),
			MessageBody:       scalarString(raw["MessageBody"]),
			DelaySeconds:      scalarString(raw["DelaySeconds"]),
			ReceiptHandle:     scalarString(raw["ReceiptHandle"]),
			VisibilityTimeout: scalarString(raw["VisibilityTimeout"]),
			HasAttributes:     hasAttributes,
		})
	}
	return form, entries, nil
}

type batchSuccessJSON struct {
	ID               string `json:"Id"`
	MessageID        string `json:"MessageId,omitempty"`
	MD5OfMessageBody string `json:"MD5OfMessageBody,omitempty"`
}

func batchJSON(result batchResult) map[string]any {
	successful := make([]batchSuccessJSON, 0, len(result.Successful))
	for _, s := range result.Successful {
		successful = append(successful, batchSuccessJSON(s))
	}
	failed := result.Failed
	if failed == nil {
		failed = []batchFailure{}
	}
	return map[string]any{"Successful": successful, "Failed": failed}
}

// The XML element names differ per operation, so they are set at run time
// through XMLName.
type batchResponseXML struct {
	XMLName          xml.Name
	XMLNS            string `xml:"xmlns,attr"`
	Result           batchResultXML
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type batchResultXML struct {
	XMLName    xml.Name
	Successful []batchSuccessXML
	Failed     []batchFailure `xml:"BatchResultErrorEntry"`
}

type batchSuccessXML struct {
	XMLName          xml.Name
	ID               string `xml:"Id"`
	MessageID        string `xml:"MessageId,omitempty"`
	MD5OfMessageBody string `xml:"MD5OfMessageBody,omitempty"`
}

func batchXML(op batchOperation, result batchResult, requestID string) batchResponseXML {
	successful := make([]batchSuccessXML, 0, len(result.Successful))
	for _, s := range result.Successful {
		successful = append(successful, batchSuccessXML{
			XMLName: xml.Name{Local: op.resultEntry}, ID: s.ID, MessageID: s.MessageID, MD5OfMessageBody: s.MD5OfMessageBody,
		})
	}
	return batchResponseXML{
		XMLName: xml.Name{Local: op.name + "Response"},
		XMLNS:   namespace,
		Result: batchResultXML{
			XMLName:    xml.Name{Local: op.name + "Result"},
			Successful: successful,
			Failed:     result.Failed,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}
}
