package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type terraformArgs struct {
	id        string
	option    string
	out       string
	deploy    bool
	terraform string
}

func parseTerraform(args []string, stderr io.Writer) (terraformArgs, error) {
	fs := flag.NewFlagSet("stratus advise terraform", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var t terraformArgs
	fs.StringVar(&t.option, "option", "", "Write Terraform for this option; default: the chosen one, else the one ranked first")
	fs.StringVar(&t.out, "out", "infra", "Directory to write the module to")
	fs.BoolVar(&t.deploy, "deploy", false, "Apply the module to Stratus, re-plan it, destroy it and record the outcome")
	fs.StringVar(&t.terraform, "terraform", "terraform", "The terraform (or tofu) binary")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprint(stderr, adviseUsage)
		return terraformArgs{}, errUsage
	}
	t.id = pos[0]
	return t, nil
}

// terraformModule is a control-API Terraform module, as the CLI reads it.
type terraformModule struct {
	ID     string `json:"id"`
	Module struct {
		Option     string      `json:"option"`
		Files      []namedFile `json:"files"`
		Deployable bool        `json:"deployable"`
		Reason     string      `json:"reason"`
	} `json:"module"`
	TestDeploy *struct {
		Override namedFile `json:"override"`
	} `json:"test_deploy"`
}

type namedFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// terraform writes a decision's Terraform to t.out and, with --deploy,
// test-deploys it against Stratus in a scratch copy, so t.out holds only
// the module a team commits.
func terraform(ctx context.Context, c ControlClient, t terraformArgs, stdout, stderr io.Writer) error {
	raw, err := c.AdviseTerraform(ctx, t.id, t.option)
	if err != nil {
		return err
	}
	var m terraformModule
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if err := writeFiles(t.out, m.Module.Files); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote Terraform for %s to %s/ (%s)\n", m.Module.Option, t.out, fileNames(m.Module.Files))
	if !t.deploy {
		if m.Module.Deployable {
			fmt.Fprintf(stdout, "test-deploy it against Stratus: stratus advise terraform %s --option %s --deploy\n", t.id, m.Module.Option)
		}
		return nil
	}
	if !m.Module.Deployable || m.TestDeploy == nil {
		return fmt.Errorf("%s cannot be test-deployed: %s", m.Module.Option, m.Module.Reason)
	}
	dir, err := os.MkdirTemp("", "stratus-terraform-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeFiles(dir, append(m.Module.Files, m.TestDeploy.Override)); err != nil {
		return err
	}
	result := testDeploy(ctx, t.terraform, dir, stderr)
	if result.notRun != nil {
		return fmt.Errorf("%w (install Terraform from https://developer.hashicorp.com/terraform/install, or pass --terraform)", result.notRun)
	}
	body, _ := json.Marshal(map[string]any{
		"option": m.Module.Option, "tool": "terraform", "outcome": result.outcome,
		"resources": result.resources, "detail": result.detail,
	})
	if _, err := c.AdviseRecordDeployment(ctx, t.id, body); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "test deploy %s: %s applied; %s\n", result.outcome, plural(result.resources, "resource"), result.summary())
	fmt.Fprintf(stdout, "The ADR now records it: stratus advise adr %s\n", t.id)
	if result.outcome != "passed" {
		return fmt.Errorf("test deploy of %s %s", m.Module.Option, result.outcome)
	}
	return nil
}

// deployResult is what init, apply, plan and destroy showed.
type deployResult struct {
	outcome   string // passed, drifted or failed
	resources int
	detail    string
	// notRun is set when terraform could not be started at all.
	notRun error
}

func (r deployResult) summary() string {
	switch r.outcome {
	case "passed":
		return "a plan straight after found nothing to change, and destroy removed everything"
	case "drifted":
		return "a plan straight after wanted changes: " + r.detail
	}
	return r.detail
}

var appliedResources = regexp.MustCompile(`Resources: (\d+) added`)

// testDeploy runs init, apply, a detailed-exitcode plan and destroy in
// dir. Destroy runs whenever apply ran, so a failed apply leaves nothing
// behind in Stratus.
func testDeploy(ctx context.Context, bin, dir string, stderr io.Writer) deployResult {
	run := func(args ...string) (int, string, error) {
		// Terraform prints errors on stderr, so both streams are kept
		// for the outcome's detail.
		var out bytes.Buffer
		argv := append([]string{bin, "-chdir=" + dir}, args...)
		both := io.MultiWriter(stderr, &out)
		code, err := commandRunner(ctx, argv, terraformEnv(), both, both)
		return code, out.String(), err
	}
	if code, out, err := run("init", "-input=false", "-no-color"); err != nil {
		return deployResult{notRun: err}
	} else if code != 0 {
		return deployResult{outcome: "failed", detail: "terraform init failed: " + lastError(out)}
	}
	code, out, err := run("apply", "-input=false", "-auto-approve", "-no-color")
	r := deployResult{outcome: "passed"}
	if m := appliedResources.FindStringSubmatch(out); m != nil {
		r.resources, _ = strconv.Atoi(m[1])
	}
	switch {
	case err != nil:
		r.outcome, r.detail = "failed", "terraform apply: "+err.Error()
	case code != 0:
		r.outcome, r.detail = "failed", "terraform apply failed: "+lastError(out)
	default:
		code, out, err := run("plan", "-input=false", "-detailed-exitcode", "-no-color")
		switch {
		case err != nil || code == 1:
			r.outcome, r.detail = "failed", "terraform plan after apply failed: "+lastError(out)
		case code == 2:
			r.outcome, r.detail = "drifted", planChanges(out)
		}
	}
	if code, out, err := run("destroy", "-input=false", "-auto-approve", "-no-color"); err != nil || code != 0 {
		r.outcome = "failed"
		r.detail = strings.TrimSpace(r.detail + " terraform destroy failed: " + lastError(out))
	}
	return r
}

// terraformEnv shares downloaded providers between test deploys, which
// each run in a fresh directory, unless the user set a cache already.
// Terraform's cache is not safe for concurrent installs, so run parallel
// deploys only once one has filled it.
func terraformEnv() []string {
	env := []string{"TF_IN_AUTOMATION=1"}
	if os.Getenv("TF_PLUGIN_CACHE_DIR") != "" {
		return env
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return env
	}
	dir := filepath.Join(cache, "stratus", "terraform-plugins")
	if os.MkdirAll(dir, 0o755) != nil {
		return env
	}
	return append(env, "TF_PLUGIN_CACHE_DIR="+dir)
}

// lastError is the first "Error:" line of terraform's output, which names
// the resource and the API error, or the last line when there is none.
func lastError(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Error: ") {
			return strings.TrimPrefix(l, "Error: ")
		}
	}
	if len(lines) == 0 {
		return "no output"
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

var planChange = regexp.MustCompile(`(?m)^\s*# (\S+) (will be|must be) (.+)$`)

// planChanges lists the resources a plan wanted to change.
func planChanges(out string) string {
	var changes []string
	for _, m := range planChange.FindAllStringSubmatch(out, -1) {
		if strings.HasPrefix(m[1], "data.") {
			continue // data sources re-read because a resource changes
		}
		changes = append(changes, m[1]+" "+m[2]+" "+m[3])
	}
	if len(changes) == 0 {
		return "the plan reported changes"
	}
	return strings.Join(changes, "; ")
}

func writeFiles(dir string, files []namedFile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if f.Name != filepath.Base(f.Name) || f.Name == "." || f.Name == ".." {
			return errors.New("refusing to write file " + strconv.Quote(f.Name) + " outside " + dir)
		}
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func fileNames(files []namedFile) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return strings.Join(names, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
