package check

import (
	"regexp"
	"strings"
)

// Go finds AWS SDK for Go v2 calls: a file that imports
// github.com/aws/aws-sdk-go-v2/service/<name> and builds <pkg>.<Op>Input{}.
type Go struct{}

func (Go) Accepts(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// goPackages maps aws-sdk-go-v2 service packages to botocore models where
// the two differ.
var goPackages = map[string]string{
	"bedrockruntime":          "bedrock-runtime",
	"cloudwatchlogs":          "logs",
	"cognitoidentityprovider": "cognito-idp",
	"elasticloadbalancingv2":  "elbv2",
	"eventbridge":             "events",
	"sfn":                     "stepfunctions",
}

var goImport = regexp.MustCompile(`(?m)^\s*(?:import\s+)?([A-Za-z_][A-Za-z0-9_]*\s+)?"github\.com/aws/aws-sdk-go-v2/service/([a-z0-9]+)"`)

func (Go) Scan(file string, src []byte) []Call {
	var calls []Call
	for _, m := range goImport.FindAllSubmatchIndex(src, -1) {
		pkg := string(src[m[4]:m[5]])
		alias := pkg
		if m[2] >= 0 {
			alias = strings.TrimSpace(string(src[m[2]:m[3]]))
		}
		if alias == "_" {
			continue
		}
		model, ok := goPackages[pkg]
		if !ok {
			model = pkg
		}
		table := operationsBy(model, identity)
		if table == nil {
			calls = append(calls, Call{Service: model, Operation: "*", Location: Location{file, lineOf(src, m[0])}})
			continue
		}
		input := regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\.([A-Z][A-Za-z0-9]*)Input\s*\{`)
		for _, c := range input.FindAllSubmatchIndex(src, -1) {
			if op, ok := table[string(src[c[2]:c[3]])]; ok {
				calls = append(calls, Call{Service: model, Operation: op, Location: Location{file, lineOf(src, c[0])}})
			}
		}
	}
	return calls
}
