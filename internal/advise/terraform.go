package advise

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Terraform turns a decision into infrastructure code: a root module for
// one option that a team can apply to AWS, and that Stratus can apply,
// re-plan and destroy first to prove it deploys. A family writes the
// resources; the core adds the provider, variables and outputs, so every
// family's module has the same shape.

// TerraformVariable is an input variable of a generated module.
type TerraformVariable struct {
	Name        string
	Type        string // an HCL type expression, e.g. "number" or "list(string)"
	Description string
	// Default is a string, a float64 or int, a bool, a []string or a
	// map[string]string; nil means the variable is required.
	Default any
}

// TerraformOutput is an output of a generated module.
type TerraformOutput struct {
	Name        string
	Description string
	Value       string // an HCL expression
}

// TerraformCode is a family's Terraform for one option.
type TerraformCode struct {
	// Resources are the module's resource and data blocks, in HCL. They
	// may read var.name and var.region, which the core declares.
	Resources string
	Variables []TerraformVariable
	Outputs   []TerraformOutput
	// Endpoints are the AWS provider endpoint names the resources call,
	// such as "sqs" or "kinesis", for pointing a test deploy at Stratus.
	Endpoints []string
	// Region is the region the module deploys to by default.
	Region string
	// NotDeployable, when set, says why Stratus cannot test-deploy this
	// module; the module is still written.
	NotDeployable string
	// Unsupported, when set, says why the family writes no Terraform for
	// this option.
	Unsupported string
}

// Terraformer is implemented by a family that writes Terraform for its
// options.
type Terraformer interface {
	Terraform(option string, r Requirements) TerraformCode
}

// File is one generated file.
type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// TerraformModule is a decision's Terraform for one option.
type TerraformModule struct {
	Option     string   `json:"option"`
	Region     string   `json:"region"`
	Files      []File   `json:"files"`
	Endpoints  []string `json:"endpoints"`
	Deployable bool     `json:"deployable"`
	// Reason says why the module cannot be test-deployed on Stratus.
	Reason string `json:"reason,omitempty"`
	// Account is the throwaway account a test deploy uses.
	Account string `json:"account"`
}

const (
	terraformProviderVersion = "~> 5.0"
	terraformMinVersion      = ">= 1.5"
	maxModuleName            = 40
	// OverrideFile is the file a test deploy adds to point the AWS
	// provider at Stratus; Terraform merges *_override.tf files over the
	// module, so the module itself stays unchanged.
	OverrideFile = "stratus_override.tf"
)

// Terraform writes the module for option, or for the chosen option (or,
// before a choice, the one ranked first) when option is empty. The
// decision must be assessed, and the option must not be ruled out.
func (d Decision) Terraform(f Family, option string) (TerraformModule, error) {
	if len(d.Assessments) == 0 {
		return TerraformModule{}, conflict("DecisionNotAssessed", "decision "+d.ID+" has no assessment of its current answers", "assess it first; Terraform is written for an option that fits")
	}
	tf, ok := f.(Terraformer)
	if !ok {
		return TerraformModule{}, conflict("TerraformUnsupported", "the "+f.ID()+" family does not write Terraform", "write the infrastructure by hand from the ADR")
	}
	option, err := d.terraformOption(option)
	if err != nil {
		return TerraformModule{}, err
	}
	code := tf.Terraform(option, d.Requirements(f))
	if code.Unsupported != "" {
		return TerraformModule{}, conflict("TerraformUnsupported", code.Unsupported, "write the infrastructure for "+option+" by hand from the ADR")
	}
	m := TerraformModule{
		Option: option, Region: code.Region, Endpoints: sortedUnique(code.Endpoints),
		Deployable: code.NotDeployable == "", Reason: code.NotDeployable,
		Account: ProofAccount(d.ID, option),
	}
	m.Files = []File{
		{Name: "versions.tf", Content: versionsTF(d, option)},
		{Name: "variables.tf", Content: variablesTF(d, option, code)},
		{Name: "main.tf", Content: strings.TrimSpace(code.Resources) + "\n"},
		{Name: "outputs.tf", Content: outputsTF(code.Outputs)},
	}
	return m, nil
}

