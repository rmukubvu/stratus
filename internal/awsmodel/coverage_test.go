package awsmodel

import (
	"slices"
	"testing"
)

func TestLookupTellsImplementedFromMissingFromUnknown(t *testing.T) {
	cases := []struct {
		service, operation string
		want               Support
	}{
		{"sqs", "SendMessage", Implemented},
		{"sqs", "AddPermission", NotImplemented},
		{"sqs", "SendMessages", UnknownOperation},
		{"glacier", "UploadArchive", UnknownService},
	}
	for _, c := range cases {
		if got := Lookup(c.service, c.operation); got != c.want {
			t.Errorf("Lookup(%s, %s) = %s, want %s", c.service, c.operation, got, c.want)
		}
	}
}

func TestEveryCoveredOperationIsOneTheModelDefines(t *testing.T) {
	for service, ops := range loadCoverage() {
		svc, err := Load(service)
		if err != nil {
			t.Fatalf("coverage.json names %q, which has no model", service)
		}
		for op := range ops {
			if _, _, ok := svc.Operation(op); !ok {
				t.Errorf("coverage.json lists %s.%s, which the model does not define; regenerate it", service, op)
			}
		}
	}
}

func TestCoverageSplitsEveryOperation(t *testing.T) {
	got, err := Coverage("sqs")
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := Load("sqs")
	if len(got.Implemented)+len(got.NotImplemented) != len(svc.OperationNames()) {
		t.Errorf("split %d+%d, model has %d", len(got.Implemented), len(got.NotImplemented), len(svc.OperationNames()))
	}
	if !slices.Contains(got.Implemented, "ReceiveMessage") {
		t.Errorf("Implemented = %v, want ReceiveMessage", got.Implemented)
	}
}
