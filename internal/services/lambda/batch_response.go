package lambda

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ReportBatchItemFailures is the FunctionResponseTypes value that lets a
// function fail some messages of a batch rather than all of them.
const ReportBatchItemFailures = "ReportBatchItemFailures"

// settleSQSBatch decides which messages of a batch the poller deletes after
// the function returned without an error. Without ReportBatchItemFailures
// a successful invocation settles every message. With it, the function's
// response names the messages to keep, following the rules AWS documents
// for partial batch responses:
//
//   - no response, null, an empty object or an empty list: all succeeded;
//   - a response that is not a JSON object, an itemIdentifier that is
//     missing, empty or not a message in the batch: the whole batch failed.
//
// It returns the receipt handles to delete, or an error when the whole
// batch must go back to the queue.
func settleSQSBatch(batch []QueueMessage, payload []byte, reportItemFailures bool) ([]string, error) {
	if !reportItemFailures {
		return receiptHandles(batch), nil
	}
	failed, err := batchItemFailures(payload)
	if err != nil {
		return nil, err
	}
	inBatch := make(map[string]bool, len(batch))
	for _, message := range batch {
		inBatch[message.MessageID] = true
	}
	keep := make(map[string]bool, len(failed))
	for _, id := range failed {
		if !inBatch[id] {
			return nil, fmt.Errorf("batchItemFailures names %q, which is not a message in the batch", id)
		}
		keep[id] = true
	}
	handles := make([]string, 0, len(batch))
	for _, message := range batch {
		if !keep[message.MessageID] {
			handles = append(handles, message.ReceiptHandle)
		}
	}
	return handles, nil
}

// batchItemFailures reads the message IDs a partial batch response lists.
func batchItemFailures(payload []byte) ([]string, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var response struct {
		BatchItemFailures []map[string]json.RawMessage `json:"batchItemFailures"`
	}
	if err := json.Unmarshal(trimmed, &response); err != nil {
		return nil, fmt.Errorf("partial batch response is not a JSON object: %w", err)
	}
	ids := make([]string, 0, len(response.BatchItemFailures))
	for _, item := range response.BatchItemFailures {
		raw, ok := item["itemIdentifier"]
		if !ok {
			return nil, fmt.Errorf("a batchItemFailures entry has no itemIdentifier")
		}
		var id string
		if err := json.Unmarshal(raw, &id); err != nil || id == "" {
			return nil, fmt.Errorf("a batchItemFailures entry has an empty or invalid itemIdentifier")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func receiptHandles(batch []QueueMessage) []string {
	out := make([]string, 0, len(batch))
	for _, message := range batch {
		out = append(out, message.ReceiptHandle)
	}
	return out
}