func (d Decision) terraformOption(option string) (string, error) {
	option = strings.TrimSpace(option)
	if option == "" {
		if d.Chosen != "" {
			return d.Chosen, nil
		}
		a, ok := d.Recommended()
		if !ok {
			return "", conflict("NothingToBuild", "every option of decision "+d.ID+" was ruled out", "revisit the answers marked as assumed")
		}
		return a.Option, nil
	}
	a, ok := d.assessment(option)
	if !ok {
		return "", invalid("UnknownOption", fmt.Sprintf("%q is not an option of decision %s", option, d.ID), "write Terraform for one of: "+strings.Join(d.viable(), ", "))
	}
	if a.Verdict == VerdictRuledOut {
		return "", conflict("OptionRuledOut", fmt.Sprintf("%s was ruled out: %s", option, firstReason(a, ReasonRuledOut)), "write Terraform for one of: "+strings.Join(d.viable(), ", "))
	}
	return option, nil
}

func versionsTF(d Decision, option string) string {
	return fmt.Sprintf(`# Generated by 'stratus advise terraform' for %s (option %s).
# Apply it as a root module, or copy main.tf into your own.

terraform {
  required_version = %q

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = %q
    }
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = var.tags
  }
}
`, d.ID, option, terraformMinVersion, terraformProviderVersion)
}

func variablesTF(d Decision, option string, code TerraformCode) string {
	vars := append([]TerraformVariable{
		{Name: "name", Type: "string", Description: "Prefix for every resource name.", Default: moduleName(d.Title, option)},
		{Name: "region", Type: "string", Description: "AWS region to deploy to.", Default: code.Region},
		{Name: "tags", Type: "map(string)", Description: "Tags applied to every resource.", Default: map[string]string{"adr": d.ID}},
	}, code.Variables...)
	var b strings.Builder
	for i, v := range vars {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "variable %q {\n", v.Name)
		fmt.Fprintf(&b, "  description = %s\n", hclString(v.Description))
		fmt.Fprintf(&b, "  type        = %s\n", v.Type)
		if v.Default != nil {
			fmt.Fprintf(&b, "  default     = %s\n", HCLValue(v.Default))
		}
		b.WriteString("}\n")
	}
	return b.String()
}

