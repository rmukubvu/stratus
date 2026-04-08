package stepfunctions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/store"
)

const (
	stateMachinesBucket = "stepfunctions-state-machines"
	executionsBucket    = "stepfunctions-executions"
	accountID           = "000000000000"
	region              = "us-east-1"
)

type Service struct {
	metadata store.Store
	lambda   *lambdasvc.Service
	now      func() time.Time
	mu       sync.Mutex
}

type CreateStateMachineInput struct {
	Definition string
	Name       string
	RoleArn    string
	Type       string
}

type stateMachineRecord struct {
	CreatedAt  time.Time `json:"created_at"`
	Definition string    `json:"definition"`
	Name       string    `json:"name"`
	RoleArn    string    `json:"role_arn"`
	Type       string    `json:"type"`
}

type executionRecord struct {
	ExecutionArn    string    `json:"execution_arn"`
	Input           string    `json:"input"`
	Name            string    `json:"name"`
	Output          string    `json:"output,omitempty"`
	StartDate       time.Time `json:"start_date"`
	StateMachineArn string    `json:"state_machine_arn"`
	Status          string    `json:"status"`
	StopDate        time.Time `json:"stop_date"`
}

type stateMachineDefinition struct {
	StartAt string                    `json:"StartAt"`
	States  map[string]definitionNode `json:"States"`
}

type definitionNode struct {
	Branches   []stateMachineDefinition `json:"Branches"`
	Catch      []catcher                `json:"Catch"`
	Choices    []choiceRule             `json:"Choices"`
	Default    string                   `json:"Default"`
	End        bool                     `json:"End"`
	ItemsPath  string                   `json:"ItemsPath"`
	Iterator   *stateMachineDefinition  `json:"Iterator"`
	Next       string                   `json:"Next"`
	Resource   string                   `json:"Resource"`
	Result     json.RawMessage          `json:"Result"`
	ResultPath string                   `json:"ResultPath"`
	Retry      []retrier                `json:"Retry"`
	Type       string                   `json:"Type"`
}

type choiceRule struct {
	Variable      string   `json:"Variable"`
	StringEquals  string   `json:"StringEquals"`
	BooleanEquals *bool    `json:"BooleanEquals"`
	NumericEquals *float64 `json:"NumericEquals"`
	Next          string   `json:"Next"`
}

type retrier struct {
	ErrorEquals []string `json:"ErrorEquals"`
	MaxAttempts int      `json:"MaxAttempts"`
}

type catcher struct {
	ErrorEquals []string `json:"ErrorEquals"`
	Next        string   `json:"Next"`
	ResultPath  string   `json:"ResultPath"`
}

func NewService(metadata store.Store, lambda *lambdasvc.Service) *Service {
	return &Service{metadata: metadata, lambda: lambda, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateStateMachine":
		return s.createStateMachine(w, r)
	case "ListStateMachines":
		return s.listStateMachines(w)
	case "DescribeStateMachine":
		return s.describeStateMachine(w, r)
	case "StartExecution":
		return s.startExecution(w, r)
	case "DescribeExecution":
		return s.describeExecution(w, r)
	case "DeleteStateMachine":
		return s.deleteStateMachine(w, r)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "stepfunctions operation is not implemented"}
	}
}

