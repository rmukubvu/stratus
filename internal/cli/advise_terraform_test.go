package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sqsModule = `{"id":"adr-1","module":{"option":"sqs-standard","deployable":true,"files":[
	{"name":"versions.tf","content":"terraform {}\n"},{"name":"main.tf","content":"resource \"aws_sqs_queue\" \"main\" {}\n"}]},
	"test_deploy":{"override":{"name":"stratus_override.tf","content":"provider \"aws\" {}\n"}}}`

// fakeTerraform answers each terraform subcommand with an exit code and
// output, and records the subcommands and the files in the working dir.
type fakeTerraform struct {
	codes   map[string]int
	outputs map[string]string
	ran     []string
	files   []string
}

func (f *fakeTerraform) install(t *testing.T) {
	commandRunner = func(_ context.Context, argv, env []string, stdout, _ io.Writer) (int, error) {
		dir := strings.TrimPrefix(argv[1], "-chdir=")
		sub := argv[2]
		f.ran = append(f.ran, sub)
		if sub == "init" {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				f.files = append(f.files, e.Name())
			}
		}
		fmt.Fprint(stdout, f.outputs[sub])
		return f.codes[sub], nil
	}
	t.Cleanup(func() { commandRunner = defaultCommandRunner })
}

func deployment(t *testing.T, fake *fakeControl) map[string]any {
	t.Helper()
	for _, c := range fake.calls {
		if body, ok := strings.CutPrefix(c, "advise-deployment adr-1 "); ok {
			out := map[string]any{}
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no deployment recorded; calls %v", fake.calls)
	return nil
}

func TestAdviseTerraformWritesTheModuleWithoutTheOverride(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-terraform": sqsModule}}
	dir := filepath.Join(t.TempDir(), "infra")
	code, stdout, stderr := run(t, fake, "advise", "terraform", "adr-1", "--option", "sqs-standard", "--out", dir)
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	if fake.calls[0] != "advise-terraform adr-1 sqs-standard" {
		t.Errorf("calls %v", fake.calls)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("wrote %v", entries)
	}
	if !strings.Contains(stdout, "--deploy") {
		t.Errorf("no hint to test-deploy:\n%s", stdout)
	}
}

func TestAdviseTerraformDeployAppliesReplansDestroysAndRecordsAPass(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-terraform": sqsModule}}
	tf := &fakeTerraform{outputs: map[string]string{"apply": "Apply complete! Resources: 2 added, 0 changed, 0 destroyed.\n"}}
	tf.install(t)
	code, stdout, stderr := run(t, fake, "advise", "terraform", "adr-1", "--out", filepath.Join(t.TempDir(), "infra"), "--deploy")
	if code != 0 {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	if got := strings.Join(tf.ran, ","); got != "init,apply,plan,destroy" {
		t.Errorf("ran %s", got)
	}
	if got := strings.Join(tf.files, ","); got != "main.tf,stratus_override.tf,versions.tf" {
		t.Errorf("deploy dir had %s", got)
	}
	d := deployment(t, fake)
	if d["outcome"] != "passed" || d["resources"] != 2.0 || d["option"] != "sqs-standard" {
		t.Errorf("recorded %v", d)
	}
	if !strings.Contains(stdout, "test deploy passed: 2 resources applied") {
		t.Errorf("output:\n%s", stdout)
	}
}

func TestAdviseTerraformDeployReportsDriftFromThePlanAfterApply(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-terraform": sqsModule}}
	tf := &fakeTerraform{
		codes: map[string]int{"plan": 2},
		outputs: map[string]string{
			"apply": "Apply complete! Resources: 1 added, 0 changed, 0 destroyed.\n",
			"plan":  "  # data.aws_iam_policy_document.x will be read during apply\n  # aws_sqs_queue.main will be updated in-place\n",
		},
	}
	tf.install(t)
	code, _, _ := run(t, fake, "advise", "terraform", "adr-1", "--out", t.TempDir(), "--deploy")
	d := deployment(t, fake)
	if code == 0 || d["outcome"] != "drifted" || d["detail"] != "aws_sqs_queue.main will be updated in-place" {
		t.Errorf("code %d, recorded %v", code, d)
	}
}

func TestAdviseTerraformDeployDestroysAfterAFailedApply(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-terraform": sqsModule}}
	tf := &fakeTerraform{
		codes:   map[string]int{"apply": 1},
		outputs: map[string]string{"apply": "Error: creating SQS Queue (x): api error NotImplemented: nope\n\n  with aws_sqs_queue.main\n"},
	}
	tf.install(t)
	code, _, _ := run(t, fake, "advise", "terraform", "adr-1", "--out", t.TempDir(), "--deploy")
	if got := strings.Join(tf.ran, ","); got != "init,apply,destroy" {
		t.Errorf("ran %s", got)
	}
	d := deployment(t, fake)
	if code == 0 || d["outcome"] != "failed" || !strings.Contains(d["detail"].(string), "creating SQS Queue (x): api error NotImplemented: nope") {
		t.Errorf("code %d, recorded %v", code, d)
	}
}

func TestAdviseTerraformDeployRefusesAModuleStratusCannotDeploy(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-terraform": `{"id":"adr-1","module":{"option":"msk","deployable":false,"reason":"needs a VPC","files":[]}}`}}
	commandRunner = func(context.Context, []string, []string, io.Writer, io.Writer) (int, error) {
		t.Fatal("ran terraform")
		return 0, nil
	}
	t.Cleanup(func() { commandRunner = defaultCommandRunner })
	code, _, stderr := run(t, fake, "advise", "terraform", "adr-1", "--out", t.TempDir(), "--deploy")
	if code == 0 || !strings.Contains(stderr, "msk cannot be test-deployed: needs a VPC") {
		t.Errorf("code %d stderr %s", code, stderr)
	}
}

func TestAdviseTerraformExplainsAMissingTerraform(t *testing.T) {
	fake := &fakeControl{bodies: map[string]string{"advise-terraform": sqsModule}}
	commandRunner = func(context.Context, []string, []string, io.Writer, io.Writer) (int, error) {
		return 1, errors.New(`exec: "terraform": executable file not found in $PATH`)
	}
	t.Cleanup(func() { commandRunner = defaultCommandRunner })
	code, _, stderr := run(t, fake, "advise", "terraform", "adr-1", "--out", t.TempDir(), "--deploy")
	if code == 0 || !strings.Contains(stderr, "install Terraform") || strings.Contains(strings.Join(fake.calls, " "), "advise-deployment") {
		t.Errorf("code %d calls %v stderr %s", code, fake.calls, stderr)
	}
}

func TestWriteFilesRefusesPathsOutsideTheDirectory(t *testing.T) {
	if err := writeFiles(t.TempDir(), []namedFile{{Name: "../evil.tf"}}); err == nil {
		t.Error("wrote outside the directory")
	}
}
