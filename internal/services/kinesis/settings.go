package kinesis

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/tags"
)

// Kinesis Data Streams retention bounds, in hours: 24 hours by default,
// up to 365 days.
const (
	defaultRetentionHours = 24
	maxRetentionHours     = 8760
	maxTagsPerStream      = 50
)

func (r streamRecord) retentionHours() int {
	if r.RetentionHours == 0 {
		return defaultRetentionHours
	}
	return r.RetentionHours
}

// changeRetention is IncreaseStreamRetentionPeriod (increase) or
// DecreaseStreamRetentionPeriod. AWS refuses a change in the wrong
// direction, and one outside 24 to 8760 hours.
func (s *Service) changeRetention(w http.ResponseWriter, r *http.Request, increase bool) error {
	var input struct {
		RetentionPeriodHours int    `json:"RetentionPeriodHours"`
		StreamARN            string `json:"StreamARN"`
		StreamName           string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	if err := retentionChange(stream, input.RetentionPeriodHours, increase); err != nil {
		return err
	}
	stream.RetentionHours = input.RetentionPeriodHours
	if err := s.putStream(stream); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func retentionChange(stream streamRecord, hours int, increase bool) error {
	if hours < defaultRetentionHours || hours > maxRetentionHours {
		return validation(fmt.Sprintf("Minimum allowed retention period is %d hours. Maximum allowed retention period is %d hours.", defaultRetentionHours, maxRetentionHours))
	}
	current := stream.retentionHours()
	if increase && hours < current {
		return validation(fmt.Sprintf("Requested retention period (%d hours) for stream %s can not be shorter than existing retention period (%d hours). Use DecreaseRetentionPeriod API.", hours, stream.Name, current))
	}
	if !increase && hours > current {
		return validation(fmt.Sprintf("Requested retention period (%d hours) for stream %s can not be longer than existing retention period (%d hours). Use IncreaseRetentionPeriod API.", hours, stream.Name, current))
	}
	return nil
}

func (s *Service) addTagsToStream(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		StreamARN  string            `json:"StreamARN"`
		StreamName string            `json:"StreamName"`
		Tags       map[string]string `json:"Tags"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	merged := stream.Tags.Merge(tags.FromMap(input.Tags))
	if err := validateTags(merged); err != nil {
		return err
	}
	stream.Tags = merged
	if err := s.putStream(stream); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) removeTagsFromStream(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		StreamARN  string   `json:"StreamARN"`
		StreamName string   `json:"StreamName"`
		TagKeys    []string `json:"TagKeys"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	stream.Tags = stream.Tags.Without(input.TagKeys)
	if err := s.putStream(stream); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) listTagsForStream(w http.ResponseWriter, r *http.Request) error {
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
	out := make([]map[string]string, 0, len(stream.Tags))
	for _, t := range stream.Tags {
		out = append(out, map[string]string{"Key": t.Key, "Value": t.Value})
	}
	writeJSON(w, http.StatusOK, map[string]any{"HasMoreTags": false, "Tags": out})
	return nil
}

// validateTags maps a tag list AWS would refuse to Kinesis's errors.
func validateTags(list tags.List) error {
	err := list.Validate(maxTagsPerStream)
	var verr *tags.ValidationError
	if errors.As(err, &verr) {
		if verr.Reason == tags.TooMany {
			return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "LimitExceededException", Message: verr.Error()}
		}
		return validation(verr.Error())
	}
	return err
}

func (r streamRecord) encryptionType() string {
	if r.KeyID != "" {
		return "KMS"
	}
	return "NONE"
}

// setEncryption is StartStreamEncryption (start) or StopStreamEncryption.
// Only KMS encryption exists, and the stream records the key it was given.
func (s *Service) setEncryption(w http.ResponseWriter, r *http.Request, start bool) error {
	var input struct {
		EncryptionType string `json:"EncryptionType"`
		KeyID          string `json:"KeyId"`
		StreamARN      string `json:"StreamARN"`
		StreamName     string `json:"StreamName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.EncryptionType != "KMS" {
		return validation("1 validation error detected: Value '" + input.EncryptionType + "' at 'encryptionType' failed to satisfy constraint: Member must satisfy enum value set: [NONE, KMS]")
	}
	if input.KeyID == "" {
		return validation("KeyId is required")
	}
	stream, err := s.resolveStream(input.StreamName, input.StreamARN)
	if err != nil {
		return err
	}
	stream.KeyID = ""
	if start {
		stream.KeyID = input.KeyID
	}
	if err := s.putStream(stream); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}
