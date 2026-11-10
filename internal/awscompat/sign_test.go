package awscompat

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The expected signatures were produced by botocore's SigV4Auth for the same
// request, keys and time, so these tests pin byte-for-byte compatibility
// with what AWS verifies.
var (
	exampleCreds = Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	exampleTime  = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	converseBody = []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
)

func converseRequest(t *testing.T) *http.Request {
	t.Helper()
	// The model id's ':' is encoded once on the wire and again in the
	// canonical request.
	req, err := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-haiku-20240307-v1%3A0/converse",
		strings.NewReader(string(converseBody)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSignV4MatchesBotocoreForBedrock(t *testing.T) {
	req := converseRequest(t)
	SignV4(req, converseBody, exampleCreds, "us-east-1", "bedrock", exampleTime)

	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261109/us-east-1/bedrock/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, " +
		"Signature=a9269893e5f31c11d1fcbd4c72de078e49712dc1f1a32950166167ade790c9e7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20261109T120000Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
}

func TestSignV4SignsTheSessionToken(t *testing.T) {
	req := converseRequest(t)
	creds := exampleCreds
	creds.SessionToken = "session-token-xyz"
	SignV4(req, converseBody, creds, "us-east-1", "bedrock", exampleTime)

	if got := req.Header.Get("X-Amz-Security-Token"); got != "session-token-xyz" {
		t.Fatalf("X-Amz-Security-Token = %q", got)
	}
	want := "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token, " +
		"Signature=a0e53478e0df8e0dc5d92661cced55bd55154dffe1f677ac01c1ad16305d44fe"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, want) {
		t.Fatalf("Authorization %s should end with %s", got, want)
	}
}

func TestSignV4SortsTheQueryString(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Version=2010-05-08&Action=ListUsers", nil)
	if err != nil {
		t.Fatal(err)
	}
	SignV4(req, nil, exampleCreds, "us-east-1", "iam", exampleTime)

	want := "Signature=a5e7b2a5591a0e265296e85b3cc9d21fc908af454f223455ff4fd29ff749df52"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, want) {
		t.Fatalf("Authorization %s should end with %s", got, want)
	}
}

func TestURIEncodeKeepsOnlyUnreservedCharacters(t *testing.T) {
	cases := map[string]string{
		"anthropic.claude-3-haiku-20240307-v1:0": "anthropic.claude-3-haiku-20240307-v1%3A0",
		"a b/c~d_e":                              "a%20b%2Fc~d_e",
		"%3A":                                    "%253A",
	}
	for in, want := range cases {
		if got := URIEncode(in); got != want {
			t.Errorf("URIEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredentialsValid(t *testing.T) {
	if (Credentials{AccessKeyID: "a"}).Valid() {
		t.Fatal("a missing secret is not valid")
	}
	if !exampleCreds.Valid() {
		t.Fatal("both keys present is valid")
	}
}
