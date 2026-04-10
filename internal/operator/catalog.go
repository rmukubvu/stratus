package operator

type ServiceDescriptor struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Mode     string `json:"mode"`
	Summary  string `json:"summary"`
}

type QuickStartExample struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Command     string `json:"command"`
}

func SupportedServices() []ServiceDescriptor {
	return []ServiceDescriptor{
		{Name: "sts", Category: "identity", Mode: "in-process", Summary: "Caller identity and basic account context for SDK and CLI bootstrapping."},
		{Name: "s3", Category: "storage", Mode: "stateful", Summary: "Path-style object storage with durable metadata, blobs, and notifications."},
		{Name: "lambda", Category: "compute", Mode: "container-backed", Summary: "Function control plane plus Docker-backed sync and async execution."},
		{Name: "apigateway", Category: "edge", Mode: "in-process", Summary: "REST API control plane and Lambda proxy integrations."},
		{Name: "apigatewayv2", Category: "edge", Mode: "in-process", Summary: "HTTP API routes and local execute-api invocation path."},
		{Name: "dynamodb", Category: "database", Mode: "stateful", Summary: "Durable key-value tables, queries, scans, and stream emission."},
		{Name: "dynamodbstreams", Category: "events", Mode: "stateful", Summary: "Stream shards and records that feed Lambda event source mappings."},
		{Name: "cloudformation", Category: "control plane", Mode: "in-process", Summary: "Executable stack engine for the supported local AWS subset."},
		{Name: "iam", Category: "identity", Mode: "stateful", Summary: "Roles and inline policies with permissive local semantics."},
		{Name: "ssm", Category: "config", Mode: "stateful", Summary: "Parameter Store for local configuration and secret indirection."},
		{Name: "logs", Category: "observability", Mode: "stateful", Summary: "CloudWatch Logs groups, streams, and event capture."},
		{Name: "monitoring", Category: "observability", Mode: "stateful", Summary: "CloudWatch metrics and alarms for local readiness checks."},
		{Name: "sns", Category: "events", Mode: "stateful", Summary: "Topics, subscriptions, filter policies, and local fanout delivery."},
		{Name: "sqs", Category: "messaging", Mode: "stateful", Summary: "Queues, DLQs, visibility control, and event source integration."},
		{Name: "events", Category: "events", Mode: "stateful", Summary: "EventBridge buses, rules, schedules, and target delivery."},
		{Name: "kms", Category: "security", Mode: "stateful", Summary: "Keys, aliases, and local encrypt/decrypt flows."},
		{Name: "secretsmanager", Category: "security", Mode: "stateful", Summary: "Secret storage and retrieval for app and infra paths."},
		{Name: "kinesis", Category: "streaming", Mode: "stateful", Summary: "Streams, records, iterators, and Lambda consumption hooks."},
		{Name: "cognitoidp", Category: "identity", Mode: "stateful", Summary: "User pools, clients, auth flows, and admin user operations."},
		{Name: "stepfunctions", Category: "orchestration", Mode: "in-process", Summary: "State machine execution for the supported workflow subset."},
		{Name: "ecr", Category: "containers", Mode: "stateful", Summary: "Image registry metadata and repository control plane."},
		{Name: "ecs", Category: "containers", Mode: "stateful", Summary: "Clusters, task definitions, and service control-plane workflows."},
		{Name: "elbv2", Category: "networking", Mode: "stateful", Summary: "Load balancer, listener, and target group control plane."},
		{Name: "acm", Category: "security", Mode: "stateful", Summary: "Certificate lifecycle metadata for local ingress flows."},
		{Name: "rds", Category: "database", Mode: "stateful", Summary: "RDS control-plane subset for deploy-path realism."},
		{Name: "elasticache", Category: "database", Mode: "stateful", Summary: "Cache cluster control-plane subset for local stack parity."},
	}
}

func QuickStartExamples(endpoint string) []QuickStartExample {
	return []QuickStartExample{
		{
			Title:       "Connect the AWS CLI",
			Description: "Set a local endpoint once and use the real AWS CLI against Stratus.",
			Command:     "export AWS_ACCESS_KEY_ID=test\nexport AWS_SECRET_ACCESS_KEY=test\nexport AWS_REGION=us-east-1\nexport AWS_PAGER=\naws --endpoint-url " + endpoint + " sts get-caller-identity",
		},
		{
			Title:       "Run the Java SDK smoke",
			Description: "Exercise STS and DynamoDB with the Java AWS SDK v2 fixture.",
			Command:     "cd /Users/robson/awsdev/stratus-git\n./scripts/smoke-java-sdk.sh",
		},
		{
			Title:       "Validate a CDK stack with Preflight",
			Description: "Lint, score, diagnose, and deploy against the running local emulator.",
			Command:     "cd /Users/robson/code/preflight/test/fixtures/cdk-http-sqs-ddb\n/Users/robson/code/preflight/dist/preflight lint --stack-name SmokeFixtureStack --no-ai\n/Users/robson/code/preflight/dist/preflight deploy --stack-name SmokeFixtureStack --no-ai",
		},
		{
			Title:       "Load-check a behavioral path",
			Description: "Replay the existing behavioral assertions under concurrent local load.",
			Command:     "cd /Users/robson/code/preflight/test/fixtures/cdk-http-sqs-ddb\n/Users/robson/code/preflight/dist/preflight load --stack-name SmokeFixtureStack --runner auto --vus 8 --iterations 40",
		},
	}
}
