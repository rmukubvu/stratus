package sns

import (
	"strings"
	"testing"
)

func TestATopicWithoutAPolicyReadsBackAWSsDefaultPolicy(t *testing.T) {
	call := newTagFixture(t)
	if _, err := call("CreateTopic", "Name", "orders"); err != nil {
		t.Fatal(err)
	}
	out, err := call("GetTopicAttributes", "TopicArn", "arn:aws:sns:us-east-1:000000000000:orders")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<key>Policy</key>", "__default_policy_ID", "SNS:Publish", "AWS:SourceOwner"} {
		if !strings.Contains(out, want) {
			t.Errorf("attributes lack %q:\n%s", want, out)
		}
	}
	if _, err := call("SetTopicAttributes", "TopicArn", "arn:aws:sns:us-east-1:000000000000:orders", "AttributeName", "Policy", "AttributeValue", `{"Version":"2012-10-17","Statement":[]}`); err != nil {
		t.Fatal(err)
	}
	out, _ = call("GetTopicAttributes", "TopicArn", "arn:aws:sns:us-east-1:000000000000:orders")
	if strings.Contains(out, "__default_policy_ID") || strings.Count(out, "<key>Policy</key>") != 1 {
		t.Errorf("a set policy should replace the default:\n%s", out)
	}
}

func TestSubscribeKeepsItsAttributesAndReportsTheSubscriptionConfirmed(t *testing.T) {
	call := newTagFixture(t)
	if _, err := call("CreateTopic", "Name", "orders"); err != nil {
		t.Fatal(err)
	}
	out, err := call("Subscribe", "TopicArn", "arn:aws:sns:us-east-1:000000000000:orders", "Protocol", "sqs",
		"Endpoint", "arn:aws:sqs:us-east-1:000000000000:audit",
		"Attributes.entry.1.key", "RawMessageDelivery", "Attributes.entry.1.value", "true",
		"Attributes.entry.2.key", "FilterPolicy", "Attributes.entry.2.value", `{"type":["paid"]}`)
	if err != nil {
		t.Fatal(err)
	}
	arn := out[strings.Index(out, "<SubscriptionArn>")+len("<SubscriptionArn>") : strings.Index(out, "</SubscriptionArn>")]
	out, err = call("GetSubscriptionAttributes", "SubscriptionArn", arn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>RawMessageDelivery</key><value>true</value>",
		"<key>PendingConfirmation</key><value>false</value>",
		"<key>FilterPolicy</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("attributes lack %q:\n%s", want, out)
		}
	}
	_, err = call("Subscribe", "TopicArn", "arn:aws:sns:us-east-1:000000000000:orders", "Protocol", "sqs",
		"Endpoint", "arn:aws:sqs:us-east-1:000000000000:audit", "Attributes.entry.1.key", "Colour", "Attributes.entry.1.value", "red")
	wantCode(t, err, "InvalidParameter")
}
