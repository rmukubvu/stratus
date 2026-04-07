package logs

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	logGroupsBucket  = "logs-groups"
	logStreamsBucket = "logs-streams"
	logEventsBucket  = "logs-events"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
}

type logGroupRecord struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type logStreamRecord struct {
	GroupName         string    `json:"group_name"`
	StreamName        string    `json:"stream_name"`
	CreatedAt         time.Time `json:"created_at"`
	LastIngestionTime int64     `json:"last_ingestion_time"`
	LastEventTime     int64     `json:"last_event_time"`
	SequenceToken     int64     `json:"sequence_token"`
	StoredBytes       int64     `json:"stored_bytes"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	switch operation {
	case "CreateLogGroup":
		return s.createLogGroup(w, r)
	case "CreateLogStream":
		return s.createLogStream(w, r)
	case "PutLogEvents":
		return s.putLogEvents(w, r)
	case "DescribeLogStreams":
		return s.describeLogStreams(w, r)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "cloudwatch logs operation is not implemented",
		}
	}
}

func (s *Service) createLogGroup(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		LogGroupName string `json:"logGroupName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidParameterException", "request body is not valid JSON")
	}
	if input.LogGroupName == "" {
		return badRequest("InvalidParameterException", "logGroupName is required")
	}
	raw, err := json.Marshal(logGroupRecord{Name: input.LogGroupName, CreatedAt: s.now().UTC()})
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(logGroupsBucket, input.LogGroupName, raw); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) createLogStream(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		LogGroupName  string `json:"logGroupName"`
		LogStreamName string `json:"logStreamName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidParameterException", "request body is not valid JSON")
	}
	if err := s.ensureGroup(input.LogGroupName); err != nil {
		return err
	}
	record := logStreamRecord{
		GroupName:  input.LogGroupName,
		StreamName: input.LogStreamName,
		CreatedAt:  s.now().UTC(),
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(logStreamsBucket, streamKey(input.LogGroupName, input.LogStreamName), raw); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) putLogEvents(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		LogEvents []struct {
			Message   string `json:"message"`
			Timestamp int64  `json:"timestamp"`
		} `json:"logEvents"`
		LogGroupName  string `json:"logGroupName"`
		LogStreamName string `json:"logStreamName"`
		SequenceToken string `json:"sequenceToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidParameterException", "request body is not valid JSON")
	}
	record, err := s.loadStream(input.LogGroupName, input.LogStreamName)
	if err != nil {
		return err
	}
	for _, event := range input.LogEvents {
		payload, err := json.Marshal(event)
		if err != nil {
			return internal(err)
		}
		if err := s.metadata.Put(logEventsBucket, streamKey(input.LogGroupName, input.LogStreamName)+"|"+strconv.FormatInt(event.Timestamp, 10)+"|"+uuid.NewString(), payload); err != nil {
			return internal(err)
		}
		record.StoredBytes += int64(len(event.Message))
		record.LastEventTime = event.Timestamp
		record.LastIngestionTime = s.now().UTC().UnixMilli()
		record.SequenceToken++
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(logStreamsBucket, streamKey(input.LogGroupName, input.LogStreamName), raw); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nextSequenceToken": strconv.FormatInt(record.SequenceToken, 10),
	})
	return nil
}

func (s *Service) describeLogStreams(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		LogGroupName string `json:"logGroupName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidParameterException", "request body is not valid JSON")
	}
	if err := s.ensureGroup(input.LogGroupName); err != nil {
		return err
	}
	var streams []map[string]any
	if err := s.metadata.Scan(logStreamsBucket, input.LogGroupName+"|", func(_, v []byte) error {
		var record logStreamRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		streams = append(streams, map[string]any{
			"logStreamName":       record.StreamName,
			"creationTime":        record.CreatedAt.UnixMilli(),
			"firstEventTimestamp": record.LastEventTime,
			"lastEventTimestamp":  record.LastEventTime,
			"lastIngestionTime":   record.LastIngestionTime,
			"storedBytes":         record.StoredBytes,
			"uploadSequenceToken": strconv.FormatInt(record.SequenceToken, 10),
			"arn":                 "arn:aws:logs:us-east-1:000000000000:log-group:" + record.GroupName + ":log-stream:" + record.StreamName,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(streams, func(i, j int) bool {
		return streams[i]["logStreamName"].(string) < streams[j]["logStreamName"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{"logStreams": streams})
	return nil
}

func (s *Service) ensureGroup(name string) error {
	raw, err := s.metadata.Get(logGroupsBucket, name)
	if err != nil {
		return internal(err)
	}
	if raw == nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "The specified log group does not exist."}
	}
	return nil
}

func (s *Service) loadStream(group, stream string) (logStreamRecord, error) {
	if err := s.ensureGroup(group); err != nil {
		return logStreamRecord{}, err
	}
	raw, err := s.metadata.Get(logStreamsBucket, streamKey(group, stream))
	if err != nil {
		return logStreamRecord{}, internal(err)
	}
	if raw == nil {
		return logStreamRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "The specified log stream does not exist."}
	}
	var record logStreamRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return logStreamRecord{}, internal(err)
	}
	return record, nil
}

func streamKey(group, stream string) string {
	return group + "|" + stream
}

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "ServiceUnavailableException", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
