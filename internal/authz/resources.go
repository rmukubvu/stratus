package authz

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/iampolicy"
	"github.com/stratus/internal/scope"
)

// maxInspectedBody bounds how much of a request body is read to find the
// resource; larger bodies (uploads) are not inspected.
const maxInspectedBody = 1 << 20

// params gives access to a request's named parameters, from a JSON body or
// a query-protocol form.
type params struct {
	json map[string]any
	form url.Values
}

func (p params) get(name string) string {
	if v, ok := p.json[name].(string); ok {
		return v
	}
	return p.form.Get(name)
}

func readParams(r *http.Request, protocolJSON bool) params {
	if r.ContentLength > maxInspectedBody {
		return params{}
	}
	if !protocolJSON {
		form, _ := awscompat.ParseQueryForm(r)
		return params{form: form}
	}
	if r.Body == nil {
		return params{}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxInspectedBody))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return params{}
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return params{json: body}
}

// Resource names the ARN a request acts on, or iampolicy.AnyResource when
// Stratus cannot name it, in which case statements are matched on action.
func Resource(service, operation string, r *http.Request, sc scope.Scope, bucket, key string, p params) string {
	switch service {
	case "s3":
		switch {
		case bucket == "":
			return iampolicy.AnyResource
		case key == "":
			return "arn:aws:s3:::" + bucket
		default:
			return "arn:aws:s3:::" + bucket + "/" + key
		}
	case "sqs":
		if name := lastSegment(p.get("QueueUrl")); name != "" {
			return sc.ARN("sqs", name)
		}
		if name := p.get("QueueName"); name != "" {
			return sc.ARN("sqs", name)
		}
	case "sns":
		for _, field := range []string{"TopicArn", "TargetArn", "ResourceArn"} {
			if arn := p.get(field); arn != "" {
				return arn
			}
		}
		if operation == "CreateTopic" && p.get("Name") != "" {
			return sc.ARN("sns", p.get("Name"))
		}
	case "dynamodb", "dynamodbstreams":
		if arn := p.get("StreamArn"); arn != "" {
			return arn
		}
		if table := p.get("TableName"); table != "" {
			if strings.HasPrefix(table, "arn:") {
				return table
			}
			return sc.ARN("dynamodb", "table/"+table)
		}
	case "lambda":
		if name := lambdaFunctionName(r.URL.Path, p); name != "" {
			if strings.HasPrefix(name, "arn:") {
				return name
			}
			return sc.ARN("lambda", "function:"+name)
		}
	case "ssm":
		if name := p.get("Name"); name != "" {
			return sc.ARN("ssm", "parameter/"+strings.TrimPrefix(name, "/"))
		}
	case "kms":
		if id := p.get("KeyId"); id != "" && !strings.HasPrefix(id, "alias/") {
			if strings.HasPrefix(id, "arn:") {
				return id
			}
			return sc.ARN("kms", "key/"+id)
		}
	case "secretsmanager":
		if id := p.get("SecretId"); strings.HasPrefix(id, "arn:") {
			return id
		}
	case "kinesis":
		if arn := p.get("StreamARN"); arn != "" {
			return arn
		}
		if name := p.get("StreamName"); name != "" {
			return sc.ARN("kinesis", "stream/"+name)
		}
	case "logs":
		if group := p.get("logGroupName"); group != "" {
			if stream := p.get("logStreamName"); stream != "" {
				return sc.ARN("logs", "log-group:"+group+":log-stream:"+stream)
			}
			return sc.ARN("logs", "log-group:"+group+":*")
		}
	case "stepfunctions":
		for _, field := range []string{"stateMachineArn", "executionArn"} {
			if arn := p.get(field); arn != "" {
				return arn
			}
		}
		if operation == "CreateStateMachine" && p.get("name") != "" {
			return sc.ARN("states", "stateMachine:"+p.get("name"))
		}
	case "events":
		if rule := firstNonEmpty(p.get("Rule"), ruleName(operation, p)); rule != "" {
			return sc.ARN("events", "rule/"+rule)
		}
	case "iam":
		if arn := p.get("PolicyArn"); arn != "" {
			return arn
		}
		if name := p.get("RoleName"); name != "" {
			return sc.GlobalARN("iam", "role/"+name)
		}
		if name := p.get("UserName"); name != "" {
			return sc.GlobalARN("iam", "user/"+name)
		}
	case "sts":
		if arn := p.get("RoleArn"); arn != "" {
			return arn
		}
	}
	return iampolicy.AnyResource
}

func ruleName(operation string, p params) string {
	switch operation {
	case "PutRule", "DeleteRule", "DescribeRule", "EnableRule", "DisableRule":
		return p.get("Name")
	}
	return ""
}

// lambdaFunctionName finds the function in a Lambda REST path such as
// /2015-03-31/functions/{name}/invocations, or in a CreateFunction body.
func lambdaFunctionName(path string, p params) string {
	const marker = "/functions/"
	if i := strings.Index(path, marker); i >= 0 {
		name := path[i+len(marker):]
		if j := strings.Index(name, "/"); j >= 0 {
			name = name[:j]
		}
		if unescaped, err := url.PathUnescape(name); err == nil {
			name = unescaped
		}
		return name
	}
	return p.get("FunctionName")
}

func lastSegment(u string) string {
	u = strings.TrimSuffix(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
