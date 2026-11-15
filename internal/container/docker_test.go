package container

import (
	"reflect"
	"testing"
)

func TestRuntimeExtraHostsAddsHostGatewayForDockerInternalEndpoints(t *testing.T) {
	env := []string{
		"EMULATOR_ENDPOINT=http://host.docker.internal:4566",
		"QUEUE_URL=http://host.docker.internal:4566/000000000000/jobs",
	}

	got := runtimeExtraHosts(env)
	want := []string{"host.docker.internal:host-gateway"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtimeExtraHosts() = %v, want %v", got, want)
	}
}

func TestRuntimeExtraHostsSkipsHostGatewayWhenNotNeeded(t *testing.T) {
	if got := runtimeExtraHosts([]string{"EMULATOR_ENDPOINT=http://127.0.0.1:4566"}); got != nil {
		t.Fatalf("runtimeExtraHosts() = %v, want nil", got)
	}
}

func TestSliceInvocationKeepsOnlyTheNextInvocation(t *testing.T) {
	output := "init noise\n" +
		"START RequestId: aaa Version: $LATEST\nfirst\nEND RequestId: aaa\nREPORT RequestId: aaa\tDuration: 1 ms\n" +
		"START RequestId: bbb Version: $LATEST\nsecond\nEND RequestId: bbb\nREPORT RequestId: bbb\tDuration: 2 ms\n"

	got, complete := sliceInvocation(output, 1)
	want := "START RequestId: bbb Version: $LATEST\nsecond\nEND RequestId: bbb\nREPORT RequestId: bbb\tDuration: 2 ms\n"
	if !complete || got != want {
		t.Fatalf("sliceInvocation() = %q, %v; want %q", got, complete, want)
	}
}

func TestSliceInvocationIgnoresWhichIDTheEmulatorPrints(t *testing.T) {
	// Newer emulators print the caller's request ID on START but their own
	// on END and REPORT, interleave "(rapid)" diagnostics, and may print
	// START after INVOKE START.
	output := "START RequestId: x1 Version: $LATEST\n" +
		"15 Nov 2026 20:08:00,198 [INFO] (rapid) INVOKE START(requestId: e4b8)\n" +
		"storing o-1\nEND RequestId: e4b8\nREPORT RequestId: e4b8\tDuration: 194.97 ms\n" +
		"15 Nov 2026 20:08:00,204 [INFO] (rapid) INVOKE RTDONE(status: success)\n" +
		"15 Nov 2026 20:08:25,109 [INFO] (rapid) INVOKE START(requestId: 6f22)\n" +
		"START RequestId: x2 Version: $LATEST\n" +
		"storing o-2\nEND RequestId: 6f22\nREPORT RequestId: 6f22\tDuration: 1.23 ms\n"

	got, complete := sliceInvocation(output, 1)
	want := "START RequestId: x2 Version: $LATEST\nstoring o-2\nEND RequestId: 6f22\nREPORT RequestId: 6f22\tDuration: 1.23 ms\n"
	if !complete || got != want {
		t.Fatalf("sliceInvocation() = %q, %v; want %q", got, complete, want)
	}
}

func TestSliceInvocationReportsIncompleteUntilReport(t *testing.T) {
	got, complete := sliceInvocation("START RequestId: ccc Version: $LATEST\nworking\n", 0)
	if complete {
		t.Fatalf("sliceInvocation() complete = true before REPORT")
	}
	if got != "START RequestId: ccc Version: $LATEST\nworking\n" {
		t.Fatalf("sliceInvocation() = %q", got)
	}
}
