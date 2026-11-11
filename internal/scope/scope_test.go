package scope

import (
	"context"
	"testing"
)

func TestARNs(t *testing.T) {
	s := Scope{Account: "111122223333", Region: "eu-central-1"}
	if got := s.ARN("sqs", "orders"); got != "arn:aws:sqs:eu-central-1:111122223333:orders" {
		t.Fatalf("ARN = %q", got)
	}
	if got := s.GlobalARN("iam", "role/app"); got != "arn:aws:iam::111122223333:role/app" {
		t.Fatalf("GlobalARN = %q", got)
	}
}

func TestDefaultAndAccountOnly(t *testing.T) {
	if !Default().IsDefault() {
		t.Fatal("Default() is not default")
	}
	s := Scope{Account: DefaultAccount, Region: "eu-west-1"}
	if s.IsDefault() || !s.AccountOnly().IsDefault() {
		t.Fatalf("AccountOnly of %v should be default", s)
	}
}

func TestValidation(t *testing.T) {
	for _, ok := range []string{"000000000000", "123456789012"} {
		if !ValidAccount(ok) {
			t.Fatalf("ValidAccount(%q) = false", ok)
		}
	}
	for _, bad := range []string{"test", "12345", "AKIAIOSFODNN7EXAMPLE"} {
		if ValidAccount(bad) {
			t.Fatalf("ValidAccount(%q) = true", bad)
		}
	}
	for _, ok := range []string{"us-east-1", "eu-central-1", "us-gov-west-1", "ap-southeast-2"} {
		if !ValidRegion(ok) {
			t.Fatalf("ValidRegion(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "local", "US-EAST-1", "us-east"} {
		if ValidRegion(bad) {
			t.Fatalf("ValidRegion(%q) = true", bad)
		}
	}
}

func TestContext(t *testing.T) {
	if FromContext(context.Background()) != Default() {
		t.Fatal("empty context should give the default scope")
	}
	s := Scope{Account: "111122223333", Region: "eu-west-1"}
	if FromContext(WithScope(context.Background(), s)) != s {
		t.Fatal("scope did not round-trip through context")
	}
}

func TestPrefixRoundTrip(t *testing.T) {
	if Prefix(Default()) != "" {
		t.Fatal("default scope must not be prefixed")
	}
	s := Scope{Account: "111122223333", Region: "eu-west-1"}
	name := Prefix(s) + "sqs-queues"
	if name != "@111122223333/eu-west-1/sqs-queues" {
		t.Fatalf("prefixed name = %q", name)
	}
	got, base := SplitPrefixed(name)
	if got != s || base != "sqs-queues" {
		t.Fatalf("SplitPrefixed = %v %q", got, base)
	}
	if got, base := SplitPrefixed("sqs-queues"); got != Default() || base != "sqs-queues" {
		t.Fatalf("SplitPrefixed(unprefixed) = %v %q", got, base)
	}
}
