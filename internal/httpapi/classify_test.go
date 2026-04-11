package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestClassifyHealth(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://localhost:4566/_stratus/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "stratus" || got.Operation != "Health" || got.Protocol != ProtocolInternal {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifySTSQueryPost(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader("Action=GetCallerIdentity&Version=2011-06-15"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "sts" || got.Operation != "GetCallerIdentity" || got.Protocol != ProtocolQuery {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifySQSQueryPost(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader("Action=CreateQueue&Version=2012-11-05&QueueName=test"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260407/us-east-1/sqs/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "sqs" || got.Operation != "CreateQueue" || got.Protocol != ProtocolQuery {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyIAMQueryPost(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader("Action=CreateRole&Version=2010-05-08&RoleName=test&AssumeRolePolicyDocument=%7B%7D"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260407/us-east-1/iam/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "iam" || got.Operation != "CreateRole" || got.Protocol != ProtocolQuery {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyCloudFormationQueryPost(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader("Action=CreateStack&Version=2010-05-15&StackName=test&TemplateBody=%7B%7D"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260407/us-east-1/cloudformation/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "cloudformation" || got.Operation != "CreateStack" || got.Protocol != ProtocolQuery {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyKMSJSONTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(`{"Description":"test"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService.CreateKey")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "kms" || got.Operation != "CreateKey" || got.Protocol != ProtocolJSON {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifySNSQueryPost(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader("Action=CreateTopic&Version=2010-03-31&Name=test"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260407/us-east-1/sns/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "sns" || got.Operation != "CreateTopic" || got.Protocol != ProtocolQuery {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyEventBridgeJSONTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(`{"Name":"test-bus"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSEvents.CreateEventBus")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "events" || got.Operation != "CreateEventBus" || got.Protocol != ProtocolJSON {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifySecretsManagerJSONTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(`{"Name":"secret","SecretString":"value"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager.CreateSecret")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "secretsmanager" || got.Operation != "CreateSecret" || got.Protocol != ProtocolJSON {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyKinesisJSONTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(`{"StreamName":"demo","ShardCount":1}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Kinesis_20131202.CreateStream")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "kinesis" || got.Operation != "CreateStream" || got.Protocol != ProtocolJSON {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyCognitoIDPJSONTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(`{"PoolName":"demo"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService.CreateUserPool")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "cognitoidp" || got.Operation != "CreateUserPool" || got.Protocol != ProtocolJSON {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyStepFunctionsJSONTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(`{"name":"demo"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AWSStepFunctions.CreateStateMachine")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "stepfunctions" || got.Operation != "CreateStateMachine" || got.Protocol != ProtocolJSON {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyCloudWatchQueryPost(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader("Action=PutMetricData&Version=2010-08-01&Namespace=Test"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260407/us-east-1/monitoring/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "monitoring" || got.Operation != "PutMetricData" || got.Protocol != ProtocolQuery {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyS3ListBuckets(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://localhost:4566/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "s3" || got.Operation != "ListBuckets" || got.Protocol != ProtocolS3 {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyS3CreateBucket(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "http://localhost:4566/demo-bucket", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "s3" || got.Operation != "CreateBucket" || got.Bucket != "demo-bucket" || got.Protocol != ProtocolS3 {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyS3ListObjectsV2(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://localhost:4566/demo-bucket?list-type=2", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "s3" || got.Operation != "ListObjectsV2" || got.Bucket != "demo-bucket" || got.Protocol != ProtocolS3 {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyS3PutObject(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "http://localhost:4566/demo-bucket/path/to/file.json", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "s3" || got.Operation != "PutObject" || got.Bucket != "demo-bucket" || got.Key != "path/to/file.json" || got.Protocol != ProtocolS3 {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyCloudWatchJSONTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/", strings.NewReader(`{"Namespace":"Stratus/Test"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonCloudWatch.PutMetricData")

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "monitoring" || got.Operation != "PutMetricData" || got.Protocol != ProtocolJSON {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyLambdaLayerPublish(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/2018-10-31/layers/shared-lib/versions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "lambda" || got.Operation != "PublishLayerVersion" || got.Protocol != ProtocolREST {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyLambdaEventSourceMappingCreate(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/2015-03-31/event-source-mappings", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "lambda" || got.Operation != "CreateEventSourceMapping" || got.Protocol != ProtocolREST {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyAPIGatewayV2REST(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/v2/apis", strings.NewReader(`{"name":"test","protocolType":"HTTP"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "apigatewayv2" || got.Protocol != ProtocolREST {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyAPIGatewayREST(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://localhost:4566/restapis", strings.NewReader(`{"name":"test"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "apigateway" || got.Protocol != ProtocolREST {
		t.Fatalf("unexpected classification: %+v", got)
	}
}

func TestClassifyS3PathStyle(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "http://localhost:4566/my-bucket/path/to/object.txt", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	got, err := Classify(req)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got.Service != "s3" || got.Bucket != "my-bucket" || got.Key != "path/to/object.txt" {
		t.Fatalf("unexpected classification: %+v", got)
	}
}
