package sts

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	meta, err := bbolt.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	svc := NewService(meta, nil)
	svc.now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	return svc
}

func TestParseRoleARN(t *testing.T) {
	cases := []struct {
		arn     string
		ok      bool
		account string
		name    string
	}{
		{"arn:aws:iam::111111111111:role/deployer", true, "111111111111", "deployer"},
		{"arn:aws:iam::111111111111:role/service-role/team/app-runner", true, "111111111111", "app-runner"},
		{"arn:aws:iam::11111111111:role/short-account", false, "", ""},
		{"arn:aws:iam::111111111111:user/alice", false, "", ""},
		{"arn:aws:sts::111111111111:role/deployer", false, "", ""},
		{"", false, "", ""},
	}
	for _, tc := range cases {
		ref, ok := ParseRoleARN(tc.arn)
		if ok != tc.ok || ref.Account != tc.account || ref.Name != tc.name {
			t.Errorf("ParseRoleARN(%q) = %+v, %v", tc.arn, ref, ok)
		}
	}
}

func TestAssumeRoleIssuesCredentialsForTheRoleAccount(t *testing.T) {
	svc := newTestService(t)
	session, err := svc.AssumeRole(AssumeRoleInput{RoleARN: "arn:aws:iam::222222222222:role/ops/deployer", RoleSessionName: "ci-run"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(session.AccessKeyID, "ASIA") || len(session.AccessKeyID) != 20 {
		t.Fatalf("access key = %q", session.AccessKeyID)
	}
	if session.SecretAccessKey == "" || session.SessionToken == "" {
		t.Fatalf("missing secret or token: %+v", session)
	}
	if want := time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC); !session.Expiration.Equal(want) {
		t.Fatalf("expiration = %v, want %v", session.Expiration, want)
	}
	if got := session.AssumedRoleARN(); got != "arn:aws:sts::222222222222:assumed-role/deployer/ci-run" {
		t.Fatalf("assumed role ARN = %q", got)
	}

	account, ok := svc.AccountFor(session.AccessKeyID)
	if !ok || account != "222222222222" {
		t.Fatalf("AccountFor = %q, %v", account, ok)
	}

	id, err := svc.GetCallerIdentity(Caller{AccessKeyID: session.AccessKeyID, Account: "222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	if id.Account != "222222222222" || id.ARN != session.AssumedRoleARN() || !strings.HasPrefix(id.UserID, "AROA") || !strings.HasSuffix(id.UserID, ":ci-run") {
		t.Fatalf("caller identity = %+v", id)
	}
}

func TestRoleIDIsStablePerRole(t *testing.T) {
	svc := newTestService(t)
	a, _ := svc.AssumeRole(AssumeRoleInput{RoleARN: "arn:aws:iam::111111111111:role/r", RoleSessionName: "one"})
	b, _ := svc.AssumeRole(AssumeRoleInput{RoleARN: "arn:aws:iam::111111111111:role/r", RoleSessionName: "two"})
	c, _ := svc.AssumeRole(AssumeRoleInput{RoleARN: "arn:aws:iam::111111111111:role/other", RoleSessionName: "one"})
	if a.RoleID != b.RoleID || a.RoleID == c.RoleID {
		t.Fatalf("role IDs: %s %s %s", a.RoleID, b.RoleID, c.RoleID)
	}
	if a.AccessKeyID == b.AccessKeyID {
		t.Fatal("each session needs its own access key")
	}
}

func TestAssumeRoleValidatesInput(t *testing.T) {
	svc := newTestService(t)
	cases := map[string]AssumeRoleInput{
		"bad ARN":          {RoleARN: "deployer", RoleSessionName: "s1"},
		"short session":    {RoleARN: "arn:aws:iam::111111111111:role/r", RoleSessionName: "s"},
		"bad session char": {RoleARN: "arn:aws:iam::111111111111:role/r", RoleSessionName: "has space"},
		"too short":        {RoleARN: "arn:aws:iam::111111111111:role/r", RoleSessionName: "s1", DurationSeconds: 60},
		"too long":         {RoleARN: "arn:aws:iam::111111111111:role/r", RoleSessionName: "s1", DurationSeconds: 43201},
	}
	for name, input := range cases {
		_, err := svc.AssumeRole(input)
		var apiErr *apierror.Error
		if !errors.As(err, &apiErr) || apiErr.Code != "ValidationError" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCallerIdentityWithoutSession(t *testing.T) {
	svc := newTestService(t)
	id, err := svc.GetCallerIdentity(Caller{AccessKeyID: "test", Account: "333333333333"})
	if err != nil {
		t.Fatal(err)
	}
	if id.Account != "333333333333" || id.ARN != "arn:aws:iam::333333333333:user/stratus" {
		t.Fatalf("caller identity = %+v", id)
	}
	if _, ok := svc.AccountFor("test"); ok {
		t.Fatal("an unknown key must not resolve")
	}
}

func TestGetAccessKeyInfo(t *testing.T) {
	svc := newTestService(t)
	session, _ := svc.AssumeRole(AssumeRoleInput{RoleARN: "arn:aws:iam::444444444444:role/r", RoleSessionName: "s1"})
	for key, want := range map[string]string{
		"111111111111":      "111111111111",
		session.AccessKeyID: "444444444444",
		"AKIAEXAMPLE":       "000000000000",
	} {
		got, err := svc.GetAccessKeyInfo(key)
		if err != nil || got != want {
			t.Errorf("GetAccessKeyInfo(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
}

func TestHandleAssumeRoleWritesCredentials(t *testing.T) {
	svc := newTestService(t)
	body := "Action=AssumeRole&Version=2011-06-15&RoleArn=arn%3Aaws%3Aiam%3A%3A111111111111%3Arole%2Fdeployer&RoleSessionName=s1&DurationSeconds=900"
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	if err := svc.Handle(w, r, "AssumeRole", "req-1", Caller{}); err != nil {
		t.Fatal(err)
	}
	out := w.Body.String()
	for _, want := range []string{"<AccessKeyId>ASIA", "<Expiration>2026-11-10T12:15:00Z</Expiration>", "assumed-role/deployer/s1", "<RequestId>req-1</RequestId>"} {
		if !strings.Contains(out, want) {
			t.Errorf("response missing %q:\n%s", want, out)
		}
	}
}
