package httpapi

import (
	"net/http"
	"strings"

	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/services/bedrock"
	"github.com/stratus/internal/services/ec2"
	"github.com/stratus/internal/services/fis"
	"github.com/stratus/internal/services/kafka"
	"github.com/stratus/internal/services/monitoring"
	"github.com/stratus/internal/services/route53"
	"github.com/stratus/internal/services/scheduler"
	"github.com/stratus/internal/services/ses"
)

type Protocol string

const (
	ProtocolInternal Protocol = "internal"
	ProtocolQuery    Protocol = "query"
	ProtocolJSON     Protocol = "json"
	ProtocolREST     Protocol = "rest"
	// ProtocolRESTXML is a REST API that speaks XML, errors included.
	ProtocolRESTXML Protocol = "rest-xml"
	ProtocolS3      Protocol = "s3"
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
	"GraniteServiceVersion20100801":        "monitoring",
	"Route53AutoNaming_v20170314":          "servicediscovery",
}

var queryActionToService = map[string]string{
	"GetCallerIdentity":        "sts",
	"AssumeRole":               "sts",
	"GetAccessKeyInfo":         "sts",
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
	"ses":            {},
	"ec2":            {},
}

func init() {
	// SES v1 is a query API like SNS; route its unsigned actions by name.
	for _, action := range ses.QueryOperations {
		if _, taken := queryActionToService[action]; !taken {
			queryActionToService[action] = "ses"
		}
	}
	for _, action := range monitoring.AlarmOperations() {
		if _, taken := queryActionToService[action]; !taken && strings.Contains(action, "Alarm") {
			queryActionToService[action] = "monitoring"
		}
	}
	for _, action := range ec2.Operations {
		if _, taken := queryActionToService[action]; !taken {
			queryActionToService[action] = "ec2"
		}
	}
}

// signedRESTServices are REST APIs whose paths (/experiments, /schedules,
// /v2/email/..., /model/...) would otherwise read as path-style S3 buckets.
// They are recognised by their SigV4 signing name, or by their host prefix
// when a client sends unsigned requests. Bedrock Runtime signs as
// "bedrock"; MSK signs as "kafka".
var signedRESTServices = map[string]string{"fis": "fis", "scheduler": "scheduler", "ses": "ses", "bedrock": "bedrock", "kafka": "kafka", "route53": "route53"}

var restHostPrefixes = map[string]string{"fis.": "fis", "scheduler.": "scheduler", "email.": "ses", "bedrock-runtime.": "bedrock", "kafka.": "kafka", "route53.": "route53"}