func (s *Service) createStateMachine(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Definition string `json:"definition"`
		Name       string `json:"name"`
		RoleArn    string `json:"roleArn"`
		Type       string `json:"type"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.Name == "" || input.Definition == "" || input.RoleArn == "" {
		return validation("name, definition, and roleArn are required")
	}
	if input.Type == "" {
		input.Type = "STANDARD"
	}
	if input.Type != "STANDARD" {
		return notImplemented("only STANDARD state machines are supported")
	}
	if _, err := parseDefinition(input.Definition); err != nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidDefinition", Message: err.Error()}
	}
	if _, err := s.loadStateMachineByName(input.Name); err == nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "StateMachineAlreadyExists", Message: "State machine already exists"}
	}
	record := stateMachineRecord{
		CreatedAt:  s.now().UTC(),
		Definition: input.Definition,
		Name:       input.Name,
		RoleArn:    input.RoleArn,
		Type:       input.Type,
	}
	if err := s.putStateMachine(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"creationDate":    formatTime(record.CreatedAt),
		"stateMachineArn": stateMachineARN(record.Name),
	})
	return nil
}

func (s *Service) CreateStateMachine(input CreateStateMachineInput) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.Name == "" || input.Definition == "" || input.RoleArn == "" {
		return "", validation("name, definition, and roleArn are required")
	}
	if input.Type == "" {
		input.Type = "STANDARD"
	}
	if _, err := parseDefinition(input.Definition); err != nil {
		return "", &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidDefinition", Message: err.Error()}
	}
	if _, err := s.loadStateMachineByName(input.Name); err == nil {
		return stateMachineARN(input.Name), nil
	}
	record := stateMachineRecord{
		CreatedAt:  s.now().UTC(),
		Definition: input.Definition,
		Name:       input.Name,
		RoleArn:    input.RoleArn,
		Type:       input.Type,
	}
	if err := s.putStateMachine(record); err != nil {
		return "", internal(err)
	}
	return stateMachineARN(input.Name), nil
}

func (s *Service) DeleteStateMachineByARN(arn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, err := s.resolveStateMachine(arn)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(stateMachinesBucket, record.Name); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(executionsBucket, executionARN(record.Name, "")); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) StartExecutionByARN(ctx context.Context, arn, input string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, err := s.resolveStateMachine(arn)
	if err != nil {
		return err
	}
	definition, err := parseDefinition(record.Definition)
	if err != nil {
		return err
	}
	name := uuid.NewString()
	start := s.now().UTC()
	payload := map[string]any{}
	if strings.TrimSpace(input) != "" {
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			return validation("input must be valid JSON")
		}
	}
	output, execErr := s.executeDefinition(definition, payload)
	execution := executionRecord{
		ExecutionArn:    executionARN(record.Name, name),
		Input:           input,
		Name:            name,
		StartDate:       start,
		StateMachineArn: arn,
		Status:          "SUCCEEDED",
		StopDate:        s.now().UTC(),
	}
	if execErr != nil {
		execution.Status = "FAILED"
		execution.Output = marshalString(map[string]any{"error": execErr.Error()})
	} else {
		execution.Output = marshalString(output)
	}
	raw, err := json.Marshal(execution)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(executionsBucket, execution.ExecutionArn, raw); err != nil {
		return internal(err)
	}
	_ = ctx
	return nil
}

func (s *Service) listStateMachines(w http.ResponseWriter) error {
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(stateMachinesBucket, "", func(_, v []byte) error {
		var record stateMachineRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"creationDate":    formatTime(record.CreatedAt),
			"name":            record.Name,
			"stateMachineArn": stateMachineARN(record.Name),
			"type":            record.Type,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stateMachines": items})
	return nil
}

func (s *Service) describeStateMachine(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		StateMachineArn string `json:"stateMachineArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveStateMachine(input.StateMachineArn)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"creationDate":    formatTime(record.CreatedAt),
		"definition":      record.Definition,
		"name":            record.Name,
		"roleArn":         record.RoleArn,
		"stateMachineArn": stateMachineARN(record.Name),
		"status":          "ACTIVE",
		"type":            record.Type,
	})
	return nil
}

func (s *Service) startExecution(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Input           string `json:"input"`
		Name            string `json:"name"`
		StateMachineArn string `json:"stateMachineArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveStateMachine(input.StateMachineArn)
	if err != nil {
		return err
	}
	if input.Name == "" {
		input.Name = uuid.NewString()
	}
	start := s.now().UTC()
	executionArn := executionARN(record.Name, input.Name)
	output, status, runErr := s.execute(record, input.Input)
	if runErr != nil {
		status = "FAILED"
		output = fmt.Sprintf(`{"error":%q}`, runErr.Error())
	}
	execRecord := executionRecord{
		ExecutionArn:    executionArn,
		Input:           defaultJSON(input.Input),
		Name:            input.Name,
		Output:          output,
		StartDate:       start,
		StateMachineArn: stateMachineARN(record.Name),
		Status:          status,
		StopDate:        s.now().UTC(),
	}
	if err := s.putExecution(execRecord); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"executionArn": executionArn,
		"startDate":    formatTime(start),
	})
	return nil
}

func (s *Service) describeExecution(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ExecutionArn string `json:"executionArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.loadExecution(input.ExecutionArn)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"executionArn":    record.ExecutionArn,
		"input":           record.Input,
		"name":            record.Name,
		"output":          record.Output,
		"startDate":       formatTime(record.StartDate),
		"stateMachineArn": record.StateMachineArn,
		"status":          record.Status,
		"stopDate":        formatTime(record.StopDate),
	})
	return nil
}

