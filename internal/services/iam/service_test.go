package iam

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
	"github.com/stratus/internal/store/bbolt"
)

type fixture struct {
	t    *testing.T
	svc  *Service
	keys *KeyDirectory
	meta store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	meta, err := bbolt.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	keys := NewKeyDirectory(meta)
	svc := NewService(store.Scoped(meta, "@111111111111/us-east-1/"), keys)
	svc.SetScope(scope.Scope{Account: "111111111111", Region: "us-east-1"})
	return &fixture{t: t, svc: svc, keys: keys, meta: meta}
}

// call runs an IAM action and returns the response body or the API error.
func (f *fixture) call(action string, params ...string) (string, error) {
	f.t.Helper()
	form := url.Values{"Action": {action}, "Version": {"2010-05-08"}}
	for i := 0; i+1 < len(params); i += 2 {
		form.Set(params[i], params[i+1])
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	if err := f.svc.Handle(w, r, action, "req"); err != nil {
		return "", err
	}
	return w.Body.String(), nil
}

func (f *fixture) must(action string, params ...string) string {
	f.t.Helper()
	out, err := f.call(action, params...)
	if err != nil {
		f.t.Fatalf("%s: %v", action, err)
	}
	return out
}

func (f *fixture) mustFail(code, action string, params ...string) {
	f.t.Helper()
	_, err := f.call(action, params...)
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		f.t.Fatalf("%s: err = %v, want %s", action, err, code)
	}
}

const allowSQS = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sqs:*","Resource":"*"}]}`

func TestUserLifecycle(t *testing.T) {
	f := newFixture(t)
	out := f.must("CreateUser", "UserName", "alice", "Path", "/team/")
	if !strings.Contains(out, "<Arn>arn:aws:iam::111111111111:user/team/alice</Arn>") || !strings.Contains(out, "<UserId>AIDA") {
		t.Fatalf("CreateUser: %s", out)
	}
	f.mustFail("EntityAlreadyExists", "CreateUser", "UserName", "alice")
	if out := f.must("ListUsers"); !strings.Contains(out, "<UserName>alice</UserName>") {
		t.Fatalf("ListUsers: %s", out)
	}
	if out := f.must("ListUsers", "PathPrefix", "/other/"); strings.Contains(out, "alice") {
		t.Fatalf("ListUsers with another path prefix: %s", out)
	}
	f.must("GetUser", "UserName", "alice")
	f.must("DeleteUser", "UserName", "alice")
	f.mustFail("NoSuchEntity", "GetUser", "UserName", "alice")
}

func TestAccessKeysAreIndexedAcrossAccounts(t *testing.T) {
	f := newFixture(t)
	f.must("CreateUser", "UserName", "ci")
	var created struct {
		Key struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
		} `xml:"CreateAccessKeyResult>AccessKey"`
	}
	if err := xml.Unmarshal([]byte(f.must("CreateAccessKey", "UserName", "ci")), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Key.AccessKeyID
	if !strings.HasPrefix(id, "AKIA") || len(id) != 20 || len(created.Key.SecretAccessKey) != 40 {
		t.Fatalf("access key = %+v", created.Key)
	}

	account, ok := f.keys.AccountFor(id)
	if !ok || account != "111111111111" {
		t.Fatalf("AccountFor = %q, %v", account, ok)
	}
	acct, arn, userID, ok := f.keys.UserFor(id)
	if !ok || acct != "111111111111" || arn != "arn:aws:iam::111111111111:user/ci" || !strings.HasPrefix(userID, "AIDA") {
		t.Fatalf("UserFor = %q %q %q %v", acct, arn, userID, ok)
	}

	if out := f.must("ListAccessKeys", "UserName", "ci"); !strings.Contains(out, id) || strings.Contains(out, created.Key.SecretAccessKey) {
		t.Fatalf("ListAccessKeys must list the key without its secret: %s", out)
	}
	f.must("CreateAccessKey", "UserName", "ci")
	f.mustFail("LimitExceeded", "CreateAccessKey", "UserName", "ci")

	f.must("UpdateAccessKey", "UserName", "ci", "AccessKeyId", id, "Status", "Inactive")
	if key, _ := f.keys.Lookup(id); key.Status != "Inactive" {
		t.Fatalf("status = %q", key.Status)
	}
	f.mustFail("DeleteConflict", "DeleteUser", "UserName", "ci")
	f.must("DeleteAccessKey", "UserName", "ci", "AccessKeyId", id)
	if _, ok := f.keys.Lookup(id); ok {
		t.Fatal("deleted key still resolves")
	}
}

