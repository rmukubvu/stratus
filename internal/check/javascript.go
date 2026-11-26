package check

import (
	"regexp"
	"strings"
)

// JavaScript finds AWS SDK for JavaScript v3 commands: a file that imports
// @aws-sdk/client-<name> and constructs new <Operation>Command(...). The
// DynamoDB document client's commands (PutCommand, QueryCommand) count as
// the DynamoDB operations they send.
type JavaScript struct{}

func (JavaScript) Accepts(name string) bool {
	for _, ext := range []string{".js", ".mjs", ".cjs", ".ts", ".mts", ".cts", ".jsx", ".tsx"} {
		if strings.HasSuffix(name, ext) && !strings.HasSuffix(name, ".d.ts") {
			return true
		}
	}
	return false
}

// jsClients maps @aws-sdk/client-<name> package names to botocore models
// where the two differ.
var jsClients = map[string]string{
	"api-gateway":                 "apigateway",
	"application-auto-scaling":    "application-autoscaling",
	"auto-scaling":                "autoscaling",
	"cloudwatch-events":           "events",
	"cloudwatch-logs":             "logs",
	"cognito-identity-provider":   "cognito-idp",
	"dynamodb-streams":            "dynamodbstreams",
	"elastic-load-balancing":      "elb",
	"elastic-load-balancing-v2":   "elbv2",
	"eventbridge":                 "events",
	"resource-groups-tagging-api": "resourcegroupstaggingapi",
	"route-53":                    "route53",
	"secrets-manager":             "secretsmanager",
	"sfn":                         "stepfunctions",
}

// documentClient maps @aws-sdk/lib-dynamodb commands to the operation each sends.
var documentClient = map[string]string{
	"Put": "PutItem", "Get": "GetItem", "Update": "UpdateItem", "Delete": "DeleteItem",
	"Query": "Query", "Scan": "Scan", "BatchGet": "BatchGetItem", "BatchWrite": "BatchWriteItem",
	"TransactGet": "TransactGetItems", "TransactWrite": "TransactWriteItems",
	"ExecuteStatement": "ExecuteStatement", "ExecuteTransaction": "ExecuteTransaction",
	"BatchExecuteStatement": "BatchExecuteStatement",
}

var (
	jsImport  = regexp.MustCompile(`["']@aws-sdk/(client-[a-z0-9-]+|lib-dynamodb)["']`)
	jsCommand = regexp.MustCompile(`new\s+([A-Z][A-Za-z0-9]*)Command\s*\(`)
)

func (JavaScript) Scan(file string, src []byte) []Call {
	var models []string
	docClient := false
	var calls []Call
	for _, m := range jsImport.FindAllSubmatchIndex(src, -1) {
		pkg := string(src[m[2]:m[3]])
		if pkg == "lib-dynamodb" {
			docClient = true
			continue
		}
		name := strings.TrimPrefix(pkg, "client-")
		model, ok := jsClients[name]
		if !ok {
			model = name
		}
		models = append(models, model)
		if operationsBy(model, identity) == nil {
			calls = append(calls, Call{Service: model, Operation: "*", Location: Location{file, lineOf(src, m[0])}})
		}
	}
	tables := make([]map[string]string, len(models))
	for i, model := range models {
		tables[i] = operationsBy(model, identity)
	}
	for _, m := range jsCommand.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		loc := Location{file, lineOf(src, m[0])}
		if op, ok := documentClient[name]; ok && docClient {
			calls = append(calls, Call{Service: "dynamodb", Operation: op, Location: loc})
			continue
		}
		for i, table := range tables {
			if op, ok := table[name]; ok {
				calls = append(calls, Call{Service: models[i], Operation: op, Location: loc})
				break
			}
		}
	}
	return calls
}

func identity(s string) string { return s }
