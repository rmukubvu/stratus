package iam

import (
	"strconv"
	"strings"
	"testing"
)

const trust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

func TestRoleTagsRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.must("CreateRole", "RoleName", "app", "AssumeRolePolicyDocument", trust,
		"Tags.member.1.Key", "env", "Tags.member.1.Value", "dev",
		"Tags.member.2.Key", "team", "Tags.member.2.Value", "core")

	got := f.must("GetRole", "RoleName", "app")
	if !strings.Contains(got, "<Tags><member><Key>env</Key><Value>dev</Value></member><member><Key>team</Key><Value>core</Value></member></Tags>") {
		t.Fatalf("GetRole tags: %s", got)
	}

	f.must("TagRole", "RoleName", "app", "Tags.member.1.Key", "env", "Tags.member.1.Value", "prod",
		"Tags.member.2.Key", "owner", "Tags.member.2.Value", "ops")
	f.must("UntagRole", "RoleName", "app", "TagKeys.member.1", "team", "TagKeys.member.2", "absent")

	got = f.must("ListRoleTags", "RoleName", "app")
	want := "<Tags><member><Key>env</Key><Value>prod</Value></member><member><Key>owner</Key><Value>ops</Value></member></Tags>"
	if !strings.Contains(got, want) || !strings.Contains(got, "<IsTruncated>false</IsTruncated>") {
		t.Fatalf("ListRoleTags: %s", got)
	}
}

func TestRoleWithoutTagsOmitsTheElement(t *testing.T) {
	f := newFixture(t)
	f.must("CreateRole", "RoleName", "bare", "AssumeRolePolicyDocument", trust)
	if got := f.must("GetRole", "RoleName", "bare"); strings.Contains(got, "<Tags>") {
		t.Fatalf("GetRole: %s", got)
	}
}

func TestRoleTagValidation(t *testing.T) {
	f := newFixture(t)
	f.mustFail("InvalidInput", "CreateRole", "RoleName", "dup", "AssumeRolePolicyDocument", trust,
		"Tags.member.1.Key", "k", "Tags.member.2.Key", "k")
	f.mustFail("InvalidInput", "CreateRole", "RoleName", "reserved", "AssumeRolePolicyDocument", trust,
		"Tags.member.1.Key", "aws:x")
	f.mustFail("NoSuchEntity", "TagRole", "RoleName", "missing", "Tags.member.1.Key", "k")

	f.must("CreateRole", "RoleName", "full", "AssumeRolePolicyDocument", trust)
	params := []string{"RoleName", "full"}
	for i := 1; i <= 51; i++ {
		params = append(params, "Tags.member."+strconv.Itoa(i)+".Key", "k"+strconv.Itoa(i))
	}
	f.mustFail("LimitExceeded", "TagRole", params...)
}
