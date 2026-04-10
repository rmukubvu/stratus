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
