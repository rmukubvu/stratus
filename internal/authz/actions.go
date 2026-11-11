package authz

import (
	"net/http"
	"strings"
)

// actionPrefixes maps Stratus service names to IAM action prefixes where
// they differ.
var actionPrefixes = map[string]string{
	"monitoring":      "cloudwatch",
	"stepfunctions":   "states",
	"cognitoidp":      "cognito-idp",
	"apigatewayv2":    "apigateway",
	"elbv2":           "elasticloadbalancing",
	"dynamodbstreams": "dynamodb",
}

// s3Actions maps S3 API operations to the IAM actions that authorise them.
var s3Actions = map[string]string{
	"ListBuckets":             "ListAllMyBuckets",
	"ListObjectsV2":           "ListBucket",
	"ListObjects":             "ListBucket",
	"HeadBucket":              "ListBucket",
	"HeadObject":              "GetObject",
	"CopyObject":              "PutObject",
	"UploadPart":              "PutObject",
	"CreateMultipartUpload":   "PutObject",
	"CompleteMultipartUpload": "PutObject",
	"AbortMultipartUpload":    "AbortMultipartUpload",
}

// s3Subresources maps a bucket subresource and method to its IAM action.
var s3Subresources = map[string]map[string]string{
	"policy":       {http.MethodGet: "GetBucketPolicy", http.MethodPut: "PutBucketPolicy", http.MethodDelete: "DeleteBucketPolicy"},
	"versioning":   {http.MethodGet: "GetBucketVersioning", http.MethodPut: "PutBucketVersioning"},
	"tagging":      {http.MethodGet: "GetBucketTagging", http.MethodPut: "PutBucketTagging", http.MethodDelete: "PutBucketTagging"},
	"notification": {http.MethodGet: "GetBucketNotification", http.MethodPut: "PutBucketNotification"},
	"cors":         {http.MethodGet: "GetBucketCORS", http.MethodPut: "PutBucketCORS", http.MethodDelete: "PutBucketCORS"},
	"lifecycle":    {http.MethodGet: "GetLifecycleConfiguration", http.MethodPut: "PutLifecycleConfiguration", http.MethodDelete: "PutLifecycleConfiguration"},
	"location":     {http.MethodGet: "GetBucketLocation"},
	"acl":          {http.MethodGet: "GetBucketAcl", http.MethodPut: "PutBucketAcl"},
	"encryption":   {http.MethodGet: "GetEncryptionConfiguration", http.MethodPut: "PutEncryptionConfiguration", http.MethodDelete: "PutEncryptionConfiguration"},
	"uploads":      {http.MethodGet: "ListBucketMultipartUploads"},
	"versions":     {http.MethodGet: "ListBucketVersions"},
	"delete":       {http.MethodPost: "DeleteObject"},
}

// Action names the IAM action a request needs, e.g. "sqs:SendMessage".
// When Stratus cannot name the operation, the action is "<prefix>:*", so
// only a caller allowed the whole service may make it.
func Action(service, operation string, r *http.Request, objectKey string) string {
	prefix := service
	if p, ok := actionPrefixes[service]; ok {
		prefix = p
	}
	switch service {
	case "s3":
		operation = s3Action(operation, r, objectKey)
	case "apigateway", "apigatewayv2":
		// API Gateway's management actions are the HTTP verbs.
		operation = r.Method
	}
	if operation == "" {
		operation = "*"
	}
	return prefix + ":" + operation
}

func s3Action(operation string, r *http.Request, key string) string {
	query := r.URL.Query()
	if key == "" {
		for sub, methods := range s3Subresources {
			if query.Has(sub) {
				return methods[r.Method]
			}
		}
	} else if query.Has("tagging") {
		if r.Method == http.MethodGet {
			return "GetObjectTagging"
		}
		return "PutObjectTagging"
	}
	if mapped, ok := s3Actions[operation]; ok {
		return mapped
	}
	if operation != "" {
		return operation
	}
	if key == "" && r.Method == http.MethodGet {
		return "ListBucket"
	}
	return ""
}

// isUnauthenticated lists actions AWS answers for any valid credentials.
func isUnauthenticated(action string) bool {
	return strings.EqualFold(action, "sts:GetCallerIdentity")
}
