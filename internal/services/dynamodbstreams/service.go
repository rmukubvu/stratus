package dynamodbstreams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/store"
)

const (
	streamsBucket = "dynamodb-streams"
	recordsBucket = "dynamodb-stream-records"
	accountID     = "000000000000"
	region        = "us-east-1"
)

type Service struct {
	metadata store.Store
	lambda   *lambdasvc.Service
	now      func() time.Time
	mu       sync.Mutex
}

type streamRecord struct {
	Arn            string    `json:"arn"`
	CreatedAt      time.Time `json:"created_at"`
	Label          string    `json:"label"`
	ShardID        string    `json:"shard_id"`
	Status         string    `json:"status"`
	StreamViewType string    `json:"stream_view_type"`
	TableName      string    `json:"table_name"`
}

type record struct {
	CreatedAt  time.Time      `json:"created_at"`
	EventID    string         `json:"event_id"`
	EventName  string         `json:"event_name"`
	NewImage   map[string]any `json:"new_image,omitempty"`
	OldImage   map[string]any `json:"old_image,omitempty"`
	Sequence   int            `json:"sequence"`
	StreamArn  string         `json:"stream_arn"`
	StreamType string         `json:"stream_type"`
	TableName  string         `json:"table_name"`
	Keys       map[string]any `json:"keys"`
}

