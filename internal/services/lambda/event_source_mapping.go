package lambda

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// eventSourceMappingInput is the body of CreateEventSourceMapping and
// UpdateEventSourceMapping. Pointers tell an update which settings it
// changes.
type eventSourceMappingInput struct {
	BatchSize                      int            `json:"BatchSize"`
	Enabled                        *bool          `json:"Enabled"`
	EventSourceArn                 string         `json:"EventSourceArn"`
	FunctionName                   string         `json:"FunctionName"`
	FunctionResponseTypes          *[]string      `json:"FunctionResponseTypes"`
	MaximumBatchingWindowInSeconds *int           `json:"MaximumBatchingWindowInSeconds"`
	ScalingConfig                  *scalingConfig `json:"ScalingConfig"`
	StartingPosition               string         `json:"StartingPosition"`
}

type scalingConfig struct {
	MaximumConcurrency int `json:"MaximumConcurrency,omitempty"`
}

// pollers is how many batches an SQS mapping processes at once.
func (m eventSourceMappingRecord) pollers() int {
	if m.MaximumConcurrency > 0 {
		return m.MaximumConcurrency
	}
	return defaultSQSPollers
}

func (m eventSourceMappingRecord) reportsItemFailures() bool {
	for _, value := range m.FunctionResponseTypes {
		if value == ReportBatchItemFailures {
			return true
		}
	}
	return false
}

// validate checks a mapping against the limits AWS enforces.
func (m eventSourceMappingRecord) validate() error {
	if m.EventSourceArn == "" {
		return badRequest("InvalidParameterValueException", "EventSourceArn is required")
	}
	if strings.Contains(m.EventSourceArn, ":stream/") && m.StartingPosition == "" {
		return badRequest("InvalidParameterValueException", "StartingPosition is required for stream event sources")
	}
	for _, value := range m.FunctionResponseTypes {
		if value != ReportBatchItemFailures {
			return badRequest("InvalidParameterValueException", "FunctionResponseTypes may only contain "+ReportBatchItemFailures)
		}
	}
	if m.MaximumBatchingWindowInSeconds < 0 || m.MaximumBatchingWindowInSeconds > maxSQSBatchingWindow {
		return badRequest("InvalidParameterValueException", fmt.Sprintf("MaximumBatchingWindowInSeconds must be between 0 and %d", maxSQSBatchingWindow))
	}
	if !isSQSQueueARN(m.EventSourceArn) {
		return nil
	}
	if m.BatchSize < 1 || m.BatchSize > maxSQSBatchSize {
		return badRequest("InvalidParameterValueException", fmt.Sprintf("BatchSize must be between 1 and %d for SQS", maxSQSBatchSize))
	}
	if m.BatchSize > maxSQSUnwindowedBatch && m.MaximumBatchingWindowInSeconds < 1 {
		return badRequest("InvalidParameterValueException", "Maximum batch window in seconds must be greater than 0 if maximum batch size is greater than 10")
	}
	if m.MaximumConcurrency != 0 && (m.MaximumConcurrency < minSQSMaximumConcurrent || m.MaximumConcurrency > maxSQSMaximumConcurrent) {
		return badRequest("InvalidParameterValueException", fmt.Sprintf("ScalingConfig.MaximumConcurrency must be between %d and %d", minSQSMaximumConcurrent, maxSQSMaximumConcurrent))
	}
	return nil
}

// apply copies the settings an update names onto the mapping.
func (m *eventSourceMappingRecord) apply(input eventSourceMappingInput) {
	if input.BatchSize > 0 {
		m.BatchSize = input.BatchSize
	}
	if input.Enabled != nil {
		m.Enabled = *input.Enabled
		m.State = ternary(m.Enabled, "Enabled", "Disabled")
	}
	if input.FunctionResponseTypes != nil {
		m.FunctionResponseTypes = append([]string(nil), (*input.FunctionResponseTypes)...)
	}
	if input.MaximumBatchingWindowInSeconds != nil {
		m.MaximumBatchingWindowInSeconds = *input.MaximumBatchingWindowInSeconds
	}
	if input.ScalingConfig != nil {
		m.MaximumConcurrency = input.ScalingConfig.MaximumConcurrency
	}
}

