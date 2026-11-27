package messaging

import (
	"fmt"
	"math"
	"strings"

	"github.com/stratus/internal/advise"
)

// Terraform for each option follows AWS's defaults for production: a
// dead-letter queue behind every queue, server-side encryption, queue
// policies that admit only the topic or rule that sends, and sizes taken
// from the stated workload. Names come from var.name.

const (
	sqsMinRetentionSeconds = 60
	sqsMaxRetentionSeconds = 14 * secondsPerDay
	defaultVisibility      = 30
	defaultMaxReceives     = 5
	kinesisMinRetention    = 24
	kinesisMaxRetention    = 8760
	fifoBasicPerSecond     = 300 // a FIFO queue without high throughput, per API action
	mskKafkaVersion        = "3.6.0"
	mskMinVolumeGB         = 100
	mskVolumeHeadroom      = 2 // provision twice the retained data per broker
)

// Terraform implements advise.Terraformer.
func (f Family) Terraform(option string, r advise.Requirements) advise.TerraformCode {
	w := workloadFrom(r)
	var code advise.TerraformCode
	switch option {
	case SQSStandard:
		code = sqsTerraform(w, false)
	case SQSFIFO:
		code = sqsTerraform(w, true)
		code.NotDeployable = "Stratus does not implement FIFO queues yet, so this module cannot be test-deployed; apply it to a sandbox AWS account."
	case SNSFanout:
		code = snsTerraform(w)
	case EventBridge:
		code = eventBridgeTerraform(w)
	case Kinesis:
		code = kinesisTerraform(w, quotaReader{src: f.quotas, region: w.region})
	case MSK:
		code = mskTerraform(w)
		code.NotDeployable = "An MSK cluster needs your VPC's subnets and security groups, and on Stratus each cluster starts a Kafka container (docs/msk.md); apply it to a sandbox AWS account."
	default:
		return advise.TerraformCode{Unsupported: "The messaging family writes no Terraform for " + option + "."}
	}
	code.Region = w.region
	return code
}

func sqsRetention(w workload) int {
	seconds := int(math.Round(w.retentionDays * secondsPerDay))
	return min(max(seconds, sqsMinRetentionSeconds), sqsMaxRetentionSeconds)
}

// attrs writes name/value pairs as block arguments, with the equals signs
// aligned the way terraform fmt aligns them.
func attrs(kv ...string) string {
	width := 0
	for i := 0; i < len(kv); i += 2 {
		width = max(width, len(kv[i]))
	}
	var b strings.Builder
	for i := 0; i < len(kv); i += 2 {
		fmt.Fprintf(&b, "  %-*s = %s\n", width, kv[i], kv[i+1])
	}
	return b.String()
}

func queueVariables(w workload) []advise.TerraformVariable {
	return []advise.TerraformVariable{
		{Name: "message_retention_seconds", Type: "number", Default: sqsRetention(w),
			Description: "How long a queue keeps an unconsumed message (60 seconds to 14 days), from the stated retention."},
		{Name: "visibility_timeout_seconds", Type: "number", Default: defaultVisibility,
			Description: "How long a received message stays hidden; set it to at least six times the consumer's processing time."},
		{Name: "max_receive_count", Type: "number", Default: defaultMaxReceives,
			Description: "Receives before a message moves to the dead-letter queue."},
	}
}

func sqsTerraform(w workload, fifo bool) advise.TerraformCode {
	name, dlqName := "var.name", `"${var.name}-dlq"`
	var fifoArgs, dlqFifo []string
	if fifo {
		name, dlqName = `"${var.name}.fifo"`, `"${var.name}-dlq.fifo"`
		fifoArgs = []string{"fifo_queue", "true", "content_based_deduplication", "true"}
		dlqFifo = []string{"fifo_queue", "true"}
		if w.ordering == orderPerKey || w.peak > fifoBasicPerSecond {
			// High throughput mode: quotas apply per message group.
			fifoArgs = append(fifoArgs, "deduplication_scope", `"messageGroup"`, "fifo_throughput_limit", `"perMessageGroupId"`)
		}
	}
	dlq := append(append([]string{"name", dlqName}, dlqFifo...),
		"message_retention_seconds", fmt.Sprint(sqsMaxRetentionSeconds),
		"sqs_managed_sse_enabled", "true")
	main := append(append([]string{"name", name}, fifoArgs...),
		"message_retention_seconds", "var.message_retention_seconds",
		"visibility_timeout_seconds", "var.visibility_timeout_seconds",
		"receive_wait_time_seconds", "20 # long polling: fewer empty receives",
		"sqs_managed_sse_enabled", "true")
	resources := `# Messages that fail max_receive_count times wait here for inspection.
resource "aws_sqs_queue" "dlq" {
` + attrs(dlq...) + `}

resource "aws_sqs_queue" "main" {
` + attrs(main...) + `
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = var.max_receive_count
  })
}
`
	return advise.TerraformCode{
		Resources: resources,
		Variables: queueVariables(w),
		Outputs: []advise.TerraformOutput{
			{Name: "queue_url", Description: "URL producers send to and consumers receive from.", Value: "aws_sqs_queue.main.url"},
			{Name: "queue_arn", Description: "ARN of the queue, for IAM policies and event source mappings.", Value: "aws_sqs_queue.main.arn"},
			{Name: "dead_letter_queue_url", Description: "URL of the dead-letter queue.", Value: "aws_sqs_queue.dlq.url"},
		},
		Endpoints: []string{"sqs"},
	}
}

