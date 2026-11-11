package iam

import "strings"

// awsManagedPrefix starts the ARN of every AWS managed policy.
const awsManagedPrefix = "arn:aws:iam::aws:policy/"

// awsManagedPolicies are the AWS managed policies Stratus knows the content
// of. Others can still be attached, so templates that reference them deploy,
// but they grant nothing when IAM enforcement is on.
var awsManagedPolicies = map[string]string{
	"AdministratorAccess":          `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`,
	"PowerUserAccess":              `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","NotAction":["iam:*","organizations:*","account:*"],"Resource":"*"},{"Effect":"Allow","Action":["iam:CreateServiceLinkedRole","iam:DeleteServiceLinkedRole","iam:ListRoles","organizations:DescribeOrganization","account:ListRegions"],"Resource":"*"}]}`,
	"ReadOnlyAccess":               `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["*:Get*","*:List*","*:Describe*","*:BatchGet*","*:Query","*:Scan","s3:Head*"],"Resource":"*"}]}`,
	"IAMFullAccess":                `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["iam:*","organizations:DescribeAccount","organizations:DescribeOrganization","organizations:DescribeOrganizationalUnit","organizations:DescribePolicy","organizations:ListChildren","organizations:ListParents","organizations:ListPoliciesForTarget","organizations:ListRoots","organizations:ListPolicies","organizations:ListTargetsForPolicy"],"Resource":"*"}]}`,
	"AmazonS3FullAccess":           `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*","s3-object-lambda:*"],"Resource":"*"}]}`,
	"AmazonS3ReadOnlyAccess":       `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:Get*","s3:List*","s3:Describe*","s3-object-lambda:Get*","s3-object-lambda:List*"],"Resource":"*"}]}`,
	"AmazonSQSFullAccess":          `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["sqs:*"],"Resource":"*"}]}`,
	"AmazonSQSReadOnlyAccess":      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["sqs:GetQueueAttributes","sqs:GetQueueUrl","sqs:ListDeadLetterSourceQueues","sqs:ListQueues","sqs:ListMessageMoveTasks","sqs:ListQueueTags"],"Resource":"*"}]}`,
	"AmazonSNSFullAccess":          `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["sns:*"],"Resource":"*"}]}`,
	"AmazonDynamoDBFullAccess":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["dynamodb:*"],"Resource":"*"}]}`,
	"AmazonDynamoDBReadOnlyAccess": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["dynamodb:BatchGetItem","dynamodb:Describe*","dynamodb:List*","dynamodb:GetItem","dynamodb:Query","dynamodb:Scan","dynamodb:PartiQLSelect"],"Resource":"*"}]}`,
	"AWSLambda_FullAccess":         `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["lambda:*","logs:*","iam:PassRole","iam:GetRole","iam:ListRoles"],"Resource":"*"}]}`,
	"AWSLambdaBasicExecutionRole":  `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`,
	"CloudWatchLogsFullAccess":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["logs:*"],"Resource":"*"}]}`,
	"SecretsManagerReadWrite":      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["secretsmanager:*","kms:Decrypt","kms:DescribeKey","kms:GenerateDataKey"],"Resource":"*"}]}`,
	"AmazonSSMReadOnlyAccess":      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ssm:Describe*","ssm:Get*","ssm:List*"],"Resource":"*"}]}`,
	"AWSStepFunctionsFullAccess":   `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["states:*"],"Resource":"*"}]}`,
	"AmazonEventBridgeFullAccess":  `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["events:*","schemas:*","scheduler:*","pipes:*"],"Resource":"*"}]}`,
	"AmazonKinesisFullAccess":      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["kinesis:*"],"Resource":"*"}]}`,
}

// awsManagedName returns the policy name of an AWS managed policy ARN,
// including ones under a path such as service-role/.
func awsManagedName(arn string) (string, bool) {
	rest, ok := strings.CutPrefix(arn, awsManagedPrefix)
	if !ok || rest == "" {
		return "", false
	}
	return rest[strings.LastIndex(rest, "/")+1:], true
}

// awsManagedDocument returns the document of a known AWS managed policy.
func awsManagedDocument(arn string) (string, bool) {
	name, ok := awsManagedName(arn)
	if !ok {
		return "", false
	}
	doc, ok := awsManagedPolicies[name]
	return doc, ok
}
