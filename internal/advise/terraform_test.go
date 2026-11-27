package advise

import (
	"strings"
	"testing"
)

// buildingFamily is fakeFamily with Terraform: "simple" deploys on
// Stratus, "heavy" is written but cannot be test-deployed.
type buildingFamily struct{ fakeFamily }

func (buildingFamily) Terraform(option string, r Requirements) TerraformCode {
	code := TerraformCode{
		Resources: `resource "aws_sqs_queue" "main" {
  name = var.name
}`,
		Variables: []TerraformVariable{{Name: "rate", Type: "number", Description: "Peak rate.", Default: r.Number("rate")}},
		Outputs:   []TerraformOutput{{Name: "queue_url", Description: "Queue URL.", Value: "aws_sqs_queue.main.url"}},
		Endpoints: []string{"sqs", "sqs"},
		Region:    "eu-west-1",
	}
	if option == "heavy" {
		code.NotDeployable = "heavy needs a VPC."
	}
	return code
}

func file(t *testing.T, m TerraformModule, name string) string {
	t.Helper()
	for _, f := range m.Files {
		if f.Name == name {
			return f.Content
		}
	}
	t.Fatalf("module has no %s; files: %v", name, m.Files)
	return ""
}

func TestTerraformIsWrittenOnlyForAnAssessedDecisionOfAFamilyThatWritesIt(t *testing.T) {
	f := buildingFamily{}
	d, _ := NewDecision("adr-t", "", f, t0)
	if _, err := d.Terraform(f, ""); code(err) != "DecisionNotAssessed" {
		t.Errorf("draft: %v", err)
	}
	plain := assessed(t, fakeFamily{}, nil)
	if _, err := plain.Terraform(fakeFamily{}, ""); code(err) != "TerraformUnsupported" {
		t.Errorf("family without Terraform: %v", err)
	}
}

func TestTerraformDefaultsToTheChosenOptionElseTheTopRanked(t *testing.T) {
	f := buildingFamily{}
	d := assessed(t, f, nil)
	m, err := d.Terraform(f, "")
	if err != nil || m.Option != "simple" {
		t.Fatalf("before a choice: %+v, %v", m, err)
	}
	if err := d.Decide(f, "heavy", "", t0); err != nil {
		t.Fatal(err)
	}
	if m, _ := d.Terraform(f, ""); m.Option != "heavy" {
		t.Errorf("after choosing heavy: wrote %s", m.Option)
	}
}

func TestTerraformRefusesRuledOutAndUnknownOptions(t *testing.T) {
	f := buildingFamily{}
	d := assessed(t, f, map[string]any{"strict": true})
	if _, err := d.Terraform(f, "simple"); code(err) != "OptionRuledOut" {
		t.Errorf("ruled out: %v", err)
	}
	if _, err := d.Terraform(f, "nope"); code(err) != "UnknownOption" {
		t.Errorf("unknown: %v", err)
	}
}