func outputsTF(outputs []TerraformOutput) string {
	var b strings.Builder
	for i, o := range outputs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "output %q {\n  description = %s\n  value       = %s\n}\n", o.Name, hclString(o.Description), o.Value)
	}
	return b.String()
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// moduleName is the default resource-name prefix: the decision's title as
// a slug, which AWS accepts in queue, topic, rule and stream names.
func moduleName(title, option string) string {
	slug := strings.Trim(nonName.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if slug == "" {
		slug = option
	}
	if len(slug) > maxModuleName {
		slug = strings.TrimRight(slug[:maxModuleName], "-")
	}
	return slug
}

// HCLValue writes a Go value as an HCL literal. Strings are escaped,
// including Terraform's ${ and %{ template sequences.
func HCLValue(v any) string {
	switch x := v.(type) {
	case string:
		return hclString(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = hclString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = hclString(k) + " = " + hclString(x[k])
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	panic(fmt.Sprintf("advise: no HCL literal for %T", v))
}

func hclString(s string) string {
	raw, _ := json.Marshal(s) // JSON string escapes are valid in HCL
	out := strings.ReplaceAll(string(raw), "${", "$${")
	return strings.ReplaceAll(out, "%{", "%%{")
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// TestDeployOverride is the override file that points a module's AWS
// provider at Stratus, in the module's throwaway account, for a test
// deploy. STS is always included because the provider may call it.
func TestDeployOverride(m TerraformModule, endpoint string) File {
	var b strings.Builder
	b.WriteString("# Added by 'stratus advise terraform --deploy': points the AWS provider at\n")
	b.WriteString("# Stratus, in a throwaway account. Delete it to deploy to AWS.\n\n")
	b.WriteString("provider \"aws\" {\n")
	fmt.Fprintf(&b, "  access_key                  = %s\n", hclString(m.Account))
	b.WriteString("  secret_key                  = \"stratus\"\n")
	b.WriteString("  skip_credentials_validation = true\n")
	b.WriteString("  skip_metadata_api_check     = true\n")
	b.WriteString("  skip_requesting_account_id  = true\n\n")
	b.WriteString("  endpoints {\n")
	for _, e := range sortedUnique(append(append([]string{}, m.Endpoints...), "sts")) {
		fmt.Fprintf(&b, "    %-11s = %s\n", e, hclString(endpoint))
	}
	b.WriteString("  }\n}\n")
	return File{Name: OverrideFile, Content: b.String()}
}

// DeployOutcome is how a test deploy went.
type DeployOutcome string

const (
	// DeployPassed: applied, re-planned with no changes, and destroyed.
	DeployPassed DeployOutcome = "passed"
	// DeployDrifted: applied and destroyed, but a plan right after the
	// apply wanted changes, so Stratus read something back differently.
	DeployDrifted DeployOutcome = "drifted"
	// DeployFailed: init, apply or destroy failed.
	DeployFailed DeployOutcome = "failed"
)

// Deployment is a recorded test deploy of an option's Terraform.
type Deployment struct {
	Option     string        `json:"option"`
	Tool       string        `json:"tool"`
	Outcome    DeployOutcome `json:"outcome"`
	Resources  int           `json:"resources"`
	Detail     string        `json:"detail,omitempty"`
	RecordedAt time.Time     `json:"recorded_at"`
}

// DeploymentInput records a test deploy.
type DeploymentInput struct {
	Option    string        `json:"option"`
	Tool      string        `json:"tool,omitempty"`
	Outcome   DeployOutcome `json:"outcome"`
	Resources int           `json:"resources"`
	Detail    string        `json:"detail,omitempty"`
}

const maxDeployDetail = 2000

// RecordDeployment attaches a test deploy to an option, replacing any
// earlier one. The decision must be assessed against its current answers
// (or decided), and the option must not be ruled out.
func (d *Decision) RecordDeployment(in DeploymentInput, now time.Time) error {
	if len(d.Assessments) == 0 {
		return conflict("DecisionNotAssessed", "decision "+d.ID+" has no assessment of its current answers", "assess it, write the Terraform again and redeploy it")
	}
	switch in.Outcome {
	case DeployPassed, DeployDrifted, DeployFailed:
	default:
		return invalid("InvalidDeployment", fmt.Sprintf("outcome %q is not one of passed, drifted or failed", in.Outcome), "send the outcome of the apply, plan and destroy")
	}
	if in.Resources < 0 {
		return invalid("InvalidDeployment", "resources must not be negative", "send how many resources the apply created")
	}
	in.Option = strings.TrimSpace(in.Option)
	if in.Option == "" {
		return invalid("InvalidDeployment", "a deployment needs an option", "send the option whose Terraform was deployed")
	}
	if _, err := d.terraformOption(in.Option); err != nil {
		return err
	}
	if in.Tool == "" {
		in.Tool = "terraform"
	}
	if len(in.Detail) > maxDeployDetail {
		in.Detail = in.Detail[:maxDeployDetail]
	}
	kept := d.Deployments[:0:0]
	for _, old := range d.Deployments {
		if old.Option != in.Option {
			kept = append(kept, old)
		}
	}
	dep := Deployment{Option: in.Option, Tool: in.Tool, Outcome: in.Outcome, Resources: in.Resources, Detail: strings.TrimSpace(in.Detail), RecordedAt: now}
	d.Deployments, d.UpdatedAt = append(kept, dep), now
	return nil
}

// DeploymentFor returns the recorded test deploy of an option.
func (d Decision) DeploymentFor(option string) (Deployment, bool) {
	for _, dep := range d.Deployments {
		if dep.Option == option {
			return dep, true
		}
	}
	return Deployment{}, false
}
