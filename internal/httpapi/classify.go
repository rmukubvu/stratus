package httpapi

import (
	"net/http"
	"strings"

	"github.com/stratus/internal/awscompat"
)

type Protocol string

const (
	ProtocolInternal Protocol = "internal"
	ProtocolQuery    Protocol = "query"
	ProtocolJSON     Protocol = "json"
	ProtocolREST     Protocol = "rest"
	ProtocolS3       Protocol = "s3"
)

type Classification struct {
	Protocol  Protocol
	Service   string
	Operation string
	Bucket    string
	Key       string
}

var targetToService = map[string]string{
	"AWSSecurityTokenServiceV20110615": "sts",
	"DynamoDB_20120810":                "dynamodb",
	"AmazonSSM":                        "ssm",
	"Logs_20140328":                    "logs",
	"AmazonSNS":                        "sns",
}

var queryActionToService = map[string]string{
	"GetCallerIdentity":  "sts",
	"CreateQueue":        "sqs",
	"GetQueueUrl":        "sqs",
	"ListQueues":         "sqs",
	"GetQueueAttributes": "sqs",
	"SendMessage":        "sqs",
	"ReceiveMessage":     "sqs",
	"DeleteMessage":      "sqs",
	"DeleteQueue":        "sqs",
	"CreateRole":         "iam",
	"GetRole":            "iam",
	"ListRoles":          "iam",
	"DeleteRole":         "iam",
	"PutRolePolicy":      "iam",
	"GetRolePolicy":      "iam",
	"ListRolePolicies":   "iam",
	"DeleteRolePolicy":   "iam",
	"ValidateTemplate":   "cloudformation",
	"CreateStack":        "cloudformation",
	"DescribeStacks":     "cloudformation",
	"GetTemplate":        "cloudformation",
	"ListStacks":         "cloudformation",
	"DeleteStack":        "cloudformation",
}

var signedQueryServices = map[string]struct{}{
	"sts":            {},
	"sqs":            {},
	"iam":            {},
	"cloudformation": {},
}

var restPrefixToService = map[string]string{
	"/2015-03-31/functions": "lambda",
	"/restapis":             "apigateway",
	"/apis":                 "apigatewayv2",
	"/stacks":               "cloudformation",
}

func Classify(r *http.Request) (Classification, error) {
	if isHealthPath(r.URL.Path) {
		return Classification{
			Protocol:  ProtocolInternal,
			Service:   "stratus",
			Operation: "Health",
		}, nil
	}

	if target := r.Header.Get("X-Amz-Target"); target != "" {
		prefix, op := splitTarget(target)
		if service, ok := targetToService[prefix]; ok {
			protocol := ProtocolJSON
			if service == "sts" {
				protocol = ProtocolQuery
			}
			return Classification{
				Protocol:  protocol,
				Service:   service,
				Operation: op,
			}, nil
		}
	}

	if classification, ok := classifyQuery(r); ok {
		return classification, nil
	}

	if bucket, key, ok := classifyVirtualHostS3(r.Host, r.URL.Path); ok {
		return Classification{
			Protocol: ProtocolS3,
			Service:  "s3",
			Bucket:   bucket,
			Key:      key,
		}, nil
	}

	for prefix, service := range restPrefixToService {
		if strings.HasPrefix(r.URL.Path, prefix) {
			operation := ""
			if service == "lambda" {
				operation = classifyLambdaOperation(r)
			}
			return Classification{
				Protocol:  ProtocolREST,
				Service:   service,
				Operation: operation,
			}, nil
		}
	}

	bucket, key := classifyPathStyleS3(r.URL.Path)
	return Classification{
		Protocol: ProtocolS3,
		Service:  "s3",
		Bucket:   bucket,
		Key:      key,
	}, nil
}

func isHealthPath(path string) bool {
	return path == "/_stratus/health" || path == "/ping"
}

func queryAction(r *http.Request) string {
	if action := r.URL.Query().Get("Action"); action != "" {
		return action
	}
	if r.Method == http.MethodPost || r.Method == http.MethodGet {
		if err := r.ParseForm(); err == nil {
			return r.Form.Get("Action")
		}
	}
	return ""
}

func classifyQuery(r *http.Request) (Classification, bool) {
	if r.URL.Path != "/" {
		return Classification{}, false
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		return Classification{}, false
	}

	action := queryAction(r)
	sigv4 := awscompat.ParseSigV4Authorization(r.Header.Get("Authorization"))
	service := queryService(sigv4, action)
	if service == "" {
		if sigv4 == nil && (action != "" || r.Method == http.MethodPost) {
			service = "sts"
		} else {
			return Classification{}, false
		}
	}

	return Classification{
		Protocol:  ProtocolQuery,
		Service:   service,
		Operation: action,
	}, true
}

func queryService(sigv4 *awscompat.SigV4Identity, action string) string {
	if sigv4 != nil {
		if _, ok := signedQueryServices[sigv4.Service]; ok {
			return sigv4.Service
		}
	}
	return queryActionToService[action]
}

func splitTarget(target string) (string, string) {
	prefix, op, found := strings.Cut(target, ".")
	if !found {
		return target, ""
	}
	return prefix, op
}

func classifyVirtualHostS3(host, path string) (bucket, key string, ok bool) {
	host = stripPort(host)
	parts := strings.Split(host, ".")
	for idx, part := range parts {
		if part == "s3" && idx > 0 {
			key = strings.TrimPrefix(path, "/")
			return parts[0], key, true
		}
	}
	return "", "", false
}

func classifyPathStyleS3(path string) (bucket, key string) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", ""
	}

	parts := strings.SplitN(trimmed, "/", 2)
	bucket = parts[0]
	if len(parts) == 2 {
		key = parts[1]
	}
	return bucket, key
}

func stripPort(host string) string {
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		return host[:idx]
	}
	return host
}

func classifyLambdaOperation(r *http.Request) string {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && path == "/2015-03-31/functions":
		return "CreateFunction"
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/invocations"):
		return "Invoke"
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/configuration"):
		return "GetFunctionConfiguration"
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/2015-03-31/functions/"):
		return "GetFunction"
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/2015-03-31/functions/"):
		return "DeleteFunction"
	default:
		return ""
	}
}
