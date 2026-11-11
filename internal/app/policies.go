package app

import (
	"strings"

	"github.com/stratus/internal/scope"
)

// TrustPolicy returns a role's trust policy, for the authorizer.
func (s *Stacks) TrustPolicy(account, roleName string) (string, bool, error) {
	return s.iam(account).TrustPolicy(roleName)
}

// ResourcePolicy returns the resource-based policy of the resource an ARN
// names in scope sc: a bucket policy, a queue or topic Policy attribute, or
// a Lambda function's permissions.
func (s *Stacks) ResourcePolicy(sc scope.Scope, service, arn string) (string, bool, error) {
	services := s.For(sc)
	switch service {
	case "s3":
		bucket := strings.TrimPrefix(arn, "arn:aws:s3:::")
		if i := strings.Index(bucket, "/"); i >= 0 {
			bucket = bucket[:i]
		}
		return services.S3.BucketPolicy(bucket)
	case "sqs":
		return services.SQS.QueuePolicy(arn[strings.LastIndex(arn, ":")+1:])
	case "sns":
		return services.SNS.TopicPolicy(arn)
	case "lambda":
		name, ok := lambdaFunctionFromARN(arn)
		if !ok {
			return "", false, nil
		}
		return services.Lambda.FunctionPolicy(name)
	}
	return "", false, nil
}

// lambdaFunctionFromARN returns the function name of
// arn:aws:lambda:region:account:function:name[:qualifier].
func lambdaFunctionFromARN(arn string) (string, bool) {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 || parts[5] != "function" {
		return "", false
	}
	return parts[6], true
}
