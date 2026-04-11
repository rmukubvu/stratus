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
	"AWSSecurityTokenServiceV20110615":     "sts",
	"DynamoDB_20120810":                    "dynamodb",
	"AmazonSSM":                            "ssm",
	"Logs_20140328":                        "logs",
	"TrentService":                         "kms",
	"AWSEvents":                            "events",
	"secretsmanager":                       "secretsmanager",
	"Kinesis_20131202":                     "kinesis",
	"AWSCognitoIdentityProviderService":    "cognitoidp",
	"AWSStepFunctions":                     "stepfunctions",
	"AmazonEC2ContainerRegistry_V20150921": "ecr",
	"AmazonEC2ContainerServiceV20141113":   "ecs",
	"CertificateManager":                   "acm",
	"DynamoDBStreams_20120810":             "dynamodbstreams",
	"AmazonSQS":                            "sqs",
	"AmazonCloudWatch":                     "monitoring",
}

var queryActionToService = map[string]string{
	"GetCallerIdentity":        "sts",
	"CreateTopic":              "sns",
	"ListTopics":               "sns",
	"GetTopicAttributes":       "sns",
	"SetTopicAttributes":       "sns",
	"Publish":                  "sns",
	"DeleteTopic":              "sns",
	"CreateQueue":              "sqs",
	"GetQueueUrl":              "sqs",
	"ListQueues":               "sqs",
	"GetQueueAttributes":       "sqs",
	"SetQueueAttributes":       "sqs",
	"SendMessage":              "sqs",
	"ReceiveMessage":           "sqs",
	"ChangeMessageVisibility":  "sqs",
	"DeleteMessage":            "sqs",
	"DeleteQueue":              "sqs",
	"CreateRole":               "iam",
	"GetRole":                  "iam",
	"ListRoles":                "iam",
	"DeleteRole":               "iam",
	"PutRolePolicy":            "iam",
	"GetRolePolicy":            "iam",
	"ListRolePolicies":         "iam",
	"ListAttachedRolePolicies": "iam",
	"DeleteRolePolicy":         "iam",
	"ValidateTemplate":         "cloudformation",
	"CreateStack":              "cloudformation",
	"CreateChangeSet":          "cloudformation",
	"DescribeStacks":           "cloudformation",
	"DescribeChangeSet":        "cloudformation",
	"DescribeStackEvents":      "cloudformation",
	"ListStackResources":       "cloudformation",
	"GetTemplate":              "cloudformation",
	"ListStacks":               "cloudformation",
	"ExecuteChangeSet":         "cloudformation",
	"DeleteChangeSet":          "cloudformation",
	"DeleteStack":              "cloudformation",
	"PutMetricData":            "monitoring",
	"ListMetrics":              "monitoring",
	"GetMetricStatistics":      "monitoring",
	"CreateLoadBalancer":       "elbv2",
	"DescribeLoadBalancers":    "elbv2",
	"CreateTargetGroup":        "elbv2",
	"DescribeTargetGroups":     "elbv2",
	"CreateListener":           "elbv2",
	"DescribeListeners":        "elbv2",
	"RegisterTargets":          "elbv2",
	"DescribeTargetHealth":     "elbv2",
	"CreateDBSubnetGroup":      "rds",
	"DescribeDBSubnetGroups":   "rds",
	"CreateDBInstance":         "rds",
	"DescribeDBInstances":      "rds",
	"DeleteDBInstance":         "rds",
	"CreateCacheCluster":       "elasticache",
	"DescribeCacheClusters":    "elasticache",
	"DeleteCacheCluster":       "elasticache",
}

var signedQueryServices = map[string]struct{}{
	"sts":            {},
	"sns":            {},
	"sqs":            {},
	"iam":            {},
	"cloudformation": {},
	"monitoring":     {},
	"rds":            {},
	"elasticache":    {},
}