func (s *Service) createEventSourceMappingFromInput(input eventSourceMappingInput) (eventSourceMappingRecord, error) {
	record, err := s.resolveFunctionFromReference(input.FunctionName)
	if err != nil {
		return eventSourceMappingRecord{}, err
	}
	now := s.now().UTC()
	mapping := eventSourceMappingRecord{
		BatchSize:        input.BatchSize,
		CreatedAt:        now,
		Enabled:          true,
		EventSourceArn:   input.EventSourceArn,
		FunctionArn:      s.lambdaARN(record.FunctionName),
		FunctionName:     record.FunctionName,
		LastModified:     now.Format(time.RFC3339),
		StartingPosition: input.StartingPosition,
		State:            "Enabled",
		UUID:             uuid.NewString(),
	}
	if mapping.BatchSize <= 0 {
		mapping.BatchSize = ternaryInt(isSQSQueueARN(input.EventSourceArn), defaultSQSBatchSize, 1)
	}
	input.BatchSize = 0
	mapping.apply(input)
	if err := mapping.validate(); err != nil {
		return eventSourceMappingRecord{}, err
	}
	if err := s.saveEventSourceMapping(mapping); err != nil {
		return eventSourceMappingRecord{}, err
	}
	return mapping, nil
}

func (s *Service) updateEventSourceMapping(w http.ResponseWriter, r *http.Request) error {
	uuidValue, err := mappingUUIDFromPath(r.URL.Path)
	if err != nil {
		return err
	}
	var input eventSourceMappingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return badRequest("InvalidRequestContentException", "request body is not valid JSON")
	}
	mapping, err := s.loadEventSourceMapping(uuidValue)
	if err != nil {
		return err
	}
	if input.FunctionName != "" {
		record, err := s.resolveFunctionFromReference(input.FunctionName)
		if err != nil {
			return err
		}
		mapping.FunctionName = record.FunctionName
		mapping.FunctionArn = s.lambdaARN(record.FunctionName)
	}
	mapping.apply(input)
	if err := mapping.validate(); err != nil {
		return err
	}
	mapping.LastModified = s.now().UTC().Format(time.RFC3339)
	if err := s.saveEventSourceMapping(mapping); err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, eventSourceMappingResponse(mapping))
	return nil
}

func (s *Service) saveEventSourceMapping(mapping eventSourceMappingRecord) error {
	raw, err := json.Marshal(mapping)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(mappingsBucket, mapping.UUID, raw); err != nil {
		return internal(err)
	}
	return nil
}

func eventSourceMappingResponse(record eventSourceMappingRecord) map[string]any {
	resp := map[string]any{
		"BatchSize":             record.BatchSize,
		"EventSourceArn":        record.EventSourceArn,
		"FunctionArn":           record.FunctionArn,
		"FunctionName":          record.FunctionName,
		"FunctionResponseTypes": append([]string{}, record.FunctionResponseTypes...),
		"LastModified":          record.LastModified,
		"State":                 record.State,
		"StateTransitionReason": "USER_INITIATED",
		"UUID":                  record.UUID,
	}
	if record.StartingPosition != "" {
		resp["StartingPosition"] = record.StartingPosition
	}
	if record.MaximumBatchingWindowInSeconds > 0 || isSQSQueueARN(record.EventSourceArn) {
		resp["MaximumBatchingWindowInSeconds"] = record.MaximumBatchingWindowInSeconds
	}
	if record.MaximumConcurrency > 0 {
		resp["ScalingConfig"] = scalingConfig{MaximumConcurrency: record.MaximumConcurrency}
	}
	return resp
}
