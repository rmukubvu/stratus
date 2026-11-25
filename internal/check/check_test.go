package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratus/internal/awsmodel"
)

func TestSnakeCaseMatchesBoto3MethodNames(t *testing.T) {
	cases := map[string]string{
		"CreateQueue":                "create_queue",
		"ListTagsForResource":        "list_tags_for_resource",
		"DescribeDBInstances":        "describe_db_instances",
		"GetQueueUrl":                "get_queue_url",
		"PutBucketAcl":               "put_bucket_acl",
		"AssumeRoleWithSAML":         "assume_role_with_saml",
		"CreateVpcEndpoint":          "create_vpc_endpoint",
		"AdminInitiateAuth":          "admin_initiate_auth",
		"InvokeModel":                "invoke_model",
		"GetSMSAttributes":           "get_sms_attributes",
		"DescribeDBClusterSnapshots": "describe_db_cluster_snapshots",
	}
	for in, want := range cases {
		if got := snake(in); got != want {
			t.Errorf("snake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPythonCallsAreAttributedToTheClientsTheFileCreates(t *testing.T) {
	src := []byte(`import boto3
sqs = boto3.client("sqs", endpoint_url=URL)
sqs.send_message(QueueUrl=q, MessageBody="x")
sqs.add_permission(QueueUrl=q, Label="l")
glue = boto3.client('glue')
`)
	calls := Python{}.Scan("app.py", src)
	got := map[string]int{}
	for _, c := range calls {
		got[c.Service+"."+c.Operation] = c.Location.Line
	}
	if got["sqs.SendMessage"] != 3 || got["sqs.AddPermission"] != 4 || got["glue.*"] != 5 {
		t.Fatalf("calls = %v", got)
	}
}

func TestJavaScriptCommandsCountWhenTheirClientIsImported(t *testing.T) {
	src := []byte(`import { SQSClient, SendMessageCommand } from "@aws-sdk/client-sqs";
import { DynamoDBDocumentClient, PutCommand } from "@aws-sdk/lib-dynamodb";
await sqs.send(new SendMessageCommand({ QueueUrl }));
await doc.send(new PutCommand({ TableName, Item }));
await x.send(new UnrelatedCommand());
`)
	calls := JavaScript{}.Scan("app.ts", src)
	got := map[string]bool{}
	for _, c := range calls {
		got[c.Service+"."+c.Operation] = true
	}
	if !got["sqs.SendMessage"] || !got["dynamodb.PutItem"] || len(got) != 2 {
		t.Fatalf("calls = %v", got)
	}
}

func TestGoCallsAreFoundThroughTheirInputTypes(t *testing.T) {
	src := []byte(`package app

import (
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	logs "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

func f() {
	c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: q})
	l.CreateLogGroup(ctx, &logs.CreateLogGroupInput{})
}
`)
	calls := Go{}.Scan("app.go", src)
	got := map[string]bool{}
	for _, c := range calls {
		got[c.Service+"."+c.Operation] = true
	}
	if !got["sqs.SendMessage"] || !got["logs.CreateLogGroup"] {
		t.Fatalf("calls = %v", got)
	}
}

func TestAssessJoinsCallsWithStratusCoverage(t *testing.T) {
	findings := Assess([]Call{
		{Service: "sqs", Operation: "SendMessage", Location: Location{"a.py", 2}},
		{Service: "sqs", Operation: "SendMessage", Location: Location{"a.py", 1}},
		{Service: "sqs", Operation: "AddPermission", Location: Location{"a.py", 3}},
		{Service: "cognito-idp", Operation: "AdminInitiateAuth", Location: Location{"b.py", 1}},
		{Service: "glue", Operation: "*", Location: Location{"c.py", 1}},
	})
	got := map[string]awsmodel.Support{}
	for _, f := range findings {
		got[f.Service+"."+f.Operation] = f.Support
	}
	want := map[string]awsmodel.Support{
		"sqs.SendMessage":              awsmodel.Implemented,
		"sqs.AddPermission":            awsmodel.NotImplemented,
		"cognitoidp.AdminInitiateAuth": awsmodel.Lookup("cognitoidp", "AdminInitiateAuth"),
		"glue.*":                       awsmodel.UnknownService,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	for _, f := range findings {
		if f.Operation == "SendMessage" && (len(f.Locations) != 2 || f.Locations[0].Line != 1) {
			t.Errorf("SendMessage locations = %v, want both, sorted", f.Locations)
		}
	}
}

func TestRunScansAProjectButNotItsDependencies(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/app.py", "import boto3\nsqs = boto3.client('sqs')\nsqs.add_permission()\n")
	write("node_modules/x/index.js", "import '@aws-sdk/client-glue'\n")
	write("README.md", "boto3.client('glue')\n")

	report, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 1 || len(report.Findings) != 1 || report.OK() {
		t.Fatalf("report = %+v", report)
	}
	md := report.Markdown()
	if !strings.Contains(md, "## Not implemented") || !strings.Contains(md, "`sqs AddPermission` at src/app.py:3") {
		t.Fatalf("markdown:\n%s", md)
	}
}
