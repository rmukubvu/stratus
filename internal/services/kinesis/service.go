package kinesis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/store"
)

const (
	streamsBucket = "kinesis-streams"
	recordsBucket = "kinesis-records"
	accountID     = "000000000000"
	region        = "us-east-1"
)

type Service struct {
	metadata store.Store
	lambda   *lambdasvc.Service
	now      func() time.Time
	mu       sync.Mutex
}

type CreateStreamInput struct {
	Mode       string
	ShardCount int
	StreamName string
}

type streamRecord struct {
	CreatedAt    time.Time `json:"created_at"`
	Mode         string    `json:"mode"`
	Name         string    `json:"name"`
	NextSequence int64     `json:"next_sequence"`
	ShardCount   int       `json:"shard_count"`
	Status       string    `json:"status"`
}

type record struct {
	ApproximateArrivalTimestamp time.Time `json:"approximate_arrival_timestamp"`
	Data                        []byte    `json:"data"`
	PartitionKey                string    `json:"partition_key"`
	SequenceNumber              string    `json:"sequence_number"`
	ShardID                     string    `json:"shard_id"`
	StreamName                  string    `json:"stream_name"`
}

type iteratorToken struct {
	Position   int    `json:"position"`
	ShardID    string `json:"shard_id"`
	StreamName string `json:"stream_name"`
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
	case "CreateStream":
		return s.createStream(w, r)
	case "DescribeStreamSummary":
		return s.describeStreamSummary(w, r)
	case "DescribeStream":
		return s.describeStream(w, r)
	case "ListStreams":
		return s.listStreams(w)
	case "ListShards":
		return s.listShards(w, r)
	case "PutRecord":
		return s.putRecord(w, r)
	case "PutRecords":
		return s.putRecords(w, r)
	case "GetShardIterator":
		return s.getShardIterator(w, r)
	case "GetRecords":
		return s.getRecords(w, r)
	case "DeleteStream":
		return s.deleteStream(w, r)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "kinesis operation is not implemented"}
	}
}

func (s *Service) createStream(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ShardCount        int `json:"ShardCount"`
		StreamModeDetails struct {
			StreamMode string `json:"StreamMode"`
		} `json:"StreamModeDetails"`
		StreamName string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.StreamName == "" {
		return validation("StreamName is required")
	}
	if _, err := s.loadStream(input.StreamName); err == nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceInUseException", Message: "Stream already exists"}
	}
	mode := input.StreamModeDetails.StreamMode
	if mode == "" {
		mode = "PROVISIONED"
	}
	if mode != "PROVISIONED" && mode != "ON_DEMAND" {
		return validation("unsupported stream mode")
	}
	shardCount := input.ShardCount
	if mode == "ON_DEMAND" && shardCount == 0 {
		shardCount = 1
	}
	if shardCount <= 0 {
		return validation("ShardCount must be greater than 0")
	}
	record := streamRecord{
		CreatedAt:    s.now().UTC(),
		Mode:         mode,
		Name:         input.StreamName,
		NextSequence: 1,
		ShardCount:   shardCount,
		Status:       "ACTIVE",
	}
	if err := s.putStream(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) CreateStream(input CreateStreamInput) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.StreamName == "" {
		return "", validation("StreamName is required")
	}
	if _, err := s.loadStream(input.StreamName); err == nil {
		return streamARN(input.StreamName), nil
	}
	mode := input.Mode
	if mode == "" {
		mode = "PROVISIONED"
	}
	shardCount := input.ShardCount
	if shardCount <= 0 {
		shardCount = 1
	}
	record := streamRecord{
		CreatedAt:    s.now().UTC(),
		Mode:         mode,
		Name:         input.StreamName,
		NextSequence: 1,
		ShardCount:   shardCount,
		Status:       "ACTIVE",
	}
	if err := s.putStream(record); err != nil {
		return "", internal(err)
	}
	return streamARN(record.Name), nil
}