var restPrefixToService = map[string]string{
	"/2015-03-31/functions":             "lambda",
	"/2019-09-25/functions":             "lambda",
	"/2015-03-31/event-source-mappings": "lambda",
	"/2018-10-31/layers":                "lambda",
	"/restapis":                         "apigateway",
	"/_aws/restapis/":                   "apigateway",
	"/v2/apis":                          "apigatewayv2",
	"/_aws/execute-api/":                "apigatewayv2",
	"/stacks":                           "cloudformation",
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
			Protocol:  ProtocolS3,
			Service:   "s3",
			Operation: classifyS3Operation(r, bucket, key),
			Bucket:    bucket,
			Key:       key,
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
		Protocol:  ProtocolS3,
		Service:   "s3",
		Operation: classifyS3Operation(r, bucket, key),
		Bucket:    bucket,
		Key:       key,
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
		if form, err := awscompat.ParseQueryForm(r); err == nil {
			return form.Get("Action")
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

func classifyS3Operation(r *http.Request, bucket, key string) string {
	query := r.URL.Query()
	switch {
	case bucket == "" && key == "" && r.Method == http.MethodGet:
		return "ListBuckets"
	case bucket != "" && key == "" && r.Method == http.MethodPut:
		return "CreateBucket"
	case bucket != "" && key == "" && r.Method == http.MethodGet:
		if query.Get("list-type") == "2" || len(query) == 0 {
			return "ListObjectsV2"
		}
	case bucket != "" && key == "" && r.Method == http.MethodHead:
		return "HeadBucket"
	case bucket != "" && key == "" && r.Method == http.MethodDelete:
		return "DeleteBucket"
	case bucket != "" && key != "" && r.Method == http.MethodPut && query.Get("uploadId") != "" && query.Get("partNumber") != "":
		return "UploadPart"
	case bucket != "" && key != "" && r.Method == http.MethodPost && query.Has("uploads"):
		return "CreateMultipartUpload"
	case bucket != "" && key != "" && r.Method == http.MethodPost && query.Get("uploadId") != "":
		return "CompleteMultipartUpload"
	case bucket != "" && key != "" && r.Method == http.MethodDelete && query.Get("uploadId") != "":
		return "AbortMultipartUpload"
	case bucket != "" && key != "" && r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		return "CopyObject"
	case bucket != "" && key != "" && r.Method == http.MethodPut:
		return "PutObject"
	case bucket != "" && key != "" && r.Method == http.MethodGet:
		return "GetObject"
	case bucket != "" && key != "" && r.Method == http.MethodHead:
		return "HeadObject"
	case bucket != "" && key != "" && r.Method == http.MethodDelete:
		return "DeleteObject"
	default:
		return ""
	}
	return ""
}

func classifyLambdaOperation(r *http.Request) string {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/2018-10-31/layers/") && !strings.Contains(path, "/versions/"):
		return "PublishLayerVersion"
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/2018-10-31/layers/") && strings.HasSuffix(path, "/versions"):
		return "ListLayerVersions"
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/2018-10-31/layers/") && strings.Contains(path, "/versions/"):
		return "GetLayerVersion"
	case r.Method == http.MethodPost && path == "/2015-03-31/event-source-mappings":
		return "CreateEventSourceMapping"
	case r.Method == http.MethodGet && path == "/2015-03-31/event-source-mappings":
		return "ListEventSourceMappings"
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/2015-03-31/event-source-mappings/"):
		return "GetEventSourceMapping"
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/2015-03-31/event-source-mappings/"):
		return "DeleteEventSourceMapping"
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/event-invoke-config"):
		return "PutFunctionEventInvokeConfig"
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/event-invoke-config"):
		return "GetFunctionEventInvokeConfig"
	case r.Method == http.MethodDelete && strings.HasSuffix(path, "/event-invoke-config"):
		return "DeleteFunctionEventInvokeConfig"
	case r.Method == http.MethodPost && path == "/2015-03-31/functions":
		return "CreateFunction"
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/versions"):
		return "PublishVersion"
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/versions"):
		return "ListVersionsByFunction"
	case r.Method == http.MethodPost && strings.Contains(path, "/aliases") && strings.Count(path, "/") == 4:
		return "CreateAlias"
	case r.Method == http.MethodGet && strings.Contains(path, "/aliases") && strings.Count(path, "/") == 4:
		return "ListAliases"
	case r.Method == http.MethodGet && strings.Contains(path, "/aliases/"):
		return "GetAlias"
	case r.Method == http.MethodPut && strings.Contains(path, "/aliases/"):
		return "UpdateAlias"
	case r.Method == http.MethodDelete && strings.Contains(path, "/aliases/"):
		return "DeleteAlias"
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