func (s *Service) deleteStateMachine(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		StateMachineArn string `json:"stateMachineArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveStateMachine(input.StateMachineArn)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(stateMachinesBucket, record.Name); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(executionsBucket, stateMachineARN(record.Name)+":execution:"); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) execute(record stateMachineRecord, input string) (string, string, error) {
	definition, err := parseDefinition(record.Definition)
	if err != nil {
		return "", "FAILED", err
	}
	var payload any = map[string]any{}
	if strings.TrimSpace(input) != "" {
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			return "", "FAILED", err
		}
	}
	output, err := s.executeDefinition(definition, payload)
	if err != nil {
		return "", "FAILED", err
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return "", "FAILED", err
	}
	return string(raw), "SUCCEEDED", nil
}

func (s *Service) executeDefinition(definition stateMachineDefinition, payload any) (any, error) {
	current := definition.StartAt
	for {
		state, ok := definition.States[current]
		if !ok {
			return nil, fmt.Errorf("state %q not found", current)
		}
		nextState := state.Next
		previousPayload := payload
		var err error

		switch state.Type {
		case "Pass":
			payload, err = executePassState(state, payload)
		case "Choice":
			nextState, err = executeChoiceState(state, payload)
		case "Task":
			payload, err = s.executeTaskState(state, payload)
		case "Map":
			payload, err = s.executeMapState(state, payload)
		case "Parallel":
			payload, err = s.executeParallelState(state, payload)
		case "Succeed":
			return payload, nil
		case "Fail":
			return nil, fmt.Errorf("execution entered Fail state")
		default:
			return nil, fmt.Errorf("state type %q is not supported", state.Type)
		}
		if err != nil {
			nextState, payload, err = handleCatchers(state, previousPayload, err)
			if err != nil {
				return nil, err
			}
		}
		if state.End {
			return payload, nil
		}
		if nextState == "" {
			return nil, fmt.Errorf("state %q has no Next or End", current)
		}
		current = nextState
	}
}

func executePassState(state definitionNode, payload any) (any, error) {
	result := payload
	if len(state.Result) > 0 {
		if err := json.Unmarshal(state.Result, &result); err != nil {
			return nil, err
		}
	}
	return applyResultPath(payload, result, state.ResultPath)
}

func executeChoiceState(state definitionNode, payload any) (string, error) {
	for _, choice := range state.Choices {
		if matchChoice(choice, payload) {
			return choice.Next, nil
		}
	}
	if state.Default != "" {
		return state.Default, nil
	}
	return "", fmt.Errorf("no Choice rule matched and no Default was provided")
}

func (s *Service) executeTaskState(state definitionNode, payload any) (any, error) {
	if s.lambda == nil {
		return nil, fmt.Errorf("lambda runtime is not configured")
	}
	if !strings.Contains(state.Resource, ":function:") {
		return nil, fmt.Errorf("only Lambda task resources are supported")
	}
	inputBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var lastErr error
	maxAttempts := 1
	if len(state.Retry) > 0 && state.Retry[0].MaxAttempts > 0 {
		maxAttempts = state.Retry[0].MaxAttempts
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result, err := s.lambda.InvokeByName(context.Background(), lambdaName(state.Resource), inputBytes)
		if err == nil && result.FunctionError == "" {
			var out any
			if err := json.Unmarshal(result.Payload, &out); err != nil {
				return nil, err
			}
			return applyResultPath(payload, out, state.ResultPath)
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("%s", result.Payload)
		}
	}
	return nil, lastErr
}

func (s *Service) executeMapState(state definitionNode, payload any) (any, error) {
	if state.Iterator == nil {
		return nil, fmt.Errorf("Map state requires Iterator")
	}
	items, err := arrayFromPath(payload, state.ItemsPath)
	if err != nil {
		return nil, err
	}
	results := make([]any, 0, len(items))
	for _, item := range items {
		out, err := s.executeDefinition(*state.Iterator, item)
		if err != nil {
			return nil, err
		}
		results = append(results, out)
	}
	return applyResultPath(payload, results, state.ResultPath)
}

func (s *Service) executeParallelState(state definitionNode, payload any) (any, error) {
	results := make([]any, 0, len(state.Branches))
	for _, branch := range state.Branches {
		out, err := s.executeDefinition(branch, payload)
		if err != nil {
			return nil, err
		}
		results = append(results, out)
	}
	return applyResultPath(payload, results, state.ResultPath)
}

func parseDefinition(raw string) (stateMachineDefinition, error) {
	var definition stateMachineDefinition
	if err := json.Unmarshal([]byte(raw), &definition); err != nil {
		return stateMachineDefinition{}, err
	}
	if definition.StartAt == "" || len(definition.States) == 0 {
		return stateMachineDefinition{}, fmt.Errorf("state machine definition requires StartAt and States")
	}
	return definition, nil
}

func handleCatchers(state definitionNode, payload any, err error) (string, any, error) {
	if len(state.Catch) == 0 {
		return "", nil, err
	}
	catchPayload := map[string]any{"Error": err.Error(), "Cause": err.Error()}
	for _, catcher := range state.Catch {
		if catcher.Next == "" {
			continue
		}
		out, applyErr := applyResultPath(payload, catchPayload, catcher.ResultPath)
		if applyErr != nil {
			return "", nil, applyErr
		}
		return catcher.Next, out, nil
	}
	return "", nil, err
}

func matchChoice(choice choiceRule, payload any) bool {
	value, ok := valueAtPath(payload, choice.Variable)
	if !ok {
		return false
	}
	if choice.StringEquals != "" {
		actual, ok := value.(string)
		return ok && actual == choice.StringEquals
	}
	if choice.BooleanEquals != nil {
		actual, ok := value.(bool)
		return ok && actual == *choice.BooleanEquals
	}
	if choice.NumericEquals != nil {
		actual, ok := value.(float64)
		return ok && actual == *choice.NumericEquals
	}
	return false
}

func valueAtPath(payload any, path string) (any, bool) {
	if path == "" || path == "$" {
		return payload, true
	}
	if !strings.HasPrefix(path, "$.") {
		return nil, false
	}
	current := payload
	for _, part := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		asMap, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = asMap[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func arrayFromPath(payload any, path string) ([]any, error) {
	value, ok := valueAtPath(payload, path)
	if !ok {
		return nil, fmt.Errorf("ItemsPath %q not found", path)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("ItemsPath %q is not an array", path)
	}
	return items, nil
}

func applyResultPath(original any, result any, resultPath string) (any, error) {
	if resultPath == "" || resultPath == "$" {
		return result, nil
	}
	if !strings.HasPrefix(resultPath, "$.") {
		return nil, fmt.Errorf("ResultPath %q is not supported", resultPath)
	}
	base, ok := original.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("ResultPath requires object payload")
	}
	out := cloneMapAny(base)
	parts := strings.Split(strings.TrimPrefix(resultPath, "$."), ".")
	current := out
	for idx, part := range parts {
		if idx == len(parts)-1 {
			current[part] = result
			return out, nil
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	return out, nil
}

func (s *Service) loadStateMachineByName(name string) (stateMachineRecord, error) {
	raw, err := s.metadata.Get(stateMachinesBucket, name)
	if err != nil {
		return stateMachineRecord{}, internal(err)
	}
	if raw == nil {
		return stateMachineRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "StateMachineDoesNotExist", Message: "State machine does not exist"}
	}
	var record stateMachineRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return stateMachineRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) resolveStateMachine(arn string) (stateMachineRecord, error) {
	if arn == "" {
		return stateMachineRecord{}, validation("stateMachineArn is required")
	}
	parts := strings.Split(arn, ":stateMachine:")
	if len(parts) != 2 {
		return stateMachineRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidArn", Message: "State machine ARN is invalid"}
	}
	return s.loadStateMachineByName(parts[1])
}

func (s *Service) putStateMachine(record stateMachineRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(stateMachinesBucket, record.Name, raw)
}

func (s *Service) putExecution(record executionRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(executionsBucket, record.ExecutionArn, raw)
}

func (s *Service) loadExecution(arn string) (executionRecord, error) {
	raw, err := s.metadata.Get(executionsBucket, arn)
	if err != nil {
		return executionRecord{}, internal(err)
	}
	if raw == nil {
		return executionRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ExecutionDoesNotExist", Message: "Execution does not exist"}
	}
	var record executionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return executionRecord{}, internal(err)
	}
	return record, nil
}

func stateMachineARN(name string) string {
	return fmt.Sprintf("arn:aws:states:%s:%s:stateMachine:%s", region, accountID, name)
}

func executionARN(stateMachineName, name string) string {
	return fmt.Sprintf("arn:aws:states:%s:%s:execution:%s:%s", region, accountID, stateMachineName, name)
}

func lambdaName(arn string) string {
	parts := strings.Split(arn, ":function:")
	if len(parts) != 2 {
		return arn
	}
	return parts[1]
}

func defaultJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return raw
}

func decodeJSON(r *http.Request, out any) error {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
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

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func marshalString(payload any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func cloneMapAny(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
