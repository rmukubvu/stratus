package validation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stratus/internal/awsmodel"
)

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	out := map[string]any{}
	if err := d.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func check(t *testing.T, service, operation, body string) []Violation {
	t.Helper()
	svc, err := awsmodel.Load(service)
	if err != nil {
		t.Fatal(err)
	}
	api, op, ok := svc.Operation(operation)
	if !ok {
		t.Fatalf("%s.%s not in catalogue", service, operation)
	}
	var c Checker
	return c.Check(api, op.Input, decode(t, body))
}

func TestAMissingRequiredMemberIsNull(t *testing.T) {
	got := check(t, "dynamodb", "DescribeTable", `{}`)
	want := "1 validation error detected: Value null at 'tableName' failed to satisfy constraint: Member must not be null"
	if Message(got) != want {
		t.Fatalf("got  %q\nwant %q", Message(got), want)
	}
}

func TestAnEmptyStringBreaksTheMinimumLength(t *testing.T) {
	got := check(t, "dynamodb", "DescribeTable", `{"TableName": ""}`)
	want := "1 validation error detected: Value '' at 'tableName' failed to satisfy constraint: Member must have length greater than or equal to 1"
	if Message(got) != want {
		t.Fatalf("got  %q\nwant %q", Message(got), want)
	}
}

func TestListElementsAreNamedByOneBasedMemberIndex(t *testing.T) {
	got := check(t, "dynamodb", "CreateTable", `{
		"TableName": "orders",
		"KeySchema": [{"AttributeName": "pk", "KeyType": "HASH"}, {"AttributeName": "sk", "KeyType": "SORT"}],
		"AttributeDefinitions": [{"AttributeName": "pk", "AttributeType": "S"}]
	}`)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	want := "Value 'SORT' at 'keySchema.2.member.keyType' failed to satisfy constraint: Member must satisfy enum value set: [HASH, RANGE]"
	if got[0].String() != want {
		t.Fatalf("got  %q\nwant %q", got[0], want)
	}
}

func TestNumbersOutsideTheirRangeAreRejected(t *testing.T) {
	got := check(t, "kms", "GenerateRandom", `{"NumberOfBytes": 2048}`)
	want := "1 validation error detected: Value '2048' at 'numberOfBytes' failed to satisfy constraint: Member must have value less than or equal to 1024"
	if Message(got) != want {
		t.Fatalf("got  %q\nwant %q", Message(got), want)
	}
}

func TestAPatternMatchesAnywhereInTheValueUnlessAnchored(t *testing.T) {
	if got := check(t, "logs", "CreateLogGroup", `{"logGroupName": "/aws/lambda/fn"}`); len(got) != 0 {
		t.Fatalf("valid name rejected: %v", got)
	}
	got := check(t, "logs", "CreateLogGroup", `{"logGroupName": "!!!"}`)
	if len(got) != 1 || !strings.Contains(got[0].Constraint, "Member must satisfy regular expression pattern: ") {
		t.Fatalf("got %v, want a pattern violation", got)
	}
}

func TestEveryViolationIsReportedTogether(t *testing.T) {
	got := check(t, "dynamodb", "Scan", `{"TableName": "", "Limit": 0}`)
	msg := Message(got)
	if !strings.HasPrefix(msg, "2 validation errors detected: ") || !strings.Contains(msg, "; ") {
		t.Fatalf("got %q", msg)
	}
}

func TestBlobLengthsCountDecodedBytes(t *testing.T) {
	// KMS Encrypt's Plaintext is 1 to 4096 bytes; "" decodes to zero bytes.
	got := check(t, "kms", "Encrypt", `{"KeyId": "alias/app", "Plaintext": ""}`)
	if len(got) != 1 || got[0].Path != "plaintext" {
		t.Fatalf("got %v", got)
	}
}

func TestAPatternRE2CannotCompileIsSkipped(t *testing.T) {
	var c Checker
	if c.pattern(`^(?!aws:).*$`) != nil {
		t.Fatal("lookahead compiled")
	}
	api := &awsmodel.API{Shapes: map[string]*awsmodel.Shape{
		"In":  {Type: "structure", Members: map[string]awsmodel.Member{"Key": {Shape: "Key"}}},
		"Key": {Type: "string", Pattern: `^(?!aws:).*$`},
	}}
	if got := c.Check(api, "In", map[string]any{"Key": "aws:x"}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestMembersOutsideTheBodyAreNotRequiredInIt(t *testing.T) {
	api := &awsmodel.API{Shapes: map[string]*awsmodel.Shape{
		"In": {Type: "structure", Required: []string{"Id"}, Members: map[string]awsmodel.Member{
			"Id": {Shape: "S", Location: "uri", LocationName: "id"},
		}},
		"S": {Type: "string"},
	}}
	var c Checker
	if got := c.Check(api, "In", map[string]any{}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestMessageQuotesOnlyRealValues(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(Violation{Path: "a", Value: "null", Constraint: "c"}.String())
	if buf.String() != "Value null at 'a' failed to satisfy constraint: c" {
		t.Fatal(buf.String())
	}
}
