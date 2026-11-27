package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f *fakeControl) AdvisePlanProofs(_ context.Context, id string, in json.RawMessage) (json.RawMessage, error) {
	return f.record("advise-proofs " + id + " " + string(in))
}
func (f *fakeControl) AdviseRecordEvidence(_ context.Context, id string, in json.RawMessage) (json.RawMessage, error) {
	return f.record("advise-evidence " + id + " " + string(in))
}

const twoPlans = `{"id":"adr-1","proofs":[
	{"option":"kinesis","provable":true,"account":"912345678901","rate":200,"duration_seconds":20,"scale":0.1,
	 "limits":{"profile":"aws-default","scale":0.1},"script":"// k6 proof of kinesis\n"},
	{"option":"msk","provable":false,"reason":"needs a Kafka client"}]}`

func TestAdviseProveRunsEachProvablePlanInALoadRunAndRecordsTheEvidence(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{
		"advise-proofs":   twoPlans,
		"run-stop":        `{"id":"run-1","status":"finished"}`,
		"advise-evidence": `{"id":"adr-1","evidence":[{"option":"kinesis","run_id":"run-1","verdict":"held","target_rate":200,"requests":4000,"per_second":199.5,"p99_ms":2}]}`,
	}}
	var gotArgv []string
	var gotScript string
	commandRunner = func(_ context.Context, argv, _ []string, _, _ io.Writer) (int, error) {
		gotArgv = argv
		raw, _ := os.ReadFile(argv[len(argv)-1])
		gotScript = string(raw)
		return 0, nil
	}
	t.Cleanup(func() { commandRunner = defaultCommandRunner })

	code, stdout, stderr := run(t, fake, "advise", "prove", "adr-1", "--scale", "0.1", "--duration", "20s")
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	calls := strings.Join(fake.calls, "\n")
	for _, want := range []string{
		`advise-proofs adr-1 {"duration_seconds":20,"options":null,"scale":0.1}`,
		`run-start {"limits":{"profile":"aws-default","scale":0.1},"name":"advise adr-1 kinesis"}`,
		`advise-evidence adr-1 {"account":"912345678901","option":"kinesis","run_id":"run-1","scale":0.1,"target_rate":200}`,
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("no call %s in\n%s", want, calls)
		}
	}
	if gotArgv[0] != "k6" || gotArgv[1] != "run" || gotScript != "// k6 proof of kinesis\n" {
		t.Errorf("ran %v with script %q", gotArgv, gotScript)
	}
	for _, want := range []string{"kinesis        held", "ran 200/s", "msk            not proved: needs a Kafka client", "stratus advise adr adr-1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestAdviseProveDryRunWritesScriptsAndLimitsWithoutRunningThem(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-proofs": twoPlans}}
	commandRunner = func(context.Context, []string, []string, io.Writer, io.Writer) (int, error) {
		t.Fatal("dry run ran a command")
		return 0, nil
	}
	t.Cleanup(func() { commandRunner = defaultCommandRunner })
	dir := t.TempDir()
	code, stdout, stderr := run(t, fake, "advise", "prove", "adr-1", "--dry-run", "--scripts", dir)
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	limits, err := os.ReadFile(filepath.Join(dir, "proof-kinesis.limits.json"))
	if err != nil || string(limits) != `{"profile":"aws-default","scale":0.1}` {
		t.Errorf("limits file %q %v", limits, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "proof-kinesis.js")); err != nil {
		t.Error(err)
	}
	if !strings.Contains(stdout, "stratus load run --limits "+filepath.Join(dir, "proof-kinesis.limits.json")+" -- k6 run") {
		t.Errorf("no command printed:\n%s", stdout)
	}
}

func TestAdviseProveKeepsGoingWhenOneProofFailsAndReportsIt(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-proofs": twoPlans, "run-stop": `{"id":"run-1"}`}}
	commandRunner = func(context.Context, []string, []string, io.Writer, io.Writer) (int, error) { return 107, nil }
	t.Cleanup(func() { commandRunner = defaultCommandRunner })
	code, stdout, _ := run(t, fake, "advise", "prove", "adr-1")
	if code == 0 || !strings.Contains(stdout, "kinesis        proof failed") || strings.Contains(strings.Join(fake.calls, " "), "advise-evidence") {
		t.Errorf("code %d, calls %v, output:\n%s", code, fake.calls, stdout)
	}
}

func TestAdviseProveExplainsAMissingK6(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-proofs": twoPlans, "run-stop": `{"id":"run-1"}`}}
	commandRunner = func(context.Context, []string, []string, io.Writer, io.Writer) (int, error) {
		return 1, errors.New(`exec: "k6": executable file not found in $PATH`)
	}
	t.Cleanup(func() { commandRunner = defaultCommandRunner })
	code, _, stderr := run(t, fake, "advise", "prove", "adr-1")
	if code == 0 || !strings.Contains(stderr, "https://k6.io") {
		t.Errorf("code %d stderr %s", code, stderr)
	}
}

func TestAdviseProveRejectsADryRunWithNowhereToWrite(t *testing.T) {
	code, _, stderr := run(t, &fakeControl{}, "advise", "prove", "adr-1", "--dry-run")
	if code != 2 || !strings.Contains(stderr, "--scripts") {
		t.Errorf("code %d stderr %s", code, stderr)
	}
}
