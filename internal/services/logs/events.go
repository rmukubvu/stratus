package logs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// storedEvent is one log event as kept in the events bucket.
type storedEvent struct {
	Message       string `json:"message"`
	Timestamp     int64  `json:"timestamp"`
	IngestionTime int64  `json:"ingestion_time,omitempty"`
}

// streamEvent is a stored event with the stream it belongs to.
type streamEvent struct {
	storedEvent
	stream string
	id     string
}

// eventKey orders a stream's events by timestamp, then by arrival, so
// lines written in the same millisecond keep their order.
func eventKey(group, stream string, timestamp, sequence int64) string {
	return fmt.Sprintf("%s|%013d|%012d", streamKey(group, stream), timestamp, sequence)
}

// Append writes lines to a log stream, creating the group and stream when
// they do not exist, as the Lambda runtime does for a function's output.
func (s *Service) Append(group, stream string, lines []string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.createGroupIfMissing(group); err != nil {
		return err
	}
	record, err := s.loadStream(group, stream)
	if err != nil {
		record = logStreamRecord{GroupName: group, StreamName: stream, CreatedAt: s.now().UTC()}
	}
	events := make([]storedEvent, len(lines))
	for i, line := range lines {
		events[i] = storedEvent{Message: line, Timestamp: at.UnixMilli()}
	}
	return s.appendEvents(&record, events)
}

func (s *Service) createGroupIfMissing(name string) error {
	raw, err := s.metadata.Get(logGroupsBucket, name)
	if err != nil {
		return internal(err)
	}
	if raw != nil {
		return nil
	}
	return s.putGroup(name)
}

func (s *Service) putGroup(name string) error {
	raw, err := json.Marshal(logGroupRecord{Name: name, CreatedAt: s.now().UTC()})
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(logGroupsBucket, name, raw); err != nil {
		return internal(err)
	}
	return nil
}

