package messaging

import (
	"strings"
	"testing"

	"github.com/stratus/internal/advise"
)

func terraformOf(t *testing.T, option string, answers map[string]any) advise.TerraformCode {
	t.Helper()
	f := New(documented, listPrices)
	d := assess(t, answers)
	return f.Terraform(option, d.Requirements(f))
}

func defaultOf(t *testing.T, code advise.TerraformCode, name string) any {
	t.Helper()
	for _, v := range code.Variables {
		if v.Name == name {
			return v.Default
		}
	}
	t.Fatalf("no variable %s in %+v", name, code.Variables)
	return nil
}

func TestEveryOptionHasTerraformAndOnlyFIFOAndMSKCannotBeTestDeployed(t *testing.T) {
	for _, o := range New(nil, nil).Options() {
		code := terraformOf(t, o.ID, nil)
		if code.Unsupported != "" || code.Resources == "" || len(code.Endpoints) == 0 || code.Region != "us-east-1" {
			t.Errorf("%s: %+v", o.ID, code)
		}
		if wantDeployable := o.ID != SQSFIFO && o.ID != MSK; (code.NotDeployable == "") != wantDeployable {
			t.Errorf("%s: NotDeployable = %q", o.ID, code.NotDeployable)
		}
	}
	if code := New(nil, nil).Terraform("nope", advise.NewRequirements(nil, nil)); code.Unsupported == "" {
		t.Error("an unknown option got Terraform")
	}
}

func TestQueuesGetADeadLetterQueueEncryptionAndTheStatedRetention(t *testing.T) {
	code := terraformOf(t, SQSStandard, map[string]any{"retention_days": 2.0})
	for _, want := range []string{`resource "aws_sqs_queue" "dlq"`, "sqs_managed_sse_enabled   = true", "deadLetterTargetArn = aws_sqs_queue.dlq.arn"} {
		if !strings.Contains(code.Resources, want) {
			t.Errorf("SQS Terraform lacks %q:\n%s", want, code.Resources)
		}
	}
	if got := defaultOf(t, code, "message_retention_seconds"); got != 172800 {
		t.Errorf("retention = %v, want 2 days", got)
	}
	if got := defaultOf(t, terraformOf(t, SQSStandard, map[string]any{"retention_days": 0.0}), "message_retention_seconds"); got != 60 {
		t.Errorf("retention floor = %v, want SQS's minimum of 60 s", got)
	}
}

func TestPerKeyOrderingUsesFIFOHighThroughputMode(t *testing.T) {
	perKey := terraformOf(t, SQSFIFO, map[string]any{"ordering": "per_key", "peak_rate": 50.0})
	if !strings.Contains(perKey.Resources, `fifo_throughput_limit       = "perMessageGroupId"`) || !strings.Contains(perKey.Resources, `"${var.name}.fifo"`) {
		t.Errorf("per-key FIFO:\n%s", perKey.Resources)
	}
	global := terraformOf(t, SQSFIFO, map[string]any{"ordering": "global", "peak_rate": 50.0})
	if strings.Contains(global.Resources, "perMessageGroupId") {
		t.Errorf("global order needs one queue-wide order:\n%s", global.Resources)
	}
}

func TestFanOutOptionsGiveEachConsumerAQueueThatAdmitsOnlyItsSender(t *testing.T) {
	sns := terraformOf(t, SNSFanout, map[string]any{"consumers": 3.0, "routing": true})
	for _, want := range []string{`identifiers = ["sns.amazonaws.com"]`, "values   = [aws_sns_topic.main.arn]", "raw_message_delivery = true", "var.filter_policies[count.index]"} {
		if !strings.Contains(sns.Resources, want) {
			t.Errorf("SNS Terraform lacks %q", want)
		}
	}
	if got := defaultOf(t, sns, "consumers"); got != 3 {
		t.Errorf("consumers = %v", got)
	}
	if strings.Contains(terraformOf(t, SNSFanout, nil).Resources, "filter_policy") {
		t.Error("filter policies without routing")
	}
	events := terraformOf(t, EventBridge, map[string]any{"consumers": 2.0})
	for _, want := range []string{`resource "aws_cloudwatch_event_bus" "main"`, `identifiers = ["events.amazonaws.com"]`, "aws_cloudwatch_event_rule.consumer[count.index].arn"} {
		if !strings.Contains(events.Resources, want) {
			t.Errorf("EventBridge Terraform lacks %q", want)
		}
	}
}

func TestAStreamIsSizedForThePeakLikeTheCostEstimate(t *testing.T) {
	code := terraformOf(t, Kinesis, map[string]any{"peak_rate": 2500.0, "message_size": 1.0, "retention_days": 7.0})
	if got := defaultOf(t, code, "shard_count"); got != 3 {
		t.Errorf("shards = %v, want 3 for 2,500 records/s", got)
	}
	if got := defaultOf(t, code, "retention_hours"); got != 168 {
		t.Errorf("retention = %v, want 168 hours", got)
	}
	if !strings.Contains(code.Resources, `encryption_type  = "KMS"`) {
		t.Errorf("stream is not encrypted:\n%s", code.Resources)
	}
}

func TestMSKNeedsTheTeamsSubnetsAndSizesStorageFromRetention(t *testing.T) {
	code := terraformOf(t, MSK, map[string]any{"kafka": true, "average_rate": 1000.0, "message_size": 10.0, "retention_days": 7.0})
	if defaultOf(t, code, "subnet_ids") != nil || defaultOf(t, code, "security_group_ids") != nil {
		t.Error("subnets and security groups must be required variables")
	}
	// 1,000 x 10 KiB/s for 7 days x3 replicas is ~17,300 GiB: ~5,770 per broker, doubled.
	if got := defaultOf(t, code, "broker_volume_gb").(int); got < 11000 || got > 12000 {
		t.Errorf("broker volume = %d GiB", got)
	}
}