func TestAnotherAccountCannotTouchAKey(t *testing.T) {
	f := newFixture(t)
	f.must("CreateUser", "UserName", "ci")
	var created struct {
		ID string `xml:"CreateAccessKeyResult>AccessKey>AccessKeyId"`
	}
	_ = xml.Unmarshal([]byte(f.must("CreateAccessKey", "UserName", "ci")), &created)

	other := NewService(store.Scoped(f.meta, "@222222222222/us-east-1/"), f.keys)
	other.SetScope(scope.Scope{Account: "222222222222", Region: "us-east-1"})
	f.svc = other
	f.must("CreateUser", "UserName", "ci")
	f.mustFail("NoSuchEntity", "DeleteAccessKey", "UserName", "ci", "AccessKeyId", created.ID)
}

func TestManagedPolicyVersionsAndAttachments(t *testing.T) {
	f := newFixture(t)
	out := f.must("CreatePolicy", "PolicyName", "queues", "Path", "/app/", "PolicyDocument", allowSQS)
	arn := "arn:aws:iam::111111111111:policy/app/queues"
	if !strings.Contains(out, "<Arn>"+arn+"</Arn>") || !strings.Contains(out, "<DefaultVersionId>v1</DefaultVersionId>") {
		t.Fatalf("CreatePolicy: %s", out)
	}
	f.mustFail("NoSuchEntity", "GetPolicy", "PolicyArn", "arn:aws:iam::111111111111:policy/queues")

	f.must("CreatePolicyVersion", "PolicyArn", arn, "PolicyDocument", `{"Statement":[]}`, "SetAsDefault", "true")
	if out := f.must("GetPolicy", "PolicyArn", arn); !strings.Contains(out, "<DefaultVersionId>v2</DefaultVersionId>") {
		t.Fatalf("GetPolicy after a new default version: %s", out)
	}
	f.mustFail("DeleteConflict", "DeletePolicyVersion", "PolicyArn", arn, "VersionId", "v2")
	f.must("SetDefaultPolicyVersion", "PolicyArn", arn, "VersionId", "v1")
	f.must("DeletePolicyVersion", "PolicyArn", arn, "VersionId", "v2")
	if out := f.must("ListPolicyVersions", "PolicyArn", arn); strings.Contains(out, "v2") {
		t.Fatalf("v2 should be gone: %s", out)
	}

	f.must("CreateRole", "RoleName", "worker", "AssumeRolePolicyDocument", `{"Statement":[]}`)
	f.must("AttachRolePolicy", "RoleName", "worker", "PolicyArn", arn)
	f.must("AttachRolePolicy", "RoleName", "worker", "PolicyArn", "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole")
	f.mustFail("NoSuchEntity", "AttachRolePolicy", "RoleName", "worker", "PolicyArn", "arn:aws:iam::111111111111:policy/missing")

	out = f.must("ListAttachedRolePolicies", "RoleName", "worker")
	if !strings.Contains(out, "<PolicyName>queues</PolicyName>") || !strings.Contains(out, "<PolicyName>AWSLambdaBasicExecutionRole</PolicyName>") {
		t.Fatalf("ListAttachedRolePolicies: %s", out)
	}
	if out := f.must("GetPolicy", "PolicyArn", arn); !strings.Contains(out, "<AttachmentCount>1</AttachmentCount>") {
		t.Fatalf("attachment count: %s", out)
	}
	if out := f.must("ListPolicies", "Scope", "Local", "OnlyAttached", "true"); !strings.Contains(out, arn) {
		t.Fatalf("ListPolicies: %s", out)
	}
	f.mustFail("DeleteConflict", "DeletePolicy", "PolicyArn", arn)
	f.mustFail("DeleteConflict", "DeleteRole", "RoleName", "worker")

	f.must("DetachRolePolicy", "RoleName", "worker", "PolicyArn", arn)
	f.mustFail("NoSuchEntity", "DetachRolePolicy", "RoleName", "worker", "PolicyArn", arn)
	f.must("DeletePolicy", "PolicyArn", arn)
}

