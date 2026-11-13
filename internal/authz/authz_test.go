package authz

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/iampolicy"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/services/iam"
	"github.com/stratus/internal/services/sts"
)

var acct = scope.Scope{Account: "111111111111", Region: "eu-west-1"}

func TestParseMode(t *testing.T) {
	for raw, want := range map[string]Mode{"": Off, "off": Off, "AUDIT": Audit, " enforce ": Enforce} {
		if got, err := ParseMode(raw); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v", raw, got, err)
		}
	}
	if _, err := ParseMode("strict"); err == nil {
		t.Error("an unknown mode must be rejected")
	}
}

func TestAction(t *testing.T) {
	cases := []struct {
		service, operation, method, target, key string
		want                                    string
	}{
		{"sqs", "SendMessage", "POST", "/", "", "sqs:SendMessage"},
		{"sqs", "SendMessageBatch", "POST", "/", "", "sqs:SendMessage"},
		{"sqs", "DeleteMessageBatch", "POST", "/", "", "sqs:DeleteMessage"},
		{"sqs", "ChangeMessageVisibilityBatch", "POST", "/", "", "sqs:ChangeMessageVisibility"},
		{"stepfunctions", "StartExecution", "POST", "/", "", "states:StartExecution"},
		{"monitoring", "PutMetricData", "POST", "/", "", "cloudwatch:PutMetricData"},
		{"dynamodbstreams", "GetRecords", "POST", "/", "", "dynamodb:GetRecords"},
		{"s3", "ListBuckets", "GET", "/", "", "s3:ListAllMyBuckets"},
		{"s3", "ListObjectsV2", "GET", "/b?list-type=2", "", "s3:ListBucket"},
		{"s3", "HeadObject", "HEAD", "/b/k", "k", "s3:GetObject"},
		{"s3", "CopyObject", "PUT", "/b/k", "k", "s3:PutObject"},
		{"s3", "", "PUT", "/b?policy", "", "s3:PutBucketPolicy"},
		{"s3", "", "GET", "/b?versioning", "", "s3:GetBucketVersioning"},
		{"s3", "", "POST", "/b?delete", "", "s3:DeleteObject"},
		{"s3", "", "PUT", "/b/k?tagging", "k", "s3:PutObjectTagging"},
		{"s3", "", "PUT", "/b/k?retention", "k", "s3:PutObjectRetention"},
		{"s3", "", "GET", "/b/k?legal-hold", "k", "s3:GetObjectLegalHold"},
		{"s3", "", "PUT", "/b?object-lock", "", "s3:PutBucketObjectLockConfiguration"},
		{"s3", "DeleteObject", "DELETE", "/b/k?versionId=v1", "k", "s3:DeleteObjectVersion"},
		{"s3", "GetObject", "GET", "/b/k?versionId=v1", "k", "s3:GetObjectVersion"},
		{"apigateway", "", "POST", "/restapis", "", "apigateway:POST"},
		{"ecs", "", "POST", "/", "", "ecs:*"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if got := Action(tc.service, tc.operation, r, tc.key); got != tc.want {
			t.Errorf("Action(%s, %s %s) = %q, want %q", tc.service, tc.method, tc.target, got, tc.want)
		}
	}
}

func jsonParams(body string) params {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return readParams(r, true)
}

func TestResource(t *testing.T) {
	get := func(target string) *http.Request { return httptest.NewRequest(http.MethodGet, target, nil) }
	cases := []struct {
		name, service, operation string
		r                        *http.Request
		bucket, key              string
		p                        params
		want                     string
	}{
		{"s3 object", "s3", "GetObject", get("/b/k"), "b", "dir/k", params{}, "arn:aws:s3:::b/dir/k"},
		{"s3 bucket", "s3", "ListObjectsV2", get("/b"), "b", "", params{}, "arn:aws:s3:::b"},
		{"s3 list buckets", "s3", "ListBuckets", get("/"), "", "", params{}, iampolicy.AnyResource},
		{"sqs url", "sqs", "SendMessage", get("/"), "", "", jsonParams(`{"QueueUrl":"http://localhost:4566/111111111111/orders"}`), "arn:aws:sqs:eu-west-1:111111111111:orders"},
		{"sqs create", "sqs", "CreateQueue", get("/"), "", "", jsonParams(`{"QueueName":"orders"}`), "arn:aws:sqs:eu-west-1:111111111111:orders"},
		{"dynamodb", "dynamodb", "PutItem", get("/"), "", "", jsonParams(`{"TableName":"users"}`), "arn:aws:dynamodb:eu-west-1:111111111111:table/users"},
		{"lambda path", "lambda", "Invoke", get("/2015-03-31/functions/my-fn/invocations"), "", "", params{}, "arn:aws:lambda:eu-west-1:111111111111:function:my-fn"},
		{"lambda create", "lambda", "CreateFunction", httptest.NewRequest(http.MethodPost, "/2015-03-31/functions", nil), "", "", jsonParams(`{"FunctionName":"f"}`), "arn:aws:lambda:eu-west-1:111111111111:function:f"},
		{"ssm", "ssm", "GetParameter", get("/"), "", "", jsonParams(`{"Name":"/app/db"}`), "arn:aws:ssm:eu-west-1:111111111111:parameter/app/db"},
		{"logs stream", "logs", "PutLogEvents", get("/"), "", "", jsonParams(`{"logGroupName":"/aws/lambda/f","logStreamName":"s"}`), "arn:aws:logs:eu-west-1:111111111111:log-group:/aws/lambda/f:log-stream:s"},
		{"kms alias", "kms", "Encrypt", get("/"), "", "", jsonParams(`{"KeyId":"alias/app"}`), iampolicy.AnyResource},
		{"iam role", "iam", "GetRole", get("/?RoleName=deployer"), "", "", readParams(get("/?RoleName=deployer"), false), "arn:aws:iam::111111111111:role/deployer"},
		{"sts", "sts", "AssumeRole", get("/?RoleArn=arn:aws:iam::2:role/r"), "", "", readParams(get("/?RoleArn=arn:aws:iam::2:role/r"), false), "arn:aws:iam::2:role/r"},
	}
	for _, tc := range cases {
		if got := Resource(tc.service, tc.operation, tc.r, acct, tc.bucket, tc.key, tc.p); got != tc.want {
			t.Errorf("%s: Resource = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadParamsKeepsTheBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"TableName":"t"}`))
	_ = readParams(r, true)
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, r.Body); err != nil || buf.String() != `{"TableName":"t"}` {
		t.Fatalf("body after inspection = %q, %v", buf.String(), err)
	}
}

type fakeSessions map[string]sts.Session

func (f fakeSessions) Session(k string) (sts.Session, bool) { s, ok := f[k]; return s, ok }

type fakeKeys map[string]iam.AccessKey

func (f fakeKeys) Lookup(k string) (iam.AccessKey, bool) { a, ok := f[k]; return a, ok }

type fakePolicies struct {
	users, roles map[string][]string
	trust        map[string]string
}

func (f fakePolicies) UserPolicies(account, name string) ([]string, error) {
	return f.users[account+"/"+name], nil
}
func (f fakePolicies) RolePolicies(account, name string) ([]string, error) {
	return f.roles[account+"/"+name], nil
}

func (f fakePolicies) TrustPolicy(account, name string) (string, bool, error) {
	doc, ok := f.trust[account+"/"+name]
	return doc, ok, nil
}

// fakeResources keys resource policies by bucket ARN.
type fakeResources map[string]string

func (f fakeResources) ResourcePolicy(_ scope.Scope, service, arn string) (string, bool, error) {
	if service != "s3" {
		return "", false, nil
	}
	bucket := arn
	if i := strings.Index(strings.TrimPrefix(arn, "arn:aws:s3:::"), "/"); i >= 0 {
		bucket = arn[:len("arn:aws:s3:::")+i]
	}
	doc, ok := f[bucket]
	return doc, ok, nil
}

func newAuthorizer(mode Mode) *Authorizer {
	callers := Callers{
		Sessions: fakeSessions{"ASIAREADER": {Account: "111111111111", RoleName: "reader", RoleARN: "arn:aws:iam::111111111111:role/reader", SessionName: "s", RoleID: "AROAX", Expiration: time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)}},
		Keys: fakeKeys{
			"AKIADEV":      {Account: "111111111111", UserName: "dev", UserARN: "arn:aws:iam::111111111111:user/dev", Status: "Active"},
			"AKIAINACTIVE": {Account: "111111111111", UserName: "dev", UserARN: "arn:aws:iam::111111111111:user/dev", Status: "Inactive"},
		},
	}
	policies := fakePolicies{
		users: map[string][]string{"111111111111/dev": {
			`{"Statement":[{"Effect":"Allow","Action":"sqs:*","Resource":"arn:aws:sqs:*:111111111111:app-*"},{"Effect":"Deny","Action":"sqs:DeleteQueue","Resource":"*"}]}`,
		}},
		roles: map[string][]string{"111111111111/reader": {`{"Statement":[{"Effect":"Allow","Action":"s3:Get*","Resource":"*"}]}`}},
		trust: map[string]string{
			"111111111111/reader":   `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::111111111111:user/dev"},"Action":"sts:AssumeRole"}]}`,
			"222222222222/deployer": `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"111111111111"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:RequestedRegion":"eu-west-1"}}}]}`,
		},
	}
	a := New(mode, callers, policies, fakeResources{
		"arn:aws:s3:::shared": `{"Statement":[
			{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::111111111111:user/dev"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/*"},
			{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::shared/*"}]}`,
	}, nil)
	a.now = func() time.Time { return time.Date(2026, 11, 11, 0, 30, 0, 0, time.UTC) }
	return a
}

func call(a *Authorizer, key, service, operation string, protocol httpapi.Protocol, body string) error {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	sc := acct
	if scope.ValidAccount(key) {
		sc.Account = key // the router acts in a twelve-digit key's account
	}
	md := httpapi.RequestMetadata{
		Classification: httpapi.Classification{Protocol: protocol, Service: service, Operation: operation},
		SigV4:          &awscompat.SigV4Identity{AccessKeyID: key, Region: acct.Region},
		Scope:          sc,
	}
	return a.Authorize(r, md)
}

func code(err error) string {
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestAuthorizerEnforce(t *testing.T) {
	a := newAuthorizer(Enforce)
	cases := []struct {
		name, key, service, operation string
		protocol                      httpapi.Protocol
		body                          string
		want                          string
	}{
		{"root is never refused", "111111111111", "iam", "CreateUser", httpapi.ProtocolQuery, "", ""},
		{"default key is root", "test", "sqs", "DeleteQueue", httpapi.ProtocolJSON, "", ""},
		{"allowed by resource pattern", "AKIADEV", "sqs", "SendMessage", httpapi.ProtocolJSON, `{"QueueUrl":"http://h/111111111111/app-orders"}`, ""},
		{"outside the resource pattern", "AKIADEV", "sqs", "SendMessage", httpapi.ProtocolJSON, `{"QueueUrl":"http://h/111111111111/billing"}`, "AccessDeniedException"},
		{"explicit deny", "AKIADEV", "sqs", "DeleteQueue", httpapi.ProtocolJSON, `{"QueueUrl":"http://h/111111111111/app-orders"}`, "AccessDeniedException"},
		{"query protocol code", "AKIADEV", "sns", "CreateTopic", httpapi.ProtocolQuery, "Action=CreateTopic&Name=t", "AccessDenied"},
		{"caller identity is always allowed", "AKIADEV", "sts", "GetCallerIdentity", httpapi.ProtocolQuery, "", ""},
		{"inactive key", "AKIAINACTIVE", "sqs", "SendMessage", httpapi.ProtocolJSON, `{"QueueUrl":"http://h/111111111111/app-orders"}`, "InvalidClientTokenId"},
		{"assumed role allowed", "ASIAREADER", "s3", "GetObject", httpapi.ProtocolS3, "", ""},
		{"assumed role refused", "ASIAREADER", "s3", "PutObject", httpapi.ProtocolS3, "", "AccessDenied"},
		{"internal requests pass", "AKIADEV", "stratus", "Health", httpapi.ProtocolInternal, "", ""},
	}
	for _, tc := range cases {
		if got := code(call(a, tc.key, tc.service, tc.operation, tc.protocol, tc.body)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAuthorizerExpiredSession(t *testing.T) {
	a := newAuthorizer(Enforce)
	a.now = func() time.Time { return time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC) }
	if got := code(call(a, "ASIAREADER", "s3", "GetObject", httpapi.ProtocolS3, "")); got != "ExpiredToken" {
		t.Fatalf("got %q", got)
	}
}

func TestAuthorizerMessageNamesThePrincipal(t *testing.T) {
	err := call(newAuthorizer(Enforce), "AKIADEV", "sqs", "SendMessage", httpapi.ProtocolJSON, `{"QueueUrl":"http://h/111111111111/billing"}`)
	msg := err.Error()
	for _, want := range []string{"arn:aws:iam::111111111111:user/dev", "sqs:SendMessage", "arn:aws:sqs:eu-west-1:111111111111:billing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestAuditAndOffNeverRefuse(t *testing.T) {
	for _, mode := range []Mode{Audit, Off} {
		if err := call(newAuthorizer(mode), "AKIADEV", "sqs", "DeleteQueue", httpapi.ProtocolJSON, `{}`); err != nil {
			t.Errorf("%s refused: %v", mode, err)
		}
	}
}

func s3call(a *Authorizer, key, operation, method, path, bucket, objectKey string) error {
	r := httptest.NewRequest(method, path, nil)
	md := httpapi.RequestMetadata{
		Classification: httpapi.Classification{Protocol: httpapi.ProtocolS3, Service: "s3", Operation: operation, Bucket: bucket, Key: objectKey},
		SigV4:          &awscompat.SigV4Identity{AccessKeyID: key, Region: acct.Region},
		Scope:          acct,
	}
	return a.Authorize(r, md)
}

func TestResourcePoliciesGrantAndDeny(t *testing.T) {
	a := newAuthorizer(Enforce)
	// dev has no S3 permissions of its own: the bucket policy grants reads.
	if got := code(s3call(a, "AKIADEV", "GetObject", http.MethodGet, "/shared/a", "shared", "a")); got != "" {
		t.Errorf("bucket policy grant: %q", got)
	}
	if got := code(s3call(a, "AKIADEV", "PutObject", http.MethodPut, "/shared/a", "shared", "a")); got != "AccessDenied" {
		t.Errorf("not granted by either policy: %q", got)
	}
	if got := code(s3call(a, "AKIADEV", "GetObject", http.MethodGet, "/other/a", "other", "a")); got != "AccessDenied" {
		t.Errorf("a bucket without a policy: %q", got)
	}
	// The reader role may read any object, but the bucket policy denies deletes to everyone.
	if got := code(s3call(a, "ASIAREADER", "GetObject", http.MethodGet, "/shared/a", "shared", "a")); got != "" {
		t.Errorf("identity grant: %q", got)
	}
	err := s3call(a, "ASIAREADER", "DeleteObject", http.MethodDelete, "/shared/a", "shared", "a")
	if code(err) != "AccessDenied" || !strings.Contains(err.Error(), "resource-based policy") {
		t.Errorf("resource policy deny: %v", err)
	}
	// The root is never refused.
	if got := code(s3call(a, "111111111111", "DeleteObject", http.MethodDelete, "/shared/a", "shared", "a")); got != "" {
		t.Errorf("root: %q", got)
	}
}

func assumeRoleCall(a *Authorizer, key, roleARN string) error {
	return call(a, key, "sts", "AssumeRole", httpapi.ProtocolQuery, "Action=AssumeRole&RoleSessionName=s&RoleArn="+roleARN)
}

func TestTrustPolicies(t *testing.T) {
	a := newAuthorizer(Enforce)
	cases := []struct {
		name, key, role, want string
	}{
		{"trusted user", "AKIADEV", "arn:aws:iam::111111111111:role/reader", "AccessDenied"},
		{"untrusted root", "111111111111", "arn:aws:iam::111111111111:role/reader", "AccessDenied"},
		{"account trust with a condition", "111111111111", "arn:aws:iam::222222222222:role/deployer", ""},
		{"another account", "333333333333", "arn:aws:iam::222222222222:role/deployer", "AccessDenied"},
		{"missing role", "111111111111", "arn:aws:iam::222222222222:role/missing", "AccessDenied"},
	}
	for _, tc := range cases {
		if got := code(assumeRoleCall(a, tc.key, tc.role)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGovernanceBypassNeedsItsOwnPermission(t *testing.T) {
	callers := Callers{Keys: fakeKeys{
		"AKIAOPS":   {Account: "111111111111", UserName: "ops", UserARN: "arn:aws:iam::111111111111:user/ops", Status: "Active"},
		"AKIAADMIN": {Account: "111111111111", UserName: "admin", UserARN: "arn:aws:iam::111111111111:user/admin", Status: "Active"},
	}}
	policies := fakePolicies{users: map[string][]string{
		"111111111111/ops":   {`{"Statement":[{"Effect":"Allow","Action":"s3:DeleteObjectVersion","Resource":"*"}]}`},
		"111111111111/admin": {`{"Statement":[{"Effect":"Allow","Action":["s3:DeleteObjectVersion","s3:BypassGovernanceRetention"],"Resource":"*"}]}`},
	}}
	a := New(Enforce, callers, policies, nil, nil)
	deleteVersion := func(key string, bypass bool) error {
		r := httptest.NewRequest(http.MethodDelete, "/audit/record?versionId=v1", nil)
		if bypass {
			r.Header.Set("X-Amz-Bypass-Governance-Retention", "true")
		}
		return a.Authorize(r, httpapi.RequestMetadata{
			Classification: httpapi.Classification{Protocol: httpapi.ProtocolS3, Service: "s3", Operation: "DeleteObject", Bucket: "audit", Key: "record"},
			SigV4:          &awscompat.SigV4Identity{AccessKeyID: key, Region: acct.Region},
			Scope:          acct,
		})
	}
	if err := deleteVersion("AKIAOPS", false); err != nil {
		t.Fatalf("delete without bypass: %v", err)
	}
	if got := code(deleteVersion("AKIAOPS", true)); got != "AccessDenied" {
		t.Fatalf("bypass without s3:BypassGovernanceRetention: got %q", got)
	}
	if err := deleteVersion("AKIAADMIN", true); err != nil {
		t.Fatalf("bypass with permission: %v", err)
	}
}