func (s *Service) DeleteStreamByName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, err := s.loadStream(name)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(streamsBucket, stream.Name); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(recordsBucket, stream.Name+"|"); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) describeStreamSummary(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		StreamARN  string `json:"StreamARN"`
		StreamName string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"StreamDescriptionSummary": map[string]any{
			"EncryptionType":       "NONE",
			"OpenShardCount":       stream.ShardCount,
			"RetentionPeriodHours": 24,
			"StreamARN":            streamARN(stream.Name),
			"StreamModeDetails":    map[string]any{"StreamMode": stream.Mode},
			"StreamName":           stream.Name,
			"StreamStatus":         stream.Status,
		},
	})
	return nil
}

func (s *Service) listStreams(w http.ResponseWriter) error {
	names := make([]string, 0)
	if err := s.metadata.Scan(streamsBucket, "", func(k, _ []byte) error {
		names = append(names, string(k))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{
		"HasMoreStreams": false,
		"StreamNames":    names,
	})
	return nil
}

func (s *Service) listShards(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		NextToken  string `json:"NextToken"`
		StreamARN  string `json:"StreamARN"`
		StreamName string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.NextToken != "" {
		return notImplemented("pagination is not supported")
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	shards := make([]map[string]any, 0, stream.ShardCount)
	for i := 0; i < stream.ShardCount; i++ {
		shards = append(shards, map[string]any{
			"HashKeyRange": map[string]string{
				"EndingHashKey":   "340282366920938463463374607431768211455",
				"StartingHashKey": "0",
			},
			"SequenceNumberRange": map[string]string{
				"StartingSequenceNumber": "0",
			},
			"ShardId": shardID(i),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"Shards": shards})
	return nil
}

func (s *Service) putRecord(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Data         []byte `json:"Data"`
		PartitionKey string `json:"PartitionKey"`
		StreamARN    string `json:"StreamARN"`
		StreamName   string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.PartitionKey == "" {
		return validation("PartitionKey is required")
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	shardIndex := chooseShard(input.PartitionKey, stream.ShardCount)
	sequenceNumber := strconv.FormatInt(stream.NextSequence, 10)
	item := record{
		ApproximateArrivalTimestamp: s.now().UTC(),
		Data:                        input.Data,
		PartitionKey:                input.PartitionKey,
		SequenceNumber:              sequenceNumber,
		ShardID:                     shardID(shardIndex),
		StreamName:                  stream.Name,
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(recordsBucket, recordKey(stream.Name, item.ShardID, stream.NextSequence), raw); err != nil {
		return internal(err)
	}
	s.dispatchRecord(stream.Name, item)
	stream.NextSequence++
	if err := s.putStream(stream); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"SequenceNumber": sequenceNumber,
		"ShardId":        item.ShardID,
	})
	return nil
}

func (s *Service) putRecords(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Records []struct {
			Data         []byte `json:"Data"`
			PartitionKey string `json:"PartitionKey"`
		} `json:"Records"`
		StreamARN  string `json:"StreamARN"`
		StreamName string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	results := make([]map[string]any, 0, len(input.Records))
	dispatched := make([]record, 0, len(input.Records))
	for _, entry := range input.Records {
		if entry.PartitionKey == "" {
			results = append(results, map[string]any{"ErrorCode": "InvalidArgumentException", "ErrorMessage": "PartitionKey is required"})
			continue
		}
		shardIndex := chooseShard(entry.PartitionKey, stream.ShardCount)
		sequenceNumber := strconv.FormatInt(stream.NextSequence, 10)
		item := record{
			ApproximateArrivalTimestamp: s.now().UTC(),
			Data:                        entry.Data,
			PartitionKey:                entry.PartitionKey,
			SequenceNumber:              sequenceNumber,
			ShardID:                     shardID(shardIndex),
			StreamName:                  stream.Name,
		}
		raw, err := json.Marshal(item)
		if err != nil {
			return internal(err)
		}
		if err := s.metadata.Put(recordsBucket, recordKey(stream.Name, item.ShardID, stream.NextSequence), raw); err != nil {
			return internal(err)
		}
		stream.NextSequence++
		dispatched = append(dispatched, item)
		results = append(results, map[string]any{"SequenceNumber": sequenceNumber, "ShardId": item.ShardID})
	}
	if err := s.putStream(stream); err != nil {
		return internal(err)
	}
	for _, item := range dispatched {
		s.dispatchRecord(stream.Name, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"FailedRecordCount": 0, "Records": results})
	return nil
}

func (s *Service) getShardIterator(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Timestamp              time.Time `json:"Timestamp"`
		ShardID                string    `json:"ShardId"`
		ShardIteratorType      string    `json:"ShardIteratorType"`
		StartingSequenceNumber string    `json:"StartingSequenceNumber"`
		StreamARN              string    `json:"StreamARN"`
		StreamName             string    `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	if !validShardID(input.ShardID, stream.ShardCount) {
		return validation("ShardId is invalid")
	}
	records, err := s.loadShardRecords(stream.Name, input.ShardID)
	if err != nil {
		return err
	}
	position, err := iteratorStartPosition(input.ShardIteratorType, input.StartingSequenceNumber, input.Timestamp, records)
	if err != nil {
		return err
	}
	token, err := encodeIterator(iteratorToken{Position: position, ShardID: input.ShardID, StreamName: stream.Name})
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ShardIterator": token})
	return nil
}

func (s *Service) describeStream(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ExclusiveStartShardID string `json:"ExclusiveStartShardId"`
		Limit                 int    `json:"Limit"`
		StreamARN             string `json:"StreamARN"`
		StreamName            string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.ExclusiveStartShardID != "" {
		return notImplemented("ExclusiveStartShardId is not supported")
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	shards := make([]map[string]any, 0, stream.ShardCount)
	for i := 0; i < stream.ShardCount; i++ {
		shards = append(shards, map[string]any{
			"ShardId": shardID(i),
			"HashKeyRange": map[string]string{
				"EndingHashKey":   "340282366920938463463374607431768211455",
				"StartingHashKey": "0",
			},
			"SequenceNumberRange": map[string]string{
				"StartingSequenceNumber": "0",
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"StreamDescription": map[string]any{
			"HasMoreShards": false,
			"Shards":        shards,
			"StreamARN":     streamARN(stream.Name),
			"StreamName":    stream.Name,
			"StreamStatus":  stream.Status,
		},
	})
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
	token, err := decodeIterator(input.ShardIterator)
	if err != nil {
		return validation("ShardIterator is invalid")
	}
	if _, err := s.loadStream(token.StreamName); err != nil {
		return err
	}
	records, err := s.loadShardRecords(token.StreamName, token.ShardID)
	if err != nil {
		return err
	}
	limit := input.Limit
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	start := token.Position
	if start < 0 {
		start = 0
	}
	if start > len(records) {
		start = len(records)
	}
	end := start + limit
	if end > len(records) {
		end = len(records)
	}
	items := make([]map[string]any, 0, end-start)
	for _, rec := range records[start:end] {
		items = append(items, map[string]any{
			"ApproximateArrivalTimestamp": rec.ApproximateArrivalTimestamp.Format(time.RFC3339),
			"Data":                        base64.StdEncoding.EncodeToString(rec.Data),
			"PartitionKey":                rec.PartitionKey,
			"SequenceNumber":              rec.SequenceNumber,
		})
	}
	nextToken, err := encodeIterator(iteratorToken{Position: end, ShardID: token.ShardID, StreamName: token.StreamName})
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"MillisBehindLatest": 0,
		"NextShardIterator":  nextToken,
		"Records":            items,
	})
	return nil
}

func (s *Service) deleteStream(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		StreamARN  string `json:"StreamARN"`
		StreamName string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(streamsBucket, stream.Name); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(recordsBucket, stream.Name+"|"); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) resolveStream(name, arn string) (streamRecord, error) {
	if name == "" && arn != "" {
		parts := strings.Split(arn, "/")
		name = parts[len(parts)-1]
	}
	return s.loadStream(name)
}

func (s *Service) loadStream(name string) (streamRecord, error) {
	if name == "" {
		return streamRecord{}, validation("StreamName is required")
	}
	raw, err := s.metadata.Get(streamsBucket, name)
	if err != nil {
		return streamRecord{}, internal(err)
	}
	if raw == nil {
		return streamRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "Stream not found"}
	}
	var record streamRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return streamRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putStream(record streamRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(streamsBucket, record.Name, raw)
}

func (s *Service) loadShardRecords(streamName, shard string) ([]record, error) {
	out := make([]record, 0)
	if err := s.metadata.Scan(recordsBucket, streamName+"|"+shard+"|", func(_, v []byte) error {
		var rec record
		if err := json.Unmarshal(v, &rec); err != nil {
			return nil
		}
		out = append(out, rec)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(out, func(i, j int) bool {
		left, _ := strconv.ParseInt(out[i].SequenceNumber, 10, 64)
		right, _ := strconv.ParseInt(out[j].SequenceNumber, 10, 64)
		return left < right
	})
	return out, nil
}

func iteratorStartPosition(kind, seq string, ts time.Time, records []record) (int, error) {
	switch kind {
	case "TRIM_HORIZON":
		return 0, nil
	case "LATEST":
		return len(records), nil
	case "AT_TIMESTAMP":
		if ts.IsZero() {
			return 0, validation("Timestamp is required")
		}
		for idx, rec := range records {
			if !rec.ApproximateArrivalTimestamp.Before(ts) {
				return idx, nil
			}
		}
		return len(records), nil
	case "AT_SEQUENCE_NUMBER", "AFTER_SEQUENCE_NUMBER":
		if seq == "" {
			return 0, validation("StartingSequenceNumber is required")
		}
		for idx, rec := range records {
			if rec.SequenceNumber == seq {
				if kind == "AFTER_SEQUENCE_NUMBER" {
					return idx + 1, nil
				}
				return idx, nil
			}
		}
		return len(records), nil
	default:
		return 0, validation("unsupported ShardIteratorType")
	}
}

func (s *Service) dispatchRecord(streamName string, item record) {
	if s.lambda == nil {
		return
	}
	payload := map[string]any{
		"eventID":           item.ShardID + ":" + item.SequenceNumber,
		"eventSource":       "aws:kinesis",
		"eventSourceARN":    streamARN(streamName),
		"eventVersion":      "1.0",
		"invokeIdentityArn": "arn:aws:iam::000000000000:role/stratus",
		"awsRegion":         region,
		"kinesis": map[string]any{
			"approximateArrivalTimestamp": float64(item.ApproximateArrivalTimestamp.Unix()),
			"data":                        base64.StdEncoding.EncodeToString(item.Data),
			"kinesisSchemaVersion":        "1.0",
			"partitionKey":                item.PartitionKey,
			"sequenceNumber":              item.SequenceNumber,
			"streamName":                  streamName,
		},
	}
	s.lambda.DispatchKinesisRecords(context.Background(), streamARN(streamName), []map[string]any{payload})
}

func encodeIterator(token iteratorToken) (string, error) {
	raw, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func decodeIterator(encoded string) (iteratorToken, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return iteratorToken{}, err
	}
	var token iteratorToken
	if err := json.Unmarshal(raw, &token); err != nil {
		return iteratorToken{}, err
	}
	return token, nil
}

func chooseShard(partitionKey string, shardCount int) int {
	if shardCount <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(partitionKey))
	return int(h.Sum32() % uint32(shardCount))
}

func shardID(index int) string {
	return fmt.Sprintf("shardId-%012d", index)
}

func validShardID(id string, count int) bool {
	for i := 0; i < count; i++ {
		if shardID(i) == id {
			return true
		}
	}
	return false
}

func streamARN(name string) string {
	return fmt.Sprintf("arn:aws:kinesis:%s:%s:stream/%s", region, accountID, name)
}

func recordKey(streamName, shard string, seq int64) string {
	return fmt.Sprintf("%s|%s|%020d", streamName, shard, seq)
}

func decodeJSON(r *http.Request, out any) error {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidArgumentException", Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