func TestATerraformModuleHasTheProviderVariablesResourcesAndOutputs(t *testing.T) {
	f := buildingFamily{}
	d := assessed(t, f, map[string]any{"rate": 250.0})
	d.Title = "Order events: v2!"
	m, err := d.Terraform(f, "simple")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range m.Files {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "versions.tf,variables.tf,main.tf,outputs.tf" {
		t.Errorf("files = %s", got)
	}
	versions := file(t, m, "versions.tf")
	for _, want := range []string{`source  = "hashicorp/aws"`, `version = "~> 5.0"`, "region = var.region", "tags = var.tags"} {
		if !strings.Contains(versions, want) {
			t.Errorf("versions.tf lacks %q:\n%s", want, versions)
		}
	}
	vars := file(t, m, "variables.tf")
	for _, want := range []string{`default     = "order-events-v2"`, `default     = "eu-west-1"`, `default     = { "adr" = "adr-p" }`, `variable "rate"`, "default     = 250"} {
		if !strings.Contains(vars, want) {
			t.Errorf("variables.tf lacks %q:\n%s", want, vars)
		}
	}
	if !strings.Contains(file(t, m, "outputs.tf"), "value       = aws_sqs_queue.main.url") {
		t.Errorf("outputs.tf:\n%s", file(t, m, "outputs.tf"))
	}
	if strings.Join(m.Endpoints, ",") != "sqs" || !m.Deployable || m.Account != ProofAccount(d.ID, "simple") {
		t.Errorf("module = %+v", m)
	}
	if heavy, _ := d.Terraform(f, "heavy"); heavy.Deployable || heavy.Reason != "heavy needs a VPC." {
		t.Errorf("heavy = %+v", heavy)
	}
}

func TestHCLValuesEscapeTemplateSequences(t *testing.T) {
	cases := map[string]any{
		`"a\"b $${x} %%{y}"`:       `a"b ${x} %{y}`,
		"true":                     true,
		"14":                       14,
		"0.5":                      0.5,
		`["a", "b"]`:               []string{"a", "b"},
		`{ "a" = "1", "b" = "2" }`: map[string]string{"b": "2", "a": "1"},
	}
	for want, v := range cases {
		if got := HCLValue(v); got != want {
			t.Errorf("HCLValue(%#v) = %s, want %s", v, got, want)
		}
	}
}

func TestTheTestDeployOverridePointsEveryEndpointAndSTSAtStratus(t *testing.T) {
	m := TerraformModule{Account: "912345678901", Endpoints: []string{"sqs", "kinesis"}}
	o := TestDeployOverride(m, "http://localhost:4566")
	if o.Name != "stratus_override.tf" {
		t.Errorf("name = %s", o.Name)
	}
	for _, want := range []string{`access_key                  = "912345678901"`, `kinesis     = "http://localhost:4566"`, `sqs         = "http://localhost:4566"`, `sts         = "http://localhost:4566"`, "skip_requesting_account_id  = true"} {
		if !strings.Contains(o.Content, want) {
			t.Errorf("override lacks %q:\n%s", want, o.Content)
		}
	}
}

func TestRecordingADeploymentReplacesTheOldOneAndChangingAnAnswerDropsIt(t *testing.T) {
	f := buildingFamily{}
	d := assessed(t, f, nil)
	if err := d.RecordDeployment(DeploymentInput{Option: "simple", Outcome: DeployFailed, Detail: "boom"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordDeployment(DeploymentInput{Option: "simple", Outcome: DeployPassed, Resources: 2}, t0); err != nil {
		t.Fatal(err)
	}
	if dep, ok := d.DeploymentFor("simple"); !ok || len(d.Deployments) != 1 || dep.Outcome != DeployPassed || dep.Tool != "terraform" {
		t.Fatalf("deployments = %+v", d.Deployments)
	}
	if err := d.Answer(f, map[string]any{"rate": 99.0}, t0); err != nil {
		t.Fatal(err)
	}
	if len(d.Deployments) != 0 {
		t.Errorf("a changed answer kept %+v", d.Deployments)
	}
}

func TestADeploymentNeedsAKnownOutcomeAndAnOptionThatFits(t *testing.T) {
	f := buildingFamily{}
	d := assessed(t, f, map[string]any{"strict": true})
	cases := map[string]DeploymentInput{
		"InvalidDeployment": {Option: "heavy", Outcome: "great"},
		"OptionRuledOut":    {Option: "simple", Outcome: DeployPassed},
	}
	for want, in := range cases {
		if err := d.RecordDeployment(in, t0); code(err) != want {
			t.Errorf("%+v: %v, want %s", in, err, want)
		}
	}
	if err := d.RecordDeployment(DeploymentInput{Option: " ", Outcome: DeployPassed}, t0); code(err) != "InvalidDeployment" {
		t.Errorf("blank option: %v", err)
	}
}

func TestTheADRPointsAtTheTerraformAndReportsTestDeploys(t *testing.T) {
	f := buildingFamily{}
	d := assessed(t, f, nil)
	adr := RenderADR(d, f)
	for _, want := range []string{"## Infrastructure", "`stratus advise terraform adr-p --out infra`", "has not been test-deployed", "`stratus advise terraform adr-p --deploy`"} {
		if !strings.Contains(adr, want) {
			t.Errorf("ADR lacks %q:\n%s", want, adr)
		}
	}
	_ = d.RecordDeployment(DeploymentInput{Option: "simple", Outcome: DeployPassed, Resources: 3}, t0)
	_ = d.RecordDeployment(DeploymentInput{Option: "heavy", Outcome: DeployDrifted, Resources: 1, Detail: "aws_sqs_queue.main will be updated in-place\nmore"}, t0)
	adr = RenderADR(d, f)
	for _, want := range []string{
		"- **Test deploy of Simple:** on 2026-11-27, terraform applied 3 resources to Stratus, a plan straight after found nothing to change, and destroy removed everything.",
		"- **Test deploy of Heavy:** on 2026-11-27, terraform applied 1 resource to Stratus and destroyed everything, but a plan straight after the apply wanted changes",
		"aws_sqs_queue.main will be updated in-place.",
	} {
		if !strings.Contains(adr, want) {
			t.Errorf("ADR lacks %q:\n%s", want, adr)
		}
	}
	if strings.Contains(adr, "has not been test-deployed") {
		t.Errorf("ADR still says not deployed:\n%s", adr)
	}
	if strings.Contains(RenderADR(assessed(t, fakeFamily{}, nil), fakeFamily{}), "## Infrastructure") {
		t.Error("a family without Terraform got an Infrastructure section")
	}
}
