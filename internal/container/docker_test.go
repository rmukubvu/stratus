package container

import (
	"reflect"
	"testing"

	lambdasvc "github.com/stratus/internal/services/lambda"
)

func TestRuntimeExtraHostsAddsHostGatewayForDockerInternalEndpoints(t *testing.T) {
	spec := lambdasvc.FunctionSpec{
		Environment: map[string]string{
			"EMULATOR_ENDPOINT": "http://host.docker.internal:4566",
			"QUEUE_URL":         "http://host.docker.internal:4566/000000000000/jobs",
		},
	}

	got := runtimeExtraHosts(spec)
	want := []string{"host.docker.internal:host-gateway"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtimeExtraHosts() = %v, want %v", got, want)
	}
}

func TestRuntimeExtraHostsSkipsHostGatewayWhenNotNeeded(t *testing.T) {
	spec := lambdasvc.FunctionSpec{
		Environment: map[string]string{
			"EMULATOR_ENDPOINT": "http://127.0.0.1:4566",
		},
	}

	if got := runtimeExtraHosts(spec); got != nil {
		t.Fatalf("runtimeExtraHosts() = %v, want nil", got)
	}
}

func TestSliceInvocationKeepsOnlyTheRequestedInvocation(t *testing.T) {
	output := "init noise\n" +
		"START RequestId: aaa Version: $LATEST\nfirst\nEND RequestId: aaa\nREPORT RequestId: aaa\tDuration: 1 ms\n" +
		"START RequestId: bbb Version: $LATEST\nsecond\nEND RequestId: bbb\nREPORT RequestId: bbb\tDuration: 2 ms\n"

	got, complete := sliceInvocation(output, "bbb")
	if !complete {
		t.Fatalf("sliceInvocation() complete = false, want true")
	}
	want := "START RequestId: bbb Version: $LATEST\nsecond\nEND RequestId: bbb\nREPORT RequestId: bbb\tDuration: 2 ms\n"
	if got != want {
		t.Fatalf("sliceInvocation() = %q, want %q", got, want)
	}
}

func TestSliceInvocationReportsIncompleteUntilReport(t *testing.T) {
	got, complete := sliceInvocation("START RequestId: ccc Version: $LATEST\nworking\n", "ccc")
	if complete {
		t.Fatalf("sliceInvocation() complete = true before REPORT")
	}
	if got != "START RequestId: ccc Version: $LATEST\nworking\n" {
		t.Fatalf("sliceInvocation() = %q", got)
	}
}