// appendEvents stores events in record's stream and saves the stream's
// counters. Callers hold s.mu.
func (s *Service) appendEvents(record *logStreamRecord, events []storedEvent) error {
	ingested := s.now().UTC().UnixMilli()
	for _, event := range events {
		record.SequenceToken++
		event.IngestionTime = ingested
		payload, err := json.Marshal(event)
		if err != nil {
			return internal(err)
		}
		if err := s.metadata.Put(logEventsBucket, eventKey(record.GroupName, record.StreamName, event.Timestamp, record.SequenceToken), payload); err != nil {
			return internal(err)
		}
		record.StoredBytes += int64(len(event.Message))
		record.LastEventTime = event.Timestamp
		record.LastIngestionTime = ingested
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(logStreamsBucket, streamKey(record.GroupName, record.StreamName), raw); err != nil {
		return internal(err)
	}
	return nil
}

// streamEvents returns a stream's events, oldest first.
func (s *Service) streamEvents(group, stream string) ([]streamEvent, error) {
	var events []streamEvent
	err := s.metadata.Scan(logEventsBucket, streamKey(group, stream)+"|", func(k, v []byte) error {
		var e storedEvent
		if json.Unmarshal(v, &e) != nil {
			return nil
		}
		events = append(events, streamEvent{storedEvent: e, stream: stream, id: string(k)})
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp < events[j].Timestamp })
	return events, nil
}

// inWindow keeps events from start (inclusive) to end (exclusive), in
// epoch milliseconds; zero leaves a side open.
func inWindow(events []streamEvent, start, end int64) []streamEvent {
	out := events[:0:0]
	for _, e := range events {
		if (start == 0 || e.Timestamp >= start) && (end == 0 || e.Timestamp < end) {
			out = append(out, e)
		}
	}
	return out
}

// page cuts events at an offset token. The token is the number of events
// already returned, so a caller that follows it sees only new events, and
// gets the same token back when there are none, which ends SDK paginators.
func page(events []streamEvent, token string, limit int) ([]streamEvent, int, error) {
	offset := 0
	if token != "" {
		_, raw, _ := strings.Cut(token, "/")
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, 0, badRequest("InvalidParameterException", "The specified nextToken is invalid.")
		}
		offset = n
	}
	if offset > len(events) {
		offset = len(events)
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	endAt := min(offset+limit, len(events))
	return events[offset:endAt], endAt, nil
}

type wireEvent struct {
	LogStreamName string `json:"logStreamName,omitempty"`
	Timestamp     int64  `json:"timestamp"`
	Message       string `json:"message"`
	IngestionTime int64  `json:"ingestionTime"`
	EventID       string `json:"eventId,omitempty"`
}

func (s *Service) getLogEvents(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		LogGroupName       string `json:"logGroupName"`
		LogGroupIdentifier string `json:"logGroupIdentifier"`
		LogStreamName      string `json:"logStreamName"`
		StartTime          int64  `json:"startTime"`
		EndTime            int64  `json:"endTime"`
		Limit              int    `json:"limit"`
		NextToken          string `json:"nextToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidParameterException", "request body is not valid JSON")
	}
	group := firstNonEmpty(input.LogGroupName, logGroupNameFromARN(input.LogGroupIdentifier))
	if _, err := s.loadStream(group, input.LogStreamName); err != nil {
		return err
	}
	events, err := s.streamEvents(group, input.LogStreamName)
	if err != nil {
		return err
	}
	selected, next, err := page(inWindow(events, input.StartTime, input.EndTime), input.NextToken, input.Limit)
	if err != nil {
		return err
	}
	out := make([]wireEvent, len(selected))
	for i, e := range selected {
		out[i] = wireEvent{Timestamp: e.Timestamp, Message: e.Message, IngestionTime: e.IngestionTime}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":            out,
		"nextForwardToken":  fmt.Sprintf("f/%d", next),
		"nextBackwardToken": "b/0",
	})
	return nil
}

func (s *Service) filterLogEvents(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		LogGroupName        string   `json:"logGroupName"`
		LogGroupIdentifier  string   `json:"logGroupIdentifier"`
		LogStreamNames      []string `json:"logStreamNames"`
		LogStreamNamePrefix string   `json:"logStreamNamePrefix"`
		StartTime           int64    `json:"startTime"`
		EndTime             int64    `json:"endTime"`
		FilterPattern       string   `json:"filterPattern"`
		Limit               int      `json:"limit"`
		NextToken           string   `json:"nextToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidParameterException", "request body is not valid JSON")
	}
	group := firstNonEmpty(input.LogGroupName, logGroupNameFromARN(input.LogGroupIdentifier))
	if err := s.ensureGroup(group); err != nil {
		return err
	}
	streams, err := s.ListStreams(group)
	if err != nil {
		return internal(err)
	}
	match := compileFilter(input.FilterPattern)
	var events []streamEvent
	searched := make([]map[string]any, 0, len(streams))
	for _, st := range streams {
		if !hasPrefix(st.StreamName, input.LogStreamNamePrefix) || (len(input.LogStreamNames) > 0 && !containsString(input.LogStreamNames, st.StreamName)) {
			continue
		}
		searched = append(searched, map[string]any{"logStreamName": st.StreamName, "searchedCompletely": true})
		streamEvents, err := s.streamEvents(group, st.StreamName)
		if err != nil {
			return err
		}
		for _, e := range inWindow(streamEvents, input.StartTime, input.EndTime) {
			if match(e.Message) {
				events = append(events, e)
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp < events[j].Timestamp })
	selected, next, err := page(events, input.NextToken, input.Limit)
	if err != nil {
		return err
	}
	out := make([]wireEvent, len(selected))
	for i, e := range selected {
		out[i] = wireEvent{LogStreamName: e.stream, Timestamp: e.Timestamp, Message: e.Message, IngestionTime: e.IngestionTime, EventID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(e.id)).String()}
	}
	resp := map[string]any{"events": out, "searchedLogStreams": searched}
	if next < len(events) {
		resp["nextToken"] = fmt.Sprintf("f/%d", next)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// compileFilter supports CloudWatch's plain term patterns: every term must
// appear in the message, and a quoted phrase must appear as written. An
// empty pattern matches everything. JSON and space-delimited patterns are
// not supported and match by their literal text.
func compileFilter(pattern string) func(string) bool {
	terms := filterTerms(pattern)
	return func(message string) bool {
		for _, term := range terms {
			if !strings.Contains(message, term) {
				return false
			}
		}
		return true
	}
}

func filterTerms(pattern string) []string {
	var terms []string
	rest := strings.TrimSpace(pattern)
	for rest != "" {
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				terms = append(terms, rest[1:])
				break
			}
			terms = append(terms, rest[1:end+1])
			rest = strings.TrimSpace(rest[end+2:])
			continue
		}
		term, after, _ := strings.Cut(rest, " ")
		terms = append(terms, term)
		rest = strings.TrimSpace(after)
	}
	return terms
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
