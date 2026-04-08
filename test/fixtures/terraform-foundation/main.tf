terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

variable "endpoint_url" {
  type    = string
  default = "http://127.0.0.1:4566"
}

provider "aws" {
  access_key                  = "test"
  secret_key                  = "test"
  region                      = "us-east-1"
  s3_use_path_style           = true
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true

  endpoints {
    cloudwatchlogs = var.endpoint_url
    dynamodb       = var.endpoint_url
    iam            = var.endpoint_url
    kms            = var.endpoint_url
    sqs            = var.endpoint_url
    ssm            = var.endpoint_url
    sts            = var.endpoint_url
  }
}

resource "aws_kms_key" "fixture" {
  description = "stratus terraform fixture key"
}

resource "aws_kms_alias" "fixture" {
  name          = "alias/stratus-fixture"
  target_key_id = aws_kms_key.fixture.key_id
}

resource "aws_cloudwatch_log_group" "fixture" {
  name = "/stratus/terraform/fixture"
}

resource "aws_sqs_queue" "fixture" {
  name                       = "stratus-terraform-fixture"
  visibility_timeout_seconds = 45
}

resource "aws_dynamodb_table" "fixture" {
  name         = "stratus_terraform_fixture"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "id"

  attribute {
    name = "id"
    type = "S"
  }
}

resource "aws_ssm_parameter" "fixture" {
  name  = "/stratus/terraform/fixture"
  type  = "String"
  value = "ready"
}

output "kms_key_arn" {
  value = aws_kms_key.fixture.arn
}

output "queue_url" {
  value = aws_sqs_queue.fixture.url
}
