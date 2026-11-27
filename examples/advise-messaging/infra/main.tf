resource "aws_kinesis_stream" "main" {
  name             = var.name
  shard_count      = var.shard_count
  retention_period = var.retention_hours
  encryption_type  = "KMS"
  kms_key_id       = "alias/aws/kinesis"

  stream_mode_details {
    stream_mode = "PROVISIONED"
  }
}

# Consumers sharing a shard share its 2 MiB/s of reads. If they fall
# behind, register each with aws_kinesis_stream_consumer (enhanced fan-out).
