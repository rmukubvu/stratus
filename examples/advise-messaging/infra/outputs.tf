output "stream_name" {
  description = "Stream producers put records to."
  value       = aws_kinesis_stream.main.name
}

output "stream_arn" {
  description = "ARN of the stream, for IAM policies and event source mappings."
  value       = aws_kinesis_stream.main.arn
}
