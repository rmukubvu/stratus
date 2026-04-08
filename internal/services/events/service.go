package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/services/sns"
	"github.com/stratus/internal/services/sqs"
	"github.com/stratus/internal/services/stepfunctions"
	"github.com/stratus/internal/store"
)

const (
	eventBusesBucket = "events-buses"
	rulesBucket      = "events-rules"
	targetsBucket    = "events-targets"
	archivesBucket   = "events-archives"
	accountID        = "000000000000"
	region           = "us-east-1"
	defaultBusName   = "default"
)

var nameRe = regexp.MustCompile(`^[\.\-_A-Za-z0-9]+$`)

type Service struct {
	metadata store.Store
	lambda   *lambdasvc.Service
	sns      *sns.Service
	sqs      *sqs.Service
	stepFunctions *stepfunctions.Service
	now      func() time.Time
	mu       sync.Mutex
}

type PutRuleInput struct {
	Description        string
	EventBusName       string
	EventPattern       string
	Name               string
	ScheduleExpression string
	State              string
}

type PutTargetsInput struct {
	EventBusName string
	Rule         string
	Targets      []TargetInput
}

type TargetInput struct {
	Arn   string
	ID    string
	Input string
}

type eventBusRecord struct {
	Arn       string    `json:"arn"`
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`
}

type ruleRecord struct {
	Arn                string    `json:"arn"`
	CreatedAt          time.Time `json:"created_at"`
	Description        string    `json:"description,omitempty"`
	EventBusName       string    `json:"event_bus_name"`
	EventPattern       string    `json:"event_pattern"`
	Name               string    `json:"name"`
	ScheduleExpression string    `json:"schedule_expression,omitempty"`
	State              string    `json:"state"`
	LastTriggeredAt    time.Time `json:"last_triggered_at,omitempty"`
}

type targetRecord struct {
	Arn          string `json:"arn"`
	EventBusName string `json:"event_bus_name"`
	ID           string `json:"id"`
	Input        string `json:"input,omitempty"`
	RuleName     string `json:"rule_name"`
}

type createEventBusInput struct {
	Name string `json:"Name"`
}

type deleteEventBusInput struct {
	Name string `json:"Name"`
}

type putRuleInput struct {
	Description        string `json:"Description"`
	EventBusName       string `json:"EventBusName"`
	EventPattern       string `json:"EventPattern"`
	Name               string `json:"Name"`
	RoleArn            string `json:"RoleArn"`
	ScheduleExpression string `json:"ScheduleExpression"`
	State              string `json:"State"`
}

type createArchiveInput struct {
	ArchiveName    string `json:"ArchiveName"`
	Description    string `json:"Description"`
	EventPattern   string `json:"EventPattern"`
	EventSourceArn string `json:"EventSourceArn"`
}

type describeArchiveInput struct {
	ArchiveName string `json:"ArchiveName"`
}

type archiveRecord struct {
	ArchiveArn     string    `json:"archive_arn"`
	ArchiveName    string    `json:"archive_name"`
	CreatedAt      time.Time `json:"created_at"`
	Description    string    `json:"description,omitempty"`
	EventPattern   string    `json:"event_pattern,omitempty"`
	EventSourceArn string    `json:"event_source_arn"`
	State          string    `json:"state"`
}

type listRulesInput struct {
	EventBusName string `json:"EventBusName"`
	NamePrefix   string `json:"NamePrefix"`
}

type describeRuleInput struct {
	EventBusName string `json:"EventBusName"`
	Name         string `json:"Name"`
}

type targetInput struct {
	Arn              string `json:"Arn"`
	ID               string `json:"Id"`
	Input            string `json:"Input"`
	InputPath        string `json:"InputPath"`
	InputTransformer any    `json:"InputTransformer"`
	RoleArn          string `json:"RoleArn"`
}

type putTargetsInput struct {
	EventBusName string        `json:"EventBusName"`
	Rule         string        `json:"Rule"`
	Targets      []targetInput `json:"Targets"`
}

type listTargetsByRuleInput struct {
	EventBusName string `json:"EventBusName"`
	Rule         string `json:"Rule"`
}

type removeTargetsInput struct {
	EventBusName string   `json:"EventBusName"`
	IDs          []string `json:"Ids"`
	Rule         string   `json:"Rule"`
}

type deleteRuleInput struct {
	EventBusName string `json:"EventBusName"`
	Force        bool   `json:"Force"`
	Name         string `json:"Name"`
}

type putEventsInput struct {
	Entries []putEventsEntry `json:"Entries"`
}

type putEventsEntry struct {
	Detail       string   `json:"Detail"`
	DetailType   string   `json:"DetailType"`
	EventBusName string   `json:"EventBusName"`
	Resources    []string `json:"Resources"`
	Source       string   `json:"Source"`
	Time         string   `json:"Time"`
}

func NewService(metadata store.Store, lambda *lambdasvc.Service) *Service {
	svc := &Service{metadata: metadata, lambda: lambda, now: time.Now}
	go svc.runScheduler()
	return svc
}

func (s *Service) SetSQS(target *sqs.Service) {
	s.sqs = target
}

func (s *Service) SetSNS(target *sns.Service) {
	s.sns = target
}

func (s *Service) SetStepFunctions(target *stepfunctions.Service) {
	s.stepFunctions = target
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureDefaultBus(); err != nil {
		return err
	}

	switch operation {
	case "CreateEventBus":
		return s.createEventBus(w, r)
	case "ListEventBuses":
		return s.listEventBuses(w)
	case "DeleteEventBus":
		return s.deleteEventBus(w, r)
	case "PutRule":
		return s.putRule(w, r)
	case "DescribeRule":
		return s.describeRule(w, r)
	case "ListRules":
		return s.listRules(w, r)
	case "PutTargets":
		return s.putTargets(w, r)
	case "ListTargetsByRule":
		return s.listTargetsByRule(w, r)
	case "RemoveTargets":
		return s.removeTargets(w, r)
	case "DeleteRule":
		return s.deleteRule(w, r)
	case "PutEvents":
		return s.putEvents(w, r)
	case "CreateArchive":
		return s.createArchive(w, r)
	case "DescribeArchive":
		return s.describeArchive(w, r)
	case "ListArchives":
		return s.listArchives(w)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "events operation is not implemented",
		}
	}
}

func (s *Service) createEventBus(w http.ResponseWriter, r *http.Request) error {
	var input createEventBusInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if err := validateName(input.Name, "Name is required"); err != nil {
		return err
	}
	if _, err := s.loadEventBus(input.Name); err == nil {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ResourceAlreadyExistsException",
			Message:    "Event bus already exists.",
		}
	}

	record := eventBusRecord{
		Arn:       eventBusARN(input.Name),
		CreatedAt: s.now().UTC(),
		Name:      input.Name,
	}
	if err := s.putEventBus(record); err != nil {
		return internal(err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"EventBusArn": record.Arn,
	})
	return nil
}

func (s *Service) CreateEventBus(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureDefaultBus(); err != nil {
		return "", err
	}
	if err := validateName(name, "Name is required"); err != nil {
		return "", err
	}
	if _, err := s.loadEventBus(name); err == nil {
		return eventBusARN(name), nil
	}
	record := eventBusRecord{Arn: eventBusARN(name), CreatedAt: s.now().UTC(), Name: name}
	if err := s.putEventBus(record); err != nil {
		return "", internal(err)
	}
	return record.Arn, nil
}

func (s *Service) DeleteEventBus(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if name == defaultBusName {
		return validation("cannot delete the default event bus")
	}
	if _, err := s.loadEventBus(name); err != nil {
		return err
	}
	if err := s.metadata.Delete(eventBusesBucket, name); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) PutRule(input PutRuleInput) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureDefaultBus(); err != nil {
		return "", err
	}
	if err := validateName(input.Name, "Name is required"); err != nil {
		return "", err
	}
	busName := defaultBus(input.EventBusName)
	if _, err := s.loadEventBus(busName); err != nil {
		return "", err
	}
	if input.EventPattern == "" && input.ScheduleExpression == "" {
		return "", validation("EventPattern or ScheduleExpression is required")
	}
	if input.EventPattern != "" {
		var rawPattern any
		if err := json.Unmarshal([]byte(input.EventPattern), &rawPattern); err != nil {
			return "", validation("EventPattern must be valid JSON")
		}
	}
	if input.ScheduleExpression != "" {
		if _, _, err := parseScheduleExpression(input.ScheduleExpression); err != nil {
			return "", err
		}
	}
	state := input.State
	if state == "" {
		state = "ENABLED"
	}
	record := ruleRecord{
		Arn:                ruleARN(busName, input.Name),
		CreatedAt:          s.now().UTC(),
		Description:        input.Description,
		EventBusName:       busName,
		EventPattern:       input.EventPattern,
		Name:               input.Name,
		ScheduleExpression: input.ScheduleExpression,
		State:              state,
	}
	if existing, err := s.loadRule(busName, input.Name); err == nil {
		record.CreatedAt = existing.CreatedAt
	}
	if err := s.putRuleRecord(record); err != nil {
		return "", internal(err)
	}
	return record.Arn, nil
}

func (s *Service) PutTargets(input PutTargetsInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	busName := defaultBus(input.EventBusName)
	if _, err := s.loadRule(busName, input.Rule); err != nil {
		return err
	}
	for _, target := range input.Targets {
		if target.ID == "" || !isSupportedTargetARN(target.Arn) {
			return validation("only Lambda, SQS, SNS, and Step Functions targets with explicit Id are supported")
		}
		record := targetRecord{
			Arn:          target.Arn,
			EventBusName: busName,
			ID:           target.ID,
			Input:        target.Input,
			RuleName:     input.Rule,
		}
		if err := s.putTarget(record); err != nil {
			return internal(err)
		}
	}
	return nil
}

func (s *Service) DeleteRule(eventBusName, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	busName := defaultBus(eventBusName)
	if _, err := s.loadRule(busName, name); err != nil {
		return err
	}
	if err := s.metadata.DeletePrefix(targetsBucket, busName+"|"+name+"|"); err != nil {
		return internal(err)
	}
	if err := s.metadata.Delete(rulesBucket, ruleStoreKey(busName, name)); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) listEventBuses(w http.ResponseWriter) error {
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(eventBusesBucket, "", func(_, v []byte) error {
		var record eventBusRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"Arn":  record.Arn,
			"Name": record.Name,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["Name"].(string) < items[j]["Name"].(string) })

	writeJSON(w, http.StatusOK, map[string]any{
		"EventBuses": items,
	})
	return nil
}

func (s *Service) deleteEventBus(w http.ResponseWriter, r *http.Request) error {
	var input deleteEventBusInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.Name == defaultBusName {
		return validation("cannot delete the default event bus")
	}
	if _, err := s.loadEventBus(input.Name); err != nil {
		return err
	}
	hasRules := false
	if err := s.metadata.Scan(rulesBucket, input.Name+"|", func(_, _ []byte) error {
		hasRules = true
		return nil
	}); err != nil {
		return internal(err)
	}
	if hasRules {
		return validation("remove rules from the event bus before deleting it")
	}
	if err := s.metadata.Delete(eventBusesBucket, input.Name); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) putRule(w http.ResponseWriter, r *http.Request) error {
	var input putRuleInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if err := validateName(input.Name, "Name is required"); err != nil {
		return err
	}
	if input.RoleArn != "" {
		return notImplemented("event rule role arns are not supported")
	}
	busName := defaultBus(input.EventBusName)
	if _, err := s.loadEventBus(busName); err != nil {
		return err
	}
	if input.EventPattern == "" && input.ScheduleExpression == "" {
		return validation("EventPattern or ScheduleExpression is required")
	}
	if input.EventPattern != "" {
		var rawPattern any
		if err := json.Unmarshal([]byte(input.EventPattern), &rawPattern); err != nil {
			return validation("EventPattern must be valid JSON")
		}
	}
	if input.ScheduleExpression != "" {
		if _, _, err := parseScheduleExpression(input.ScheduleExpression); err != nil {
			return err
		}
	}
	state := input.State
	if state == "" {
		state = "ENABLED"
	}
	if state != "ENABLED" && state != "DISABLED" {
		return validation("State must be ENABLED or DISABLED")
	}
	record := ruleRecord{
		Arn:                ruleARN(busName, input.Name),
		CreatedAt:          s.now().UTC(),
		Description:        input.Description,
		EventBusName:       busName,
		EventPattern:       input.EventPattern,
		Name:               input.Name,
		ScheduleExpression: input.ScheduleExpression,
		State:              state,
	}
	if existing, err := s.loadRule(busName, input.Name); err == nil {
		record.CreatedAt = existing.CreatedAt
	}
	if err := s.putRuleRecord(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"RuleArn": record.Arn})
	return nil
}

func (s *Service) describeRule(w http.ResponseWriter, r *http.Request) error {
	var input describeRuleInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.loadRule(defaultBus(input.EventBusName), input.Name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ruleResponse(record))
	return nil
}

func (s *Service) listRules(w http.ResponseWriter, r *http.Request) error {
	var input listRulesInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	busName := defaultBus(input.EventBusName)
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(rulesBucket, busName+"|", func(_, v []byte) error {
		var record ruleRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if input.NamePrefix == "" || strings.HasPrefix(record.Name, input.NamePrefix) {
			items = append(items, ruleResponse(record))
		}
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["Name"].(string) < items[j]["Name"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"Rules": items})
	return nil
}

func (s *Service) putTargets(w http.ResponseWriter, r *http.Request) error {
	var input putTargetsInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	busName := defaultBus(input.EventBusName)
	if _, err := s.loadRule(busName, input.Rule); err != nil {
		return err
	}
	failed := make([]map[string]any, 0)
	for _, target := range input.Targets {
		if target.ID == "" {
			failed = append(failed, map[string]any{"TargetId": target.ID, "ErrorCode": "ValidationException", "ErrorMessage": "target Id is required"})
			continue
		}
		if target.InputPath != "" || target.InputTransformer != nil || target.RoleArn != "" {
			failed = append(failed, map[string]any{"TargetId": target.ID, "ErrorCode": "NotImplementedException", "ErrorMessage": "advanced target options are not supported"})
			continue
		}
		if !isSupportedTargetARN(target.Arn) {
			failed = append(failed, map[string]any{"TargetId": target.ID, "ErrorCode": "NotImplementedException", "ErrorMessage": "only Lambda, SQS, SNS, and Step Functions targets are supported"})
			continue
		}
		record := targetRecord{
			Arn:          target.Arn,
			EventBusName: busName,
			ID:           target.ID,
			Input:        target.Input,
			RuleName:     input.Rule,
		}
		if err := s.putTarget(record); err != nil {
			return internal(err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"FailedEntries":    failed,
		"FailedEntryCount": len(failed),
	})
	return nil
}

func (s *Service) listTargetsByRule(w http.ResponseWriter, r *http.Request) error {
	var input listTargetsByRuleInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	busName := defaultBus(input.EventBusName)
	if _, err := s.loadRule(busName, input.Rule); err != nil {
		return err
	}
	targets, err := s.loadTargets(busName, input.Rule)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		entry := map[string]any{
			"Arn": target.Arn,
			"Id":  target.ID,
		}
		if target.Input != "" {
			entry["Input"] = target.Input
		}
		items = append(items, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Targets": items})
	return nil
}

func (s *Service) removeTargets(w http.ResponseWriter, r *http.Request) error {
	var input removeTargetsInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	busName := defaultBus(input.EventBusName)
	if _, err := s.loadRule(busName, input.Rule); err != nil {
		return err
	}
	for _, id := range input.IDs {
		if err := s.metadata.Delete(targetsBucket, targetStoreKey(busName, input.Rule, id)); err != nil {
			return internal(err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"FailedEntries":    []any{},
		"FailedEntryCount": 0,
	})
	return nil
}

func (s *Service) deleteRule(w http.ResponseWriter, r *http.Request) error {
	var input deleteRuleInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.Force {
		return notImplemented("force delete for managed rules is not supported")
	}
	busName := defaultBus(input.EventBusName)
	if _, err := s.loadRule(busName, input.Name); err != nil {
		return err
	}
	targets, err := s.loadTargets(busName, input.Name)
	if err != nil {
		return err
	}
	if len(targets) > 0 {
		return validation("remove targets from the rule before deleting it")
	}
	if err := s.metadata.Delete(rulesBucket, ruleStoreKey(busName, input.Name)); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Service) putEvents(w http.ResponseWriter, r *http.Request) error {
	var input putEventsInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	results := make([]map[string]any, 0, len(input.Entries))
	failedCount := 0
	for _, entry := range input.Entries {
		eventID, err := s.processEntry(entry)
		if err != nil {
			failedCount++
			results = append(results, map[string]any{
				"ErrorCode":    errorCode(err),
				"ErrorMessage": errorMessage(err),
			})
			continue
		}
		results = append(results, map[string]any{"EventId": eventID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Entries":          results,
		"FailedEntryCount": failedCount,
	})
	return nil
}

func (s *Service) createArchive(w http.ResponseWriter, r *http.Request) error {
	var input createArchiveInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.ArchiveName == "" || input.EventSourceArn == "" {
		return validation("ArchiveName and EventSourceArn are required")
	}
	if input.EventPattern != "" {
		var raw any
		if err := json.Unmarshal([]byte(input.EventPattern), &raw); err != nil {
			return validation("EventPattern must be valid JSON")
		}
	}
	record := archiveRecord{
		ArchiveArn:     archiveARN(input.ArchiveName),
		ArchiveName:    input.ArchiveName,
		CreatedAt:      s.now().UTC(),
		Description:    input.Description,
		EventPattern:   input.EventPattern,
		EventSourceArn: input.EventSourceArn,
		State:          "ENABLED",
	}
	if err := s.putArchive(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, archiveResponse(record))
	return nil
}

func (s *Service) describeArchive(w http.ResponseWriter, r *http.Request) error {
	var input describeArchiveInput
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.loadArchive(input.ArchiveName)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, archiveResponse(record))
	return nil
}

func (s *Service) listArchives(w http.ResponseWriter) error {
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(archivesBucket, "", func(_, v []byte) error {
		var record archiveRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, archiveResponse(record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["ArchiveName"].(string) < items[j]["ArchiveName"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"Archives": items})
	return nil
}

func (s *Service) processEntry(entry putEventsEntry) (string, error) {
	busName := defaultBus(entry.EventBusName)
	if _, err := s.loadEventBus(busName); err != nil {
		return "", err
	}
	if entry.Source == "" || entry.DetailType == "" || entry.Detail == "" {
		return "", validation("each event entry requires Source, DetailType, and Detail")
	}
	var detail any
	if err := json.Unmarshal([]byte(entry.Detail), &detail); err != nil {
		return "", validation("Detail must be valid JSON")
	}

	eventID := uuid.NewString()
	eventTime := s.now().UTC()
	if entry.Time != "" {
		if parsed, err := time.Parse(time.RFC3339, entry.Time); err == nil {
			eventTime = parsed.UTC()
		}
	}

	payload := map[string]any{
		"account":     accountID,
		"detail":      detail,
		"detail-type": entry.DetailType,
		"id":          eventID,
		"region":      region,
		"resources":   entry.Resources,
		"source":      entry.Source,
		"time":        eventTime.Format(time.RFC3339),
		"version":     "0",
	}
	rules, err := s.rulesForBus(busName)
	if err != nil {
		return "", err
	}
	for _, rule := range rules {
		if rule.State == "DISABLED" || !matchesRule(rule, payload) {
			continue
		}
		targets, err := s.loadTargets(busName, rule.Name)
		if err != nil {
			return "", err
		}
		for _, target := range targets {
			invokePayload, err := payloadBytes(target, payload)
			if err != nil {
				return "", err
			}
			if err := s.deliverTarget(context.Background(), target, invokePayload); err != nil {
				return "", err
			}
		}
	}

	return eventID, nil
}

func (s *Service) ensureDefaultBus() error {
	if _, err := s.loadEventBus(defaultBusName); err == nil {
		return nil
	}
	record := eventBusRecord{
		Arn:       eventBusARN(defaultBusName),
		CreatedAt: s.now().UTC(),
		Name:      defaultBusName,
	}
	if err := s.putEventBus(record); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) loadEventBus(name string) (eventBusRecord, error) {
	raw, err := s.metadata.Get(eventBusesBucket, name)
	if err != nil {
		return eventBusRecord{}, internal(err)
	}
	if raw == nil {
		return eventBusRecord{}, &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ResourceNotFoundException",
			Message:    "Event bus does not exist.",
		}
	}
	var record eventBusRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return eventBusRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putEventBus(record eventBusRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(eventBusesBucket, record.Name, raw)
}

func (s *Service) loadRule(busName, name string) (ruleRecord, error) {
	if err := validateName(name, "Name is required"); err != nil {
		return ruleRecord{}, err
	}
	raw, err := s.metadata.Get(rulesBucket, ruleStoreKey(busName, name))
	if err != nil {
		return ruleRecord{}, internal(err)
	}
	if raw == nil {
		return ruleRecord{}, &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "ResourceNotFoundException",
			Message:    "Rule does not exist.",
		}
	}
	var record ruleRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return ruleRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putRuleRecord(record ruleRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(rulesBucket, ruleStoreKey(record.EventBusName, record.Name), raw)
}

func (s *Service) rulesForBus(busName string) ([]ruleRecord, error) {
	rules := make([]ruleRecord, 0)
	if err := s.metadata.Scan(rulesBucket, busName+"|", func(_, v []byte) error {
		var record ruleRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		rules = append(rules, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
	return rules, nil
}

func (s *Service) putTarget(record targetRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(targetsBucket, targetStoreKey(record.EventBusName, record.RuleName, record.ID), raw)
}

func (s *Service) putArchive(record archiveRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(archivesBucket, record.ArchiveName, raw)
}

func (s *Service) loadArchive(name string) (archiveRecord, error) {
	raw, err := s.metadata.Get(archivesBucket, name)
	if err != nil {
		return archiveRecord{}, internal(err)
	}
	if raw == nil {
		return archiveRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ResourceNotFoundException", Message: "Archive does not exist."}
	}
	var record archiveRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return archiveRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadTargets(busName, ruleName string) ([]targetRecord, error) {
	targets := make([]targetRecord, 0)
	if err := s.metadata.Scan(targetsBucket, busName+"|"+ruleName+"|", func(_, v []byte) error {
		var record targetRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		targets = append(targets, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets, nil
}

func ruleStoreKey(busName, name string) string {
	return busName + "|" + name
}

func targetStoreKey(busName, ruleName, id string) string {
	return busName + "|" + ruleName + "|" + id
}

func archiveARN(name string) string {
	return fmt.Sprintf("arn:aws:events:%s:%s:archive/%s", region, accountID, name)
}

func eventBusARN(name string) string {
	return fmt.Sprintf("arn:aws:events:%s:%s:event-bus/%s", region, accountID, name)
}

func ruleARN(busName, name string) string {
	if busName == defaultBusName {
		return fmt.Sprintf("arn:aws:events:%s:%s:rule/%s", region, accountID, name)
	}
	return fmt.Sprintf("arn:aws:events:%s:%s:rule/%s/%s", region, accountID, busName, name)
}

func defaultBus(name string) string {
	if name == "" {
		return defaultBusName
	}
	return name
}

func validateName(name, emptyMessage string) error {
	if name == "" {
		return validation(emptyMessage)
	}
	if !nameRe.MatchString(name) {
		return validation("Name contains invalid characters")
	}
	return nil
}

func payloadBytes(target targetRecord, payload map[string]any) ([]byte, error) {
	if target.Input != "" {
		return []byte(target.Input), nil
	}
	return json.Marshal(payload)
}

func isSupportedTargetARN(arn string) bool {
	return strings.Contains(arn, ":function:") || strings.Contains(arn, ":sqs:") || strings.Contains(arn, ":sns:") || strings.Contains(arn, ":states:")
}

func (s *Service) deliverTarget(ctx context.Context, target targetRecord, payload []byte) error {
	switch {
	case strings.Contains(target.Arn, ":function:"):
		if s.lambda == nil {
			return nil
		}
		_, err := s.lambda.InvokeByName(ctx, lambdaNameFromARN(target.Arn), payload)
		return err
	case strings.Contains(target.Arn, ":sqs:"):
		if s.sqs == nil {
			return nil
		}
		return s.sqs.SendMessageToARN(target.Arn, string(payload))
	case strings.Contains(target.Arn, ":sns:"):
		if s.sns == nil {
			return nil
		}
		return s.sns.PublishToTopic(target.Arn, string(payload))
	case strings.Contains(target.Arn, ":states:"):
		if s.stepFunctions == nil {
			return nil
		}
		return s.stepFunctions.StartExecutionByARN(ctx, target.Arn, string(payload))
	default:
		return nil
	}
}

func archiveResponse(record archiveRecord) map[string]any {
	return map[string]any{
		"ArchiveArn":     record.ArchiveArn,
		"ArchiveName":    record.ArchiveName,
		"CreationTime":   record.CreatedAt.Format(time.RFC3339),
		"Description":    record.Description,
		"EventPattern":   record.EventPattern,
		"EventSourceArn": record.EventSourceArn,
		"State":          record.State,
	}
}

func ruleResponse(record ruleRecord) map[string]any {
	resp := map[string]any{
		"Arn":          record.Arn,
		"Description":  record.Description,
		"EventBusName": record.EventBusName,
		"EventPattern": record.EventPattern,
		"Name":         record.Name,
		"State":        record.State,
	}
	if record.ScheduleExpression != "" {
		resp["ScheduleExpression"] = record.ScheduleExpression
	}
	return resp
}

func (s *Service) runScheduler() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		_ = s.processScheduledRules()
	}
}

func (s *Service) processScheduledRules() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureDefaultBus(); err != nil {
		return err
	}
	rules := make([]ruleRecord, 0)
	if err := s.metadata.Scan(rulesBucket, "", func(_, v []byte) error {
		var rule ruleRecord
		if err := json.Unmarshal(v, &rule); err != nil {
			return nil
		}
		rules = append(rules, rule)
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Arn < rules[j].Arn })
	now := s.now().UTC()
	for _, rule := range rules {
		if rule.State != "ENABLED" || rule.ScheduleExpression == "" {
			continue
		}
		interval, _, err := parseScheduleExpression(rule.ScheduleExpression)
		if err != nil {
			continue
		}
		if !rule.LastTriggeredAt.IsZero() && now.Sub(rule.LastTriggeredAt) < interval {
			continue
		}
		targets, err := s.loadTargets(rule.EventBusName, rule.Name)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"account":     accountID,
			"detail":      map[string]any{},
			"detail-type": "Scheduled Event",
			"id":          uuid.NewString(),
			"region":      region,
			"resources":   []string{rule.Arn},
			"source":      "aws.events",
			"time":        now.Format(time.RFC3339),
			"version":     "0",
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		for _, target := range targets {
			if err := s.deliverTarget(context.Background(), target, body); err != nil {
				return err
			}
		}
		rule.LastTriggeredAt = now
		if err := s.putRuleRecord(rule); err != nil {
			return internal(err)
		}
	}
	return nil
}

func matchesRule(rule ruleRecord, event map[string]any) bool {
	if rule.EventPattern == "" {
		return false
	}
	var pattern map[string]any
	if err := json.Unmarshal([]byte(rule.EventPattern), &pattern); err != nil {
		return false
	}
	return matchObject(pattern, event)
}

func matchObject(pattern map[string]any, event map[string]any) bool {
	for key, expected := range pattern {
		actual, ok := event[key]
		if !ok {
			if expectsMissing(expected) {
				continue
			}
			return false
		}
		if !matchValue(expected, actual) {
			return false
		}
	}
	return true
}

func expectsMissing(expected any) bool {
	items, ok := expected.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if obj, ok := item.(map[string]any); ok {
			if exists, ok := obj["exists"].(bool); ok && !exists {
				return true
			}
		}
	}
	return false
}

func matchValue(expected, actual any) bool {
	switch typed := expected.(type) {
	case []any:
		for _, candidate := range typed {
			if matchValue(candidate, actual) {
				return true
			}
		}
		return false
	case map[string]any:
		if value, ok := typed["prefix"]; ok {
			actualString, ok := actual.(string)
			prefix, ok2 := value.(string)
			return ok && ok2 && strings.HasPrefix(actualString, prefix)
		}
		if value, ok := typed["anything-but"]; ok {
			switch typedValue := value.(type) {
			case string:
				actualString, ok := actual.(string)
				return ok && actualString != typedValue
			case []any:
				for _, candidate := range typedValue {
					if matchValue(candidate, actual) {
						return false
					}
				}
				return true
			}
		}
		if value, ok := typed["exists"]; ok {
			exists, ok2 := value.(bool)
			return ok2 && exists
		}
		if value, ok := typed["numeric"]; ok {
			clauses, ok := value.([]any)
			if !ok {
				return false
			}
			actualFloat, ok := actual.(float64)
			if !ok {
				return false
			}
			return matchNumeric(clauses, actualFloat)
		}
		actualMap, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		return matchObject(typed, actualMap)
	case string:
		actualString, ok := actual.(string)
		return ok && actualString == typed
	case bool:
		actualBool, ok := actual.(bool)
		return ok && actualBool == typed
	case float64:
		actualFloat, ok := actual.(float64)
		return ok && actualFloat == typed
	default:
		return false
	}
}

func matchNumeric(clauses []any, actual float64) bool {
	for idx := 0; idx+1 < len(clauses); idx += 2 {
		operator, ok := clauses[idx].(string)
		if !ok {
			return false
		}
		value, ok := clauses[idx+1].(float64)
		if !ok {
			return false
		}
		switch operator {
		case ">":
			if !(actual > value) {
				return false
			}
		case ">=":
			if !(actual >= value) {
				return false
			}
		case "<":
			if !(actual < value) {
				return false
			}
		case "<=":
			if !(actual <= value) {
				return false
			}
		case "=":
			if actual != value {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func parseScheduleExpression(expr string) (time.Duration, string, error) {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "rate(") && strings.HasSuffix(expr, ")") {
		body := strings.TrimSuffix(strings.TrimPrefix(expr, "rate("), ")")
		parts := strings.Fields(body)
		if len(parts) != 2 {
			return 0, "", validation("ScheduleExpression is invalid")
		}
		value, err := strconv.Atoi(parts[0])
		if err != nil || value <= 0 {
			return 0, "", validation("ScheduleExpression is invalid")
		}
		unit := strings.TrimSpace(parts[1])
		switch unit {
		case "second", "seconds":
			return time.Duration(value) * time.Second, unit, nil
		case "minute", "minutes":
			return time.Duration(value) * time.Minute, unit, nil
		default:
			return 0, "", validation("ScheduleExpression is invalid")
		}
	}
	if strings.HasPrefix(expr, "cron(") && strings.HasSuffix(expr, ")") {
		return time.Second, "cron", nil
	}
	return 0, "", validation("ScheduleExpression is invalid")
}

func lambdaNameFromARN(arn string) string {
	parts := strings.Split(arn, ":function:")
	if len(parts) != 2 {
		return arn
	}
	return parts[1]
}

func decodeJSON(r *http.Request, target any) error {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
}

func errorCode(err error) string {
	var apiErr *apierror.Error
	if ok := As(err, &apiErr); ok {
		return apiErr.Code
	}
	return "InternalFailure"
}

func errorMessage(err error) string {
	var apiErr *apierror.Error
	if ok := As(err, &apiErr); ok {
		return apiErr.Message
	}
	return err.Error()
}

func As(err error, target **apierror.Error) bool {
	apiErr, ok := err.(*apierror.Error)
	if !ok {
		return false
	}
	*target = apiErr
	return true
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationException", Message: message}
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