type iteratorToken struct {
	Position  int    `json:"position"`
	StreamArn string `json:"stream_arn"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) SetLambda(lambda *lambdasvc.Service) {
	s.lambda = lambda
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "ListStreams":
		return s.listStreams(w, r)
	case "DescribeStream":
		return s.describeStream(w, r)
	case "GetShardIterator":
		return s.getShardIterator(w, r)
	case "GetRecords":
		return s.getRecords(w, r)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "dynamodbstreams operation is not implemented"}
	}
}

func (s *Service) EnsureStream(tableName, viewType string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if stream, err := s.loadStreamByTable(tableName); err == nil {
		return stream.Arn, nil
	}
	label := s.now().UTC().Format("2006-01-02T15:04:05.000")
	record := streamRecord{
		Arn:            streamARN(tableName, label),
		CreatedAt:      s.now().UTC(),
		Label:          label,
		ShardID:        "shardId-00000000000000000000",
		Status:         "ENABLED",
		StreamViewType: viewType,
		TableName:      tableName,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return "", internal(err)
	}
	if err := s.metadata.Put(streamsBucket, tableName, raw); err != nil {
		return "", internal(err)
	}
	return record.Arn, nil
}

func (s *Service) DeleteStream(tableName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.metadata.Delete(streamsBucket, tableName); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(recordsBucket, tableName+"|"); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) AddRecord(tableName string, keys, oldImage, newImage map[string]any, eventName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, err := s.loadStreamByTable(tableName)
	if err != nil {
		return nil
	}
	next, err := s.nextSequence(tableName)
	if err != nil {
		return err
	}
	rec := record{
		CreatedAt:  s.now().UTC(),
		EventID:    uuid.NewString(),
		EventName:  eventName,
		NewImage:   selectImage(stream.StreamViewType, "new", keys, oldImage, newImage),
		OldImage:   selectImage(stream.StreamViewType, "old", keys, oldImage, newImage),
		Sequence:   next,
		StreamArn:  stream.Arn,
		StreamType: stream.StreamViewType,
		TableName:  tableName,
		Keys:       cloneMap(keys),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(recordsBucket, recordKey(tableName, next), raw); err != nil {
		return internal(err)
	}
	if s.lambda != nil {
		payload := map[string]any{
			"awsRegion":      region,
			"eventID":        rec.EventID,
			"eventName":      rec.EventName,
			"eventSource":    "aws:dynamodb",
			"eventSourceARN": rec.StreamArn,
			"dynamodb": map[string]any{
				"ApproximateCreationDateTime": float64(rec.CreatedAt.Unix()),
				"Keys":                        rec.Keys,
				"NewImage":                    rec.NewImage,
				"OldImage":                    rec.OldImage,
				"SequenceNumber":              stringInt(next),
				"StreamViewType":              stream.StreamViewType,
				"SizeBytes":                   len(raw),
			},
		}
		s.lambda.DispatchDynamoDBRecords(context.Background(), stream.Arn, []map[string]any{payload})
	}
	return nil
}

func (s *Service) listStreams(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TableName string `json:"TableName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	prefix := ""
	if input.TableName != "" {
		prefix = input.TableName
	}
	if err := s.metadata.Scan(streamsBucket, prefix, func(_, v []byte) error {
		var record streamRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"StreamArn":   record.Arn,
			"StreamLabel": record.Label,
			"TableName":   record.TableName,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["TableName"].(string) < items[j]["TableName"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"Streams": items})
	return nil
}

func (s *Service) describeStream(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		StreamArn string `json:"StreamArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.loadStreamByARN(input.StreamArn)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"StreamDescription": map[string]any{
			"CreationRequestDateTime": stream.CreatedAt.Format(time.RFC3339),
			"KeySchema": []map[string]string{{"AttributeName": "", "KeyType": "HASH"}},
			"LastEvaluatedShardId":    nil,
			"Shards": []map[string]any{{
				"SequenceNumberRange": map[string]any{"StartingSequenceNumber": "1"},
				"ShardId":             stream.ShardID,
			}},
			"StreamArn":       stream.Arn,
			"StreamLabel":     stream.Label,
			"StreamStatus":    stream.Status,
			"StreamViewType":  stream.StreamViewType,
			"TableName":       stream.TableName,
		},
	})
	return nil
}

func (s *Service) getShardIterator(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ShardID           string `json:"ShardId"`
		ShardIteratorType string `json:"ShardIteratorType"`
		StreamArn         string `json:"StreamArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.loadStreamByARN(input.StreamArn)
	if err != nil {
		return err
	}
	position := 0
	if input.ShardIteratorType == "LATEST" {
		position, err = s.recordCount(stream.TableName)
		if err != nil {
			return err
		}
	}
	tokenRaw, err := json.Marshal(iteratorToken{Position: position, StreamArn: stream.Arn})
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ShardIterator": base64.StdEncoding.EncodeToString(tokenRaw)})
	return nil
}

func (s *Service) getRecords(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Limit         int    `json:"Limit"`
		ShardIterator string `json:"ShardIterator"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	tokenBytes, err := base64.StdEncoding.DecodeString(input.ShardIterator)
	if err != nil {
		return validation("ShardIterator is invalid")
	}
	var token iteratorToken
	if err := json.Unmarshal(tokenBytes, &token); err != nil {
		return validation("ShardIterator is invalid")
	}
	stream, err := s.loadStreamByARN(token.StreamArn)
	if err != nil {
		return err
	}
	records := make([]map[string]any, 0)
	position := token.Position
	if err := s.metadata.Scan(recordsBucket, stream.TableName+"|", func(_, v []byte) error {
		var rec record
		if err := json.Unmarshal(v, &rec); err != nil {
			return nil
		}
		if rec.Sequence <= position {
			return nil
		}
		records = append(records, map[string]any{
			"awsRegion":      region,
			"eventID":        rec.EventID,
			"eventName":      rec.EventName,
			"eventSource":    "aws:dynamodb",
			"eventSourceARN": rec.StreamArn,
			"dynamodb": map[string]any{
				"ApproximateCreationDateTime": float64(rec.CreatedAt.Unix()),
				"Keys":                        rec.Keys,
				"NewImage":                    rec.NewImage,
				"OldImage":                    rec.OldImage,
				"SequenceNumber":              stringInt(rec.Sequence),
				"StreamViewType":              rec.StreamType,
				"SizeBytes":                   len(v),
			},
		})
		position = rec.Sequence
		if input.Limit > 0 && len(records) >= input.Limit {
			return context.Canceled
		}
		return nil
	}); err != nil && err != context.Canceled {
		return internal(err)
	}
	nextRaw, err := json.Marshal(iteratorToken{Position: position, StreamArn: stream.Arn})
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"NextShardIterator": base64.StdEncoding.EncodeToString(nextRaw),
		"Records":           records,
	})
	return nil
}

func (s *Service) loadStreamByTable(tableName string) (streamRecord, error) {
	raw, err := s.metadata.Get(streamsBucket, tableName)
	if err != nil {
		return streamRecord{}, internal(err)
	}
	if raw == nil {
		return streamRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "stream not found"}
	}
	var record streamRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return streamRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadStreamByARN(arn string) (streamRecord, error) {
	var found streamRecord
	matched := false
	if err := s.metadata.Scan(streamsBucket, "", func(_, v []byte) error {
		var record streamRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if record.Arn == arn {
			found = record
			matched = true
		}
		return nil
	}); err != nil {
		return streamRecord{}, internal(err)
	}
	if !matched {
		return streamRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "stream not found"}
	}
	return found, nil
}

func (s *Service) nextSequence(tableName string) (int, error) {
	count, err := s.recordCount(tableName)
	if err != nil {
		return 0, err
	}
	return count + 1, nil
}

func (s *Service) recordCount(tableName string) (int, error) {
	count := 0
	if err := s.metadata.Scan(recordsBucket, tableName+"|", func(_, _ []byte) error {
		count++
		return nil
	}); err != nil {
		return 0, internal(err)
	}
	return count, nil
}

func selectImage(viewType, side string, keys, oldImage, newImage map[string]any) map[string]any {
	switch viewType {
	case "KEYS_ONLY":
		return cloneMap(keys)
	case "NEW_IMAGE":
		if side == "new" {
			return cloneMap(newImage)
		}
	case "OLD_IMAGE":
		if side == "old" {
			return cloneMap(oldImage)
		}
	case "NEW_AND_OLD_IMAGES":
		if side == "new" {
			return cloneMap(newImage)
		}
		return cloneMap(oldImage)
	}
	return nil
}

func streamARN(tableName, label string) string {
	return "arn:aws:dynamodb:" + region + ":" + accountID + ":table/" + tableName + "/stream/" + label
}

func recordKey(tableName string, seq int) string {
	return tableName + "|" + stringInt(seq)
}

func stringInt(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+(n%10))) + out
		n /= 10
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServerError", Message: err.Error()}
}

func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