// consumerQueues is the queues, dead-letter queue and queue policies that
// fan-out options deliver to: one queue per consumer, admitting only the
// service that sends (principal) from the source ARNs (sourceARN).
func consumerQueues(principal, sourceARN string) string {
	return fmt.Sprintf(`# One queue per consumer, each with its own copy of every message.
resource "aws_sqs_queue" "consumer" {
  count = var.consumers

  name                       = "${var.name}-${count.index}"
  message_retention_seconds  = var.message_retention_seconds
  visibility_timeout_seconds = var.visibility_timeout_seconds
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = var.max_receive_count
  })
}

resource "aws_sqs_queue" "dlq" {
  name                      = "${var.name}-dlq"
  message_retention_seconds = %[3]d
  sqs_managed_sse_enabled   = true
}

data "aws_iam_policy_document" "consumer" {
  count = var.consumers

  statement {
    sid       = "AllowDelivery"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.consumer[count.index].arn]

    principals {
      type        = "Service"
      identifiers = [%[1]q]
    }

    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [%[2]s]
    }
  }
}

resource "aws_sqs_queue_policy" "consumer" {
  count = var.consumers

  queue_url = aws_sqs_queue.consumer[count.index].id
  policy    = data.aws_iam_policy_document.consumer[count.index].json
}
`, principal, sourceARN, sqsMaxRetentionSeconds)
}

func consumersVariable(w workload) advise.TerraformVariable {
	return advise.TerraformVariable{Name: "consumers", Type: "number", Default: max(1, w.consumers),
		Description: "Independent consumers; each gets its own queue."}
}

func snsTerraform(w workload) advise.TerraformCode {
	vars := append([]advise.TerraformVariable{consumersVariable(w)}, queueVariables(w)...)
	filter := ""
	if w.routing {
		vars = append(vars, advise.TerraformVariable{Name: "filter_policies", Type: "list(string)", Default: []string{},
			Description: "SNS filter policy (JSON) for each consumer's subscription, in order; a consumer without one gets every message."})
		filter = "\n  filter_policy        = length(var.filter_policies) > count.index ? var.filter_policies[count.index] : null"
	}
	resources := `resource "aws_sns_topic" "main" {
  name              = var.name
  kms_master_key_id = "alias/aws/sns"
}

resource "aws_sns_topic_subscription" "consumer" {
  count = var.consumers

  topic_arn            = aws_sns_topic.main.arn
  protocol             = "sqs"
  endpoint             = aws_sqs_queue.consumer[count.index].arn
  raw_message_delivery = true` + filter + `
}

` + consumerQueues("sns.amazonaws.com", "aws_sns_topic.main.arn")
	return advise.TerraformCode{
		Resources: resources,
		Variables: vars,
		Outputs: []advise.TerraformOutput{
			{Name: "topic_arn", Description: "ARN producers publish to.", Value: "aws_sns_topic.main.arn"},
			{Name: "consumer_queue_urls", Description: "URL of each consumer's queue.", Value: "aws_sqs_queue.consumer[*].url"},
			{Name: "dead_letter_queue_url", Description: "URL of the shared dead-letter queue.", Value: "aws_sqs_queue.dlq.url"},
		},
		Endpoints: []string{"sns", "sqs"},
	}
}

