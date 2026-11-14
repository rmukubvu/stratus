package limits

// defaultRates are the aws-default rate quotas: the default per-account
// (or per-prefix) request rates AWS documents for the APIs most often
// called on a hot path. Some vary by region; these are the common
// defaults. Override or disable any of them by ID in a profile.
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
			// per-account rate (5,500 per second in most regions).
			ID: "kms-cryptographic-operations", Service: "kms", Per: ScopeAccount,
			Operations: []string{"Encrypt", "Decrypt", "ReEncrypt", "GenerateDataKey", "GenerateDataKeyWithoutPlaintext", "GenerateDataKeyPair", "GenerateDataKeyPairWithoutPlaintext", "GenerateRandom", "GenerateMac", "VerifyMac", "Sign", "Verify"},
			PerSecond:  5500, BurstSeconds: 1,
		},
		{
			// Secrets Manager: GetSecretValue and DescribeSecret share
			// 10,000 requests per second.
			ID: "secretsmanager-get-secret-value", Service: "secretsmanager", Per: ScopeAccount,
			Operations: []string{"GetSecretValue", "DescribeSecret", "BatchGetSecretValue"},
			PerSecond:  10000, BurstSeconds: 1,
		},
	}
}
