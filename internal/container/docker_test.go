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

func TestRuntimeImageForNodejs20(t *testing.T) {
	got, err := runtimeImageFor("nodejs20.x")
	if err != nil {
		t.Fatalf("runtimeImageFor() error = %v", err)
	}
	if got != runtimeImageNodejs20 {
		t.Fatalf("runtimeImageFor() = %q, want %q", got, runtimeImageNodejs20)
	}
}

func TestRuntimeLaunchConfigForNodejs20(t *testing.T) {
	got, err := runtimeLaunchConfigFor("nodejs20.x", "index.handler", "demo")
	if err != nil {
		t.Fatalf("runtimeLaunchConfigFor() error = %v", err)
	}
	if got.PathEnvKey != "NODE_PATH" {
		t.Fatalf("runtimeLaunchConfigFor().PathEnvKey = %q, want NODE_PATH", got.PathEnvKey)
	}
	if len(got.Entrypoint) != 2 || got.Entrypoint[0] != "node" {
		t.Fatalf("unexpected entrypoint: %v", got.Entrypoint)
	}
}
