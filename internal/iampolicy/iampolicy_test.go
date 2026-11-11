package iampolicy

import "testing"

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"*", "anything", true},
		{"s3:Get*", "s3:GetObject", true},
		{"s3:Get*", "s3:PutObject", false},
		{"arn:aws:s3:::bucket/*", "arn:aws:s3:::bucket/a/b.txt", true},
		{"arn:aws:s3:::bucket/*", "arn:aws:s3:::bucket", false},
		{"queue-?", "queue-1", true},
		{"queue-?", "queue-10", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"", "", true},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.pattern, tc.value); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q) = %v", tc.pattern, tc.value, got)
		}
	}
}

func TestParseAcceptsShorthand(t *testing.T) {
	doc := MustParse(`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"sqs:SendMessage","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":true}}}}`)
	if len(doc.Statement) != 1 {
		t.Fatalf("statements = %d", len(doc.Statement))
	}
	st := doc.Statement[0]
	if st.Action[0] != "sqs:SendMessage" || st.Resource[0] != "*" || st.Condition["Bool"]["aws:SecureTransport"][0] != "true" {
		t.Fatalf("statement = %+v", st)
	}
}

func TestParseRejectsBadDocuments(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"Statement":[{"Effect":"Maybe","Action":"*","Resource":"*"}]}`,
		`{"Statement":[{"Effect":"Allow","Resource":"*"}]}`,
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%s) should fail", raw)
		}
	}
}

func TestExplicitDenyWinsOverAllow(t *testing.T) {
	docs := []Document{
		MustParse(`{"Statement":[{"Sid":"all","Effect":"Allow","Action":"s3:*","Resource":"*"}]}`),
		MustParse(`{"Statement":[{"Sid":"noDelete","Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::prod/*"}]}`),
	}
	cases := []struct {
		action, resource string
		want             Outcome
		sid              string
	}{
		{"s3:GetObject", "arn:aws:s3:::prod/a", Allowed, "all"},
		{"s3:DeleteObject", "arn:aws:s3:::prod/a", ExplicitDeny, "noDelete"},
		{"s3:DeleteObject", "arn:aws:s3:::dev/a", Allowed, "all"},
		{"sqs:SendMessage", "arn:aws:sqs:us-east-1:111111111111:q", ImplicitDeny, ""},
	}
	for _, tc := range cases {
		d := Evaluate(docs, Request{Action: tc.action, Resource: tc.resource}, false)
		if d.Outcome != tc.want || d.Statement != tc.sid {
			t.Errorf("%s on %s = %v/%q, want %v/%q", tc.action, tc.resource, d.Outcome, d.Statement, tc.want, tc.sid)
		}
	}
}

func TestActionsAreCaseInsensitiveResourcesAreNot(t *testing.T) {
	docs := []Document{MustParse(`{"Statement":[{"Effect":"Allow","Action":"SQS:sendmessage","Resource":"arn:aws:sqs:*:*:Orders"}]}`)}
	if d := Evaluate(docs, Request{Action: "sqs:SendMessage", Resource: "arn:aws:sqs:us-east-1:1:Orders"}, false); d.Outcome != Allowed {
		t.Fatalf("action case: %v", d.Outcome)
	}
	if d := Evaluate(docs, Request{Action: "sqs:SendMessage", Resource: "arn:aws:sqs:us-east-1:1:orders"}, false); d.Outcome != ImplicitDeny {
		t.Fatalf("resource case: %v", d.Outcome)
	}
}

func TestNotActionAndNotResource(t *testing.T) {
	docs := []Document{MustParse(`{"Statement":[
		{"Effect":"Allow","NotAction":"iam:*","Resource":"*"},
		{"Effect":"Deny","Action":"s3:*","NotResource":["arn:aws:s3:::public","arn:aws:s3:::public/*"]}]}`)}
	check := func(action, resource string, want Outcome) {
		t.Helper()
		if d := Evaluate(docs, Request{Action: action, Resource: resource}, false); d.Outcome != want {
			t.Errorf("%s on %s = %v, want %v", action, resource, d.Outcome, want)
		}
	}
	check("iam:CreateUser", "*", ImplicitDeny)
	check("sqs:SendMessage", "arn:aws:sqs:us-east-1:1:q", Allowed)
	check("s3:GetObject", "arn:aws:s3:::public/x", Allowed)
	check("s3:GetObject", "arn:aws:s3:::private/x", ExplicitDeny)
}

func TestUnknownResourceMatchesOnActionAlone(t *testing.T) {
	docs := []Document{MustParse(`{"Statement":[{"Effect":"Allow","Action":"kms:*","Resource":"arn:aws:kms:us-east-1:1:key/abc"}]}`)}
	if d := Evaluate(docs, Request{Action: "kms:ListKeys", Resource: AnyResource}, false); d.Outcome != Allowed {
		t.Fatalf("got %v", d.Outcome)
	}
}

func TestPolicyVariables(t *testing.T) {
	docs := []Document{MustParse(`{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::home/${aws:username}/*"}]}`)}
	ctx := map[string][]string{"aws:username": {"alice"}}
	if d := Evaluate(docs, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::home/alice/x", Context: ctx}, false); d.Outcome != Allowed {
		t.Fatalf("own prefix: %v", d.Outcome)
	}
	if d := Evaluate(docs, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::home/bob/x", Context: ctx}, false); d.Outcome != ImplicitDeny {
		t.Fatalf("other prefix: %v", d.Outcome)
	}
	if d := Evaluate(docs, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::home//x"}, false); d.Outcome != ImplicitDeny {
		t.Fatalf("missing variable must not match an empty segment: %v", d.Outcome)
	}
}

func TestConditions(t *testing.T) {
	ctx := map[string][]string{
		"aws:SourceIp":        {"10.0.1.7"},
		"aws:RequestedRegion": {"eu-west-1"},
		"aws:SecureTransport": {"false"},
		"aws:PrincipalArn":    {"arn:aws:iam::111111111111:role/ci-deployer"},
		"aws:CurrentTime":     {"2026-11-11T00:00:00Z"},
		"aws:TagKeys":         {"team", "env"},
		"s3:max-keys":         {"50"},
	}
	cases := []struct {
		condition string
		want      bool
	}{
		{`{"StringEquals":{"aws:RequestedRegion":["eu-west-1","eu-central-1"]}}`, true},
		{`{"StringEquals":{"aws:RequestedRegion":"us-east-1"}}`, false},
		{`{"StringNotEquals":{"aws:RequestedRegion":"us-east-1"}}`, true},
		{`{"StringEqualsIgnoreCase":{"aws:RequestedRegion":"EU-WEST-1"}}`, true},
		{`{"StringLike":{"aws:PrincipalArn":"arn:aws:iam::*:role/ci-*"}}`, true},
		{`{"StringNotLike":{"aws:PrincipalArn":"arn:aws:iam::*:role/ci-*"}}`, false},
		{`{"ArnLike":{"aws:PrincipalArn":"arn:aws:iam::111111111111:role/*"}}`, true},
		{`{"ArnNotLike":{"aws:PrincipalArn":"arn:aws:iam::222222222222:*"}}`, true},
		{`{"IpAddress":{"aws:SourceIp":"10.0.0.0/16"}}`, true},
		{`{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/16"}}`, false},
		{`{"IpAddress":{"aws:SourceIp":"10.0.1.7"}}`, true},
		{`{"Bool":{"aws:SecureTransport":"false"}}`, true},
		{`{"NumericLessThanEquals":{"s3:max-keys":"100"}}`, true},
		{`{"NumericGreaterThan":{"s3:max-keys":"100"}}`, false},
		{`{"DateLessThan":{"aws:CurrentTime":"2027-01-01T00:00:00Z"}}`, true},
		{`{"DateGreaterThan":{"aws:CurrentTime":"2027-01-01T00:00:00Z"}}`, false},
		{`{"Null":{"aws:MultiFactorAuthPresent":"true"}}`, true},
		{`{"Null":{"aws:SourceIp":"true"}}`, false},
		// Missing keys.
		{`{"StringEquals":{"aws:MissingKey":"x"}}`, false},
		{`{"StringNotEquals":{"aws:MissingKey":"x"}}`, true},
		{`{"StringEqualsIfExists":{"aws:MissingKey":"x"}}`, true},
		{`{"StringEqualsIfExists":{"aws:RequestedRegion":"us-east-1"}}`, false},
		// Multi-valued keys.
		{`{"ForAnyValue:StringEquals":{"aws:TagKeys":["env"]}}`, true},
		{`{"ForAllValues:StringEquals":{"aws:TagKeys":["env","team","cost"]}}`, true},
		{`{"ForAllValues:StringEquals":{"aws:TagKeys":["env"]}}`, false},
		{`{"ForAllValues:StringEquals":{"aws:Missing":["env"]}}`, true},
		{`{"ForAnyValue:StringEquals":{"aws:Missing":["env"]}}`, false},
		// Keys are case-insensitive; conditions are ANDed.
		{`{"StringEquals":{"AWS:requestedregion":"eu-west-1"},"Bool":{"aws:SecureTransport":"true"}}`, false},
		// Unknown operators never hold.
		{`{"StringSoundsLike":{"aws:RequestedRegion":"eu-west-1"}}`, false},
	}
	for _, tc := range cases {
		doc := MustParse(`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*","Condition":` + tc.condition + `}]}`)
		got := Evaluate([]Document{doc}, Request{Action: "s3:ListBucket", Resource: "arn:aws:s3:::b", Context: ctx}, false).Outcome == Allowed
		if got != tc.want {
			t.Errorf("%s = %v, want %v", tc.condition, got, tc.want)
		}
	}
}

func TestResourcePolicyPrincipals(t *testing.T) {
	doc := MustParse(`{"Statement":[
		{"Sid":"role","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::111111111111:role/reader"},"Action":"s3:GetObject"},
		{"Sid":"account","Effect":"Allow","Principal":{"AWS":"222222222222"},"Action":"s3:ListBucket"},
		{"Sid":"service","Effect":"Allow","Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:SendMessage"},
		{"Sid":"public","Effect":"Allow","Principal":"*","Action":"s3:GetBucketLocation"}]}`)
	reader := PrincipalInfo{ARN: "arn:aws:sts::111111111111:assumed-role/reader/s1", RoleARN: "arn:aws:iam::111111111111:role/reader", Account: "111111111111"}
	other := PrincipalInfo{ARN: "arn:aws:iam::222222222222:user/bob", Account: "222222222222"}
	cases := []struct {
		p      PrincipalInfo
		action string
		want   Outcome
	}{
		{reader, "s3:GetObject", Allowed},
		{other, "s3:GetObject", ImplicitDeny},
		{other, "s3:ListBucket", Allowed},
		{reader, "s3:ListBucket", ImplicitDeny},
		{PrincipalInfo{Service: "sns.amazonaws.com"}, "sqs:SendMessage", Allowed},
		{other, "s3:GetBucketLocation", Allowed},
	}
	for _, tc := range cases {
		if d := Evaluate([]Document{doc}, Request{Action: tc.action, Resource: "arn:aws:s3:::b", Principal: tc.p}, true); d.Outcome != tc.want {
			t.Errorf("%s by %+v = %v, want %v", tc.action, tc.p, d.Outcome, tc.want)
		}
	}
}

func TestNotPrincipal(t *testing.T) {
	doc := MustParse(`{"Statement":[{"Effect":"Deny","NotPrincipal":{"AWS":"arn:aws:iam::111111111111:role/admin"},"Action":"s3:DeleteBucket"}]}`)
	admin := PrincipalInfo{ARN: "arn:aws:sts::111111111111:assumed-role/admin/x", RoleARN: "arn:aws:iam::111111111111:role/admin", Account: "111111111111"}
	dev := PrincipalInfo{ARN: "arn:aws:iam::111111111111:user/dev", Account: "111111111111"}
	if d := Evaluate([]Document{doc}, Request{Action: "s3:DeleteBucket", Principal: admin}, true); d.Outcome != ImplicitDeny {
		t.Fatalf("admin: %v", d.Outcome)
	}
	if d := Evaluate([]Document{doc}, Request{Action: "s3:DeleteBucket", Principal: dev}, true); d.Outcome != ExplicitDeny {
		t.Fatalf("dev: %v", d.Outcome)
	}
}