var restPrefixToService = map[string]string{
	"/2015-03-31/functions":             "lambda",
	"/2019-09-25/functions":             "lambda",
	"/2020-06-30/functions":             "lambda",
	"/2015-03-31/event-source-mappings": "lambda",
	"/2018-10-31/layers":                "lambda",
	"/restapis":                         "apigateway",
	"/_aws/restapis/":                   "apigateway",
	"/v2/apis":                          "apigatewayv2",
	"/_aws/execute-api/":                "apigatewayv2",
	"/stacks":                           "cloudformation",
	ses.V2PathPrefix:                    "ses",
	route53.PathPrefix:                  "route53",
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

	if service := signedRESTService(r); service != "" {
		return Classification{
			Protocol:  restProtocol(service),
			Service:   service,
			Operation: restOperation(service, r),
		}, nil
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
			switch service {
			case "lambda":
				operation = classifyLambdaOperation(r)
			case "ses":
				operation = ses.OperationV2(r.Method, r.URL.Path)
			case "route53":
				operation = route53.Operation(r.Method, r.URL.Path)
			}
			return Classification{
				Protocol:  restProtocol(service),
				Service:   service,
				Operation: operation,
			}, nil
		}
	}

	if name := nonS3SigningName(r); name != "" {
		return Classification{
			Protocol:  ProtocolREST,
			Service:   UnsupportedService,
			Operation: name + " " + r.Method + " " + r.URL.Path,
		}, nil
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

// UnsupportedService classifies a REST request signed for a service other
// than S3 whose path matches nothing Stratus serves. Without it, the request
// would fall through to path-style S3 (a CreateUsagePlan POST to /usageplans
// would read as an S3 bucket) and get a misleading S3 error instead of
// NotImplemented.
const UnsupportedService = "unsupported"

// nonS3SigningName is the SigV4 signing name of a request signed for a service
// other than S3, or "".
func nonS3SigningName(r *http.Request) string {
	sigv4 := awscompat.ParseSigV4Authorization(r.Header.Get("Authorization"))
	if sigv4 == nil {
		return ""
	}
	switch sigv4.Service {
	case "", "s3", "s3express", "s3-object-lambda":
		return ""
	}
	return sigv4.Service
}

// signedRESTService names the REST service a request is signed for, or ""
// when it is not one of signedRESTServices. SES v1 signs with the same name
// but is a query API on "/", so it is left to classifyQuery.
func signedRESTService(r *http.Request) string {
	service := ""
	if sigv4 := awscompat.ParseSigV4Authorization(r.Header.Get("Authorization")); sigv4 != nil {
		service = signedRESTServices[sigv4.Service]
	} else {
		for prefix, s := range restHostPrefixes {
			if strings.HasPrefix(stripPort(r.Host), prefix) {
				service = s
			}
		}
	}
	if service == "ses" && r.URL.Path == "/" {
		return ""
	}
	return service
}

func restOperation(service string, r *http.Request) string {
	switch service {
	case "fis":
		return fis.Operation(r.Method, r.URL.Path)
	case "scheduler":
		return scheduler.Operation(r.Method, r.URL.Path)
	case "ses":
		return ses.OperationV2(r.Method, r.URL.Path)
	case "bedrock":
		return bedrock.Operation(r.Method, r.URL.EscapedPath())
	case "kafka":
		return kafka.Operation(r.Method, r.URL.EscapedPath())
	case "route53":
		return route53.Operation(r.Method, r.URL.Path)
	}
	return ""
}

// restProtocol tells REST-JSON services from Route 53, which speaks XML.
func restProtocol(service string) Protocol {
	if service == "route53" {
		return ProtocolRESTXML
	}
	return ProtocolREST
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

// s3BucketSubresources names the S3 API operation for a bucket request that
// carries a sub-resource query parameter, by parameter and method.
var s3BucketSubresources = map[string]map[string]string{
	"tagging":           {http.MethodPut: "PutBucketTagging", http.MethodGet: "GetBucketTagging", http.MethodDelete: "DeleteBucketTagging"},
	"versioning":        {http.MethodPut: "PutBucketVersioning", http.MethodGet: "GetBucketVersioning"},
	"lifecycle":         {http.MethodPut: "PutBucketLifecycleConfiguration", http.MethodGet: "GetBucketLifecycleConfiguration", http.MethodDelete: "DeleteBucketLifecycle"},
	"cors":              {http.MethodPut: "PutBucketCors", http.MethodGet: "GetBucketCors", http.MethodDelete: "DeleteBucketCors"},
	"website":           {http.MethodPut: "PutBucketWebsite", http.MethodGet: "GetBucketWebsite", http.MethodDelete: "DeleteBucketWebsite"},
	"replication":       {http.MethodPut: "PutBucketReplication", http.MethodGet: "GetBucketReplication", http.MethodDelete: "DeleteBucketReplication"},
	"object-lock":       {http.MethodPut: "PutObjectLockConfiguration", http.MethodGet: "GetObjectLockConfiguration"},
	"encryption":        {http.MethodPut: "PutBucketEncryption", http.MethodGet: "GetBucketEncryption", http.MethodDelete: "DeleteBucketEncryption"},
	"ownershipControls": {http.MethodPut: "PutBucketOwnershipControls", http.MethodGet: "GetBucketOwnershipControls", http.MethodDelete: "DeleteBucketOwnershipControls"},
	"publicAccessBlock": {http.MethodPut: "PutPublicAccessBlock", http.MethodGet: "GetPublicAccessBlock", http.MethodDelete: "DeletePublicAccessBlock"},
	"notification":      {http.MethodPut: "PutBucketNotificationConfiguration", http.MethodGet: "GetBucketNotificationConfiguration"},
	"logging":           {http.MethodPut: "PutBucketLogging", http.MethodGet: "GetBucketLogging"},
	"accelerate":        {http.MethodPut: "PutBucketAccelerateConfiguration", http.MethodGet: "GetBucketAccelerateConfiguration"},
	"requestPayment":    {http.MethodPut: "PutBucketRequestPayment", http.MethodGet: "GetBucketRequestPayment"},
	"policy":            {http.MethodPut: "PutBucketPolicy", http.MethodGet: "GetBucketPolicy", http.MethodDelete: "DeleteBucketPolicy"},
	"acl":               {http.MethodPut: "PutBucketAcl", http.MethodGet: "GetBucketAcl"},
	"location":          {http.MethodGet: "GetBucketLocation"},
	"versions":          {http.MethodGet: "ListObjectVersions"},
	"uploads":           {http.MethodGet: "ListMultipartUploads"},
	"delete":            {http.MethodPost: "DeleteObjects"},
}

// s3ObjectSubresources does the same for object requests.
var s3ObjectSubresources = map[string]map[string]string{
	"tagging":    {http.MethodPut: "PutObjectTagging", http.MethodGet: "GetObjectTagging", http.MethodDelete: "DeleteObjectTagging"},
	"retention":  {http.MethodPut: "PutObjectRetention", http.MethodGet: "GetObjectRetention"},
	"legal-hold": {http.MethodPut: "PutObjectLegalHold", http.MethodGet: "GetObjectLegalHold"},
	"acl":        {http.MethodPut: "PutObjectAcl", http.MethodGet: "GetObjectAcl"},
	"attributes": {http.MethodGet: "GetObjectAttributes"},
	"restore":    {http.MethodPost: "RestoreObject"},
}

// s3SubresourceOperation names a sub-resource request, so a PUT ?retention
// is PutObjectRetention rather than PutObject. ok is false when the request
// names no known sub-resource.
func s3SubresourceOperation(r *http.Request, bucket, key string) (string, bool) {
	if bucket == "" {
		return "", false
	}
	table := s3BucketSubresources
	if key != "" {
		table = s3ObjectSubresources
	}
	query := r.URL.Query()
	for name, methods := range table {
		if query.Has(name) {
			return methods[r.Method], true
		}
	}
	return "", false
}

func classifyS3Operation(r *http.Request, bucket, key string) string {
	if operation, ok := s3SubresourceOperation(r, bucket, key); ok {
		return operation
	}
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
	case bucket != "" && key != "" && r.Method == http.MethodGet && query.Get("uploadId") != "":
		return "ListParts"
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
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/code-signing-config"):
		return "GetFunctionCodeSigningConfig"
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/policy"):
		return "AddPermission"
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/policy"):
		return "GetPolicy"
	case r.Method == http.MethodDelete && strings.Contains(path, "/policy/"):
		return "RemovePermission"
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
