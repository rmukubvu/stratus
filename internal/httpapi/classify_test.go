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
