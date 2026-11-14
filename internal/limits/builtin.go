package limits

// defaultRates are the aws-default rate quotas: the default per-account
// (or per-prefix) request rates AWS documents for the APIs most often
// called on a hot path, checked against the AWS documentation in November
// 2026. Where AWS sets a quota higher in some regions, RegionPerSecond
// carries those. Override or disable any of them by ID in a profile.
//
// DynamoDB capacity and Lambda concurrency are not request-rate quotas and
// are modelled separately.
func defaultRates() []RateRule {
	return []RateRule{
		{
			// S3 scales to at least 5,500 GET/HEAD requests per second per
			// partitioned prefix.
			ID: "s3-reads-per-prefix", Service: "s3", Per: ScopePrefix,
			Operations: []string{"GetObject", "HeadObject"},
			PerSecond:  5500, BurstSeconds: 1,
		},
		{
			// ...and 3,500 PUT/COPY/POST/DELETE requests per second.
			ID: "s3-writes-per-prefix", Service: "s3", Per: ScopePrefix,
			Operations: []string{"PutObject", "CopyObject", "DeleteObject", "DeleteObjects", "CreateMultipartUpload", "CompleteMultipartUpload"},
			PerSecond:  3500, BurstSeconds: 1,
		},
		{
			// Parameter Store standard throughput: 40 transactions per
			// second shared by the Get* APIs, unless higher throughput is
			// turned on.
			ID: "ssm-get-parameters", Service: "ssm", Per: ScopeAccount,
			Operations: []string{"GetParameter", "GetParameters", "GetParametersByPath"},
			PerSecond:  40, BurstSeconds: 1,
		},
		{
			// KMS cryptographic operations on symmetric keys share one
			// per-account rate: 10,000 per second, 20,000 or 100,000 in
			// the regions below. Asymmetric keys have their own, lower
			// quotas, which Stratus does not model.
			ID: "kms-cryptographic-operations", Service: "kms", Per: ScopeAccount,
			Operations: []string{"Encrypt", "Decrypt", "ReEncrypt", "GenerateDataKey", "GenerateDataKeyWithoutPlaintext", "GenerateRandom", "GenerateMac", "VerifyMac"},
			PerSecond:  10000, BurstSeconds: 1,
			RegionPerSecond: map[string]float64{
				"us-east-2": 20000, "ap-southeast-1": 20000, "ap-southeast-2": 20000, "ap-northeast-1": 20000,
				"eu-central-1": 20000, "eu-west-2": 20000, "eu-south-2": 20000,
				"us-east-1": 100000, "us-west-2": 100000, "eu-west-1": 100000,
			},
		},
		{
			// Secrets Manager gives each read API its own rate:
			// GetSecretValue 10,000 per second...
			ID: "secretsmanager-get-secret-value", Service: "secretsmanager", Per: ScopeAccount,
			Operations: []string{"GetSecretValue"},
			PerSecond:  10000, BurstSeconds: 1,
		},
		{
			// ...DescribeSecret 40,000...
			ID: "secretsmanager-describe-secret", Service: "secretsmanager", Per: ScopeAccount,
			Operations: []string{"DescribeSecret"},
			PerSecond:  40000, BurstSeconds: 1,
		},
		{
			// ...and BatchGetSecretValue 100.
			ID: "secretsmanager-batch-get-secret-value", Service: "secretsmanager", Per: ScopeAccount,
			Operations: []string{"BatchGetSecretValue"},
			PerSecond:  100, BurstSeconds: 1,
		},
	}
}
