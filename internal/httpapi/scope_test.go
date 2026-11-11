package httpapi

import (
	"net/http/httptest"
	"testing"

	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/scope"
)

type fixedAccounts map[string]string

func (f fixedAccounts) AccountFor(key string) (string, bool) {
	account, ok := f[key]
	return account, ok
}

func signed(key, region string) *awscompat.SigV4Identity {
	return awscompat.ParseSigV4Authorization("AWS4-HMAC-SHA256 Credential=" + key + "/20261110/" + region + "/sqs/aws4_request, SignedHeaders=host, Signature=x")
}

func TestResolveScope(t *testing.T) {
	req := httptest.NewRequest("POST", "http://localhost:4566/", nil)
	cases := []struct {
		name     string
		sig      *awscompat.SigV4Identity
		accounts AccountResolver
		want     scope.Scope
	}{
		{"unsigned", nil, nil, scope.Default()},
		{"plain test key", signed("test", "us-east-1"), nil, scope.Default()},
		{"region from signature", signed("test", "eu-central-1"), nil, scope.Scope{Account: scope.DefaultAccount, Region: "eu-central-1"}},
		{"account from numeric key", signed("111122223333", "eu-west-1"), nil, scope.Scope{Account: "111122223333", Region: "eu-west-1"}},
		{"issued key", signed("ASIASTRATUS1", "us-east-1"), fixedAccounts{"ASIASTRATUS1": "444455556666"}, scope.Scope{Account: "444455556666", Region: "us-east-1"}},
		{"unknown key", signed("ASIAOTHER", "us-east-1"), fixedAccounts{}, scope.Default()},
		{"bogus region", signed("111122223333", "local"), nil, scope.Scope{Account: "111122223333", Region: scope.DefaultRegion}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveScope(req, tc.sig, tc.accounts); got != tc.want {
				t.Fatalf("ResolveScope() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveScopeFromPresignedURL(t *testing.T) {
	req := httptest.NewRequest("GET", "http://localhost:4566/bucket/key?X-Amz-Credential=111122223333%2F20261110%2Fap-southeast-2%2Fs3%2Faws4_request", nil)
	want := scope.Scope{Account: "111122223333", Region: "ap-southeast-2"}
	if got := ResolveScope(req, nil, nil); got != want {
		t.Fatalf("ResolveScope() = %v, want %v", got, want)
	}
}