func TestAWSManagedPolicies(t *testing.T) {
	f := newFixture(t)
	arn := "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"
	if out := f.must("GetPolicy", "PolicyArn", arn); !strings.Contains(out, "<PolicyName>AmazonS3ReadOnlyAccess</PolicyName>") {
		t.Fatalf("GetPolicy: %s", out)
	}
	if out := f.must("GetPolicyVersion", "PolicyArn", arn, "VersionId", "v1"); !strings.Contains(out, "s3%3AGet") {
		t.Fatalf("GetPolicyVersion: %s", out)
	}
	if out := f.must("ListPolicies", "Scope", "AWS"); !strings.Contains(out, "AdministratorAccess") {
		t.Fatalf("ListPolicies: %s", out)
	}
}

func TestIdentityPoliciesCombineInlineAndAttached(t *testing.T) {
	f := newFixture(t)
	f.must("CreateUser", "UserName", "alice")
	f.must("PutUserPolicy", "UserName", "alice", "PolicyName", "inline", "PolicyDocument", allowSQS)
	if out := f.must("ListUserPolicies", "UserName", "alice"); !strings.Contains(out, "<member>inline</member>") {
		t.Fatalf("ListUserPolicies: %s", out)
	}
	if out := f.must("GetUserPolicy", "UserName", "alice", "PolicyName", "inline"); !strings.Contains(out, "sqs%3A%2A") {
		t.Fatalf("GetUserPolicy: %s", out)
	}
	f.must("CreatePolicy", "PolicyName", "tables", "PolicyDocument", `{"Statement":[{"Effect":"Allow","Action":"dynamodb:*","Resource":"*"}]}`)
	f.must("AttachUserPolicy", "UserName", "alice", "PolicyArn", "arn:aws:iam::111111111111:policy/tables")
	f.must("AttachUserPolicy", "UserName", "alice", "PolicyArn", "arn:aws:iam::aws:policy/AdministratorAccess")
	f.must("AttachUserPolicy", "UserName", "alice", "PolicyArn", "arn:aws:iam::aws:policy/SomethingStratusDoesNotKnow")

	docs, err := f.svc.UserPolicies("alice")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(docs, "\n")
	if len(docs) != 3 || !strings.Contains(joined, "sqs:*") || !strings.Contains(joined, "dynamodb:*") || !strings.Contains(joined, `"Action":"*"`) {
		t.Fatalf("user policies = %v", docs)
	}

	f.must("CreateRole", "RoleName", "r", "AssumeRolePolicyDocument", `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`)
	f.must("PutRolePolicy", "RoleName", "r", "PolicyName", "p", "PolicyDocument", allowSQS)
	if docs, _ := f.svc.RolePolicies("r"); len(docs) != 1 || !strings.Contains(docs[0], "sqs:*") {
		t.Fatalf("role policies = %v", docs)
	}
	trust, ok, err := f.svc.TrustPolicy("r")
	if err != nil || !ok || !strings.Contains(trust, "sts:AssumeRole") {
		t.Fatalf("trust policy = %q %v %v", trust, ok, err)
	}
	if _, ok, _ := f.svc.TrustPolicy("missing"); ok {
		t.Fatal("a missing role has no trust policy")
	}

	f.mustFail("DeleteConflict", "DeleteUser", "UserName", "alice")
}
