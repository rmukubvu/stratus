variable "name" {
  description = "Prefix for every resource name."
  type        = string
  default     = "order-events"
}

variable "region" {
  description = "AWS region to deploy to."
  type        = string
  default     = "eu-west-1"
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = { "adr" = "adr-d686a61e207f" }
}

variable "shard_count" {
  description = "Provisioned shards, sized for the stated peak from the per-shard write quotas (1,000 records/s and 1 MiB/s each)."
  type        = number
  default     = 4
}

variable "retention_hours" {
  description = "How long the stream keeps records (24 to 8,760 hours), from the stated retention."
  type        = number
  default     = 168
}