func eventBridgeTerraform(w workload) advise.TerraformCode {
	vars := append([]advise.TerraformVariable{
		consumersVariable(w),
		{Name: "event_patterns", Type: "list(string)", Default: []string{},
			Description: "Event pattern (JSON) of each consumer's rule, in order; a consumer without one matches events whose source is var.name."},
	}, queueVariables(w)...)
	resources := `resource "aws_cloudwatch_event_bus" "main" {
  name = var.name
}

# One rule per consumer, so each can match the events it needs.
resource "aws_cloudwatch_event_rule" "consumer" {
  count = var.consumers

  name           = "${var.name}-${count.index}"
  event_bus_name = aws_cloudwatch_event_bus.main.name
  event_pattern  = length(var.event_patterns) > count.index ? var.event_patterns[count.index] : jsonencode({ source = [var.name] })
}

resource "aws_cloudwatch_event_target" "consumer" {
  count = var.consumers

  rule           = aws_cloudwatch_event_rule.consumer[count.index].name
  event_bus_name = aws_cloudwatch_event_bus.main.name
  target_id      = "queue"
  arn            = aws_sqs_queue.consumer[count.index].arn
}

` + consumerQueues("events.amazonaws.com", "aws_cloudwatch_event_rule.consumer[count.index].arn")
	return advise.TerraformCode{
		Resources: resources,
		Variables: vars,
		Outputs: []advise.TerraformOutput{
			{Name: "event_bus_name", Description: "Bus producers put events on.", Value: "aws_cloudwatch_event_bus.main.name"},
			{Name: "event_bus_arn", Description: "ARN of the bus, for IAM policies.", Value: "aws_cloudwatch_event_bus.main.arn"},
			{Name: "consumer_queue_urls", Description: "URL of each consumer's queue.", Value: "aws_sqs_queue.consumer[*].url"},
		},
		Endpoints: []string{"eventbridge", "sqs"},
	}
}

func kinesisTerraform(w workload, q quotaReader) advise.TerraformCode {
	shards, _ := provisionedShards(w, q)
	hours := int(math.Ceil(w.retentionDays * 24))
	hours = min(max(hours, kinesisMinRetention), kinesisMaxRetention)
	resources := `resource "aws_kinesis_stream" "main" {
  name             = var.name
  shard_count      = var.shard_count
  retention_period = var.retention_hours
  encryption_type  = "KMS"
  kms_key_id       = "alias/aws/kinesis"

  stream_mode_details {
    stream_mode = "PROVISIONED"
  }
}
`
	if w.consumers > 1 {
		resources += `
# Consumers sharing a shard share its 2 MiB/s of reads. If they fall
# behind, register each with aws_kinesis_stream_consumer (enhanced fan-out).
`
	}
	return advise.TerraformCode{
		Resources: resources,
		Variables: []advise.TerraformVariable{
			{Name: "shard_count", Type: "number", Default: shards,
				Description: "Provisioned shards, sized for the stated peak from the per-shard write quotas (1,000 records/s and 1 MiB/s each)."},
			{Name: "retention_hours", Type: "number", Default: hours,
				Description: "How long the stream keeps records (24 to 8,760 hours), from the stated retention."},
		},
		Outputs: []advise.TerraformOutput{
			{Name: "stream_name", Description: "Stream producers put records to.", Value: "aws_kinesis_stream.main.name"},
			{Name: "stream_arn", Description: "ARN of the stream, for IAM policies and event source mappings.", Value: "aws_kinesis_stream.main.arn"},
		},
		Endpoints: []string{"kinesis"},
	}
}

func mskTerraform(w workload) advise.TerraformCode {
	stored := w.average * w.sizeBytes * w.retentionDays * secondsPerDay * mskReplication / bytesPerGB
	volume := max(mskMinVolumeGB, int(math.Ceil(stored/mskBrokers*mskVolumeHeadroom)))
	resources := fmt.Sprintf(`resource "aws_msk_cluster" "main" {
  cluster_name           = var.name
  kafka_version          = var.kafka_version
  number_of_broker_nodes = %d # one per subnet; use as many subnets as Availability Zones

  broker_node_group_info {
    instance_type   = "kafka.m7g.large"
    client_subnets  = var.subnet_ids
    security_groups = var.security_group_ids

    storage_info {
      ebs_storage_info {
        volume_size = var.broker_volume_gb
      }
    }
  }

  client_authentication {
    sasl {
      iam = true
    }
  }

  encryption_info {
    encryption_in_transit {
      client_broker = "TLS"
      in_cluster    = true
    }
  }
}
`, mskBrokers)
	return advise.TerraformCode{
		Resources: resources,
		Variables: []advise.TerraformVariable{
			{Name: "subnet_ids", Type: "list(string)", Description: fmt.Sprintf("%d private subnets, one per Availability Zone, for the brokers.", mskBrokers)},
			{Name: "security_group_ids", Type: "list(string)", Description: "Security groups that let clients reach the brokers on port 9098 (IAM auth)."},
			{Name: "kafka_version", Type: "string", Default: mskKafkaVersion, Description: "Apache Kafka version."},
			{Name: "broker_volume_gb", Type: "number", Default: volume,
				Description: "EBS storage per broker in GiB: twice the data retained at the average rate with replication factor 3, at least 100."},
		},
		Outputs: []advise.TerraformOutput{
			{Name: "cluster_arn", Description: "ARN of the cluster.", Value: "aws_msk_cluster.main.arn"},
			{Name: "bootstrap_brokers", Description: "Bootstrap brokers for IAM-authenticated clients.", Value: "aws_msk_cluster.main.bootstrap_brokers_sasl_iam"},
		},
		Endpoints: []string{"kafka"},
	}
}
