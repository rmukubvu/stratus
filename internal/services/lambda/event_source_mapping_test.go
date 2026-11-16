package lambda

import (
	"encoding/json"
	"net/http"
	"testing"
)

func decodeMapping(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode mapping: %v\n%s", err, body)
	}
	return out
}

func TestCreateSQSMappingDefaultsLikeAWS(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "worker")

	rec, err := call(t, svc, http.MethodPost, "/2015-03-31/event-source-mappings", "CreateEventSourceMapping", defaultMappingBody(""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	out := decodeMapping(t, rec.Body.Bytes())
	if out["BatchSize"] != float64(defaultSQSBatchSize) || out["State"] != "Enabled" || out["MaximumBatchingWindowInSeconds"] != float64(0) {
		t.Fatalf("unexpected defaults: %v", out)
	}
	if types, ok := out["FunctionResponseTypes"].([]any); !ok || len(types) != 0 {
		t.Fatalf("FunctionResponseTypes = %v, want []", out["FunctionResponseTypes"])
	}
}

func TestCreateMappingValidatesSQSLimits(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "worker")

	for name, extra := range map[string]string{
		"batch above ten without a window": `,"BatchSize":11`,
		"batch above the maximum":          `,"BatchSize":10001,"MaximumBatchingWindowInSeconds":1`,
		"unknown response type":            `,"FunctionResponseTypes":["Everything"]`,
		"window above five minutes":        `,"MaximumBatchingWindowInSeconds":301`,
		"concurrency below two":            `,"ScalingConfig":{"MaximumConcurrency":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := call(t, svc, http.MethodPost, "/2015-03-31/event-source-mappings", "CreateEventSourceMapping", defaultMappingBody(extra))
			if errorCode(err) != "InvalidParameterValueException" {
				t.Fatalf("err = %v, want InvalidParameterValueException", err)
			}
		})
	}
	if _, err := call(t, svc, http.MethodPost, "/2015-03-31/event-source-mappings", "CreateEventSourceMapping", defaultMappingBody(`,"BatchSize":100,"MaximumBatchingWindowInSeconds":5`)); err != nil {
		t.Fatalf("a large batch with a window is valid: %v", err)
	}
}

func TestUpdateEventSourceMappingChangesOnlyWhatItNames(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "worker")
	provisionTestFunction(t, svc, "worker-v2")
	mapping := createSQSMapping(t, svc, defaultMappingBody(`,"BatchSize":5`))

	path := "/2015-03-31/event-source-mappings/" + mapping.UUID
	rec, err := call(t, svc, http.MethodPut, path, "UpdateEventSourceMapping",
		`{"Enabled":false,"FunctionResponseTypes":["ReportBatchItemFailures"],"ScalingConfig":{"MaximumConcurrency":3}}`)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	out := decodeMapping(t, rec.Body.Bytes())
	if rec.Code != http.StatusAccepted || out["State"] != "Disabled" || out["BatchSize"] != float64(5) {
		t.Fatalf("unexpected update response %d: %v", rec.Code, out)
	}
	stored, _ := svc.loadEventSourceMapping(mapping.UUID)
	if stored.Enabled || !stored.reportsItemFailures() || stored.pollers() != 3 {
		t.Fatalf("update not stored: %+v", stored)
	}

	if _, err := call(t, svc, http.MethodPut, path, "UpdateEventSourceMapping", `{"FunctionName":"worker-v2","Enabled":true,"FunctionResponseTypes":[]}`); err != nil {
		t.Fatalf("retarget: %v", err)
	}
	stored, _ = svc.loadEventSourceMapping(mapping.UUID)
	if stored.FunctionName != "worker-v2" || !stored.Enabled || stored.reportsItemFailures() {
		t.Fatalf("retarget not stored: %+v", stored)
	}

	if _, err := call(t, svc, http.MethodPut, path, "UpdateEventSourceMapping", `{"BatchSize":50}`); errorCode(err) != "InvalidParameterValueException" {
		t.Fatalf("an invalid update should be rejected, got %v", err)
	}
	if _, err := call(t, svc, http.MethodPut, "/2015-03-31/event-source-mappings/missing", "UpdateEventSourceMapping", `{}`); errorCode(err) != "ResourceNotFoundException" {
		t.Fatalf("updating a missing mapping: %v", err)
	}
}

func TestStreamMappingsKeepTheirDefaults(t *testing.T) {
	svc := newTestService(t)
	provisionTestFunction(t, svc, "worker")
	mapping, err := svc.createEventSourceMappingRecord("worker", "arn:aws:kinesis:us-east-1:000000000000:stream/clicks", 0, "LATEST", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if mapping.BatchSize != 1 {
		t.Fatalf("stream batch size = %d, want the existing default of 1", mapping.BatchSize)
	}
	if _, err := svc.createEventSourceMappingRecord("worker", "arn:aws:kinesis:us-east-1:000000000000:stream/clicks", 0, "", nil); errorCode(err) != "InvalidParameterValueException" {
		t.Fatalf("a stream needs a starting position, got %v", err)
	}
}
