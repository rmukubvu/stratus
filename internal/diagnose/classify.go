// Package diagnose turns raw request failures into a short, structured
// explanation that a developer or a coding agent can act on: what failed,
// how often, the likely cause, and the next step to take.
//
// The aim is to stop agents from chasing ghosts. A throttle injected by a
// chaos rule, an operation Stratus does not emulate, and a resource that a
// reset removed all look like "the test failed" in a raw log, but each
// calls for a different response.
package diagnose

import (
	"fmt"
	"strings"
)

// Category groups failures by what the caller should do about them.
type Category string

const (
	// CategoryInjected failures were caused on purpose by a chaos rule.
	CategoryInjected Category = "injected_fault"
	// CategoryUnsupported failures hit an operation Stratus does not emulate.
	CategoryUnsupported Category = "unsupported_operation"
	// CategoryMissingResource failures reference something that does not exist.
	CategoryMissingResource Category = "missing_resource"
	// CategoryConflict failures collide with existing state.
	CategoryConflict Category = "conflict"
	// CategoryInvalidRequest failures were rejected as malformed.
	CategoryInvalidRequest Category = "invalid_request"
	// CategoryLimit failures were throttled by the active limits profile,
	// as AWS would throttle them at this load.
	CategoryLimit Category = "limit_exceeded"
	// CategoryThrottling failures were rate limited.
	CategoryThrottling Category = "throttling"
	// CategorySignature failures were refused by strict signature mode: the
	// request was not signed the way AWS requires.
	CategorySignature Category = "signature"
	// CategoryAuth failures were rejected for identity or permission reasons.
	CategoryAuth Category = "auth"
	// CategoryConnection failures never got a response.
	CategoryConnection Category = "connection"
	// CategoryEmulator failures are server-side errors inside Stratus.
	CategoryEmulator Category = "emulator_error"
	// CategoryClient covers other 4xx responses.
	CategoryClient Category = "client_error"
	// CategoryEnvironment failures come from the machine Stratus runs on,
	// such as Docker not running or a Lambda runtime image not pulled.
	CategoryEnvironment Category = "environment"
	// CategoryFunctionError failures are Lambda functions that threw (or
	// reported failed batch items) while handling an event Stratus
	// delivered, such as an SQS message from an event source mapping.
	CategoryFunctionError Category = "function_error"
)

// FunctionErrorCode is the Code of a delivery failure where the target
// function ran and failed, as opposed to Stratus failing to run it.
const FunctionErrorCode = "FunctionError"

// Explanation is the advice attached to a group of failures.
type Explanation struct {
	Category    Category `json:"category"`
	LikelyCause string   `json:"likely_cause"`
	NextStep    string   `json:"next_step"`
	// YourBug is false when the failure is not a defect in the code under
	// test (injected faults, emulator gaps), so an agent should not try to
	// "fix" it there.
	YourBug bool `json:"your_bug"`
}

// failure is the input to classification.
type failure struct {
	service, operation, code, message string
	status                            int
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// explain classifies a failure that was not injected by chaos.
func explain(f failure) Explanation {
	code := f.code
	lower := strings.ToLower(code)
	op := describeOp(f.service, f.operation)

	switch {
	case code == "ConnectionDropped":
		return Explanation{CategoryConnection,
			"the connection was closed before a response was sent",
			"make sure the client has timeouts and retries; check Stratus logs for the request ID",
			false}
	case containsAny(code, "NotImplemented") || code == "InvalidAction" || code == "UnknownOperationException":
		return Explanation{CategoryUnsupported,
			fmt.Sprintf("Stratus does not emulate %s yet; this is an emulator gap, not a bug in your code", op),
			"avoid this call in local tests, stub it, or report the gap; do not change application code to work around it",
			false}
	case isMissingObjectLock(f):
		return Explanation{CategoryMissingResource,
			fmt.Sprintf("%s needs Object Lock on the bucket, and the bucket has none. S3 only has it when the bucket was created with Object Lock enabled (ObjectLockEnabledForBucket, the x-amz-bucket-object-lock-enabled header) or got PutObjectLockConfiguration while versioned. If your setup does create the bucket that way, the gap is in the emulator, not your code: Stratus 0.2.0 and earlier ignored that header", op),
			"check with `aws s3api get-object-lock-configuration --bucket <bucket>`; if setup already creates the bucket with Object Lock, upgrade Stratus (0.3.0 or later) and recreate the bucket rather than changing application code",
			false}
	case containsAny(lower, "throttl", "slowdown", "throughputexceeded", "toomanyrequests", "limitexceeded"):
		return Explanation{CategoryThrottling,
			"the request was rate limited",
			"retry with exponential backoff; AWS SDKs do this by default unless retries are disabled",
			true}
	case containsAny(code, "NotFound", "NoSuch", "DoesNotExist", "NotExist") || code == "NoSuchEntity":
		next := "create it in your setup step before use, and check the name, region and account in the request"
		if list := listHint(f.service); list != "" {
			next += "; list what exists with " + list
		}
		return Explanation{CategoryMissingResource,
			fmt.Sprintf("%s referenced a resource that does not exist; common causes are a typo, setup running in the wrong order, or a reset or snapshot restore that removed it", op),
			next,
			true}
	case containsAny(code, "AlreadyExists", "Exists", "Conflict", "InUse", "NotEmpty"):
		return Explanation{CategoryConflict,
			fmt.Sprintf("%s collided with existing state", op),
			fmt.Sprintf("make setup idempotent, or start from a clean slate with `stratus reset %s`", f.service),
			true}
	case containsAny(code, "Validation", "InvalidParameter", "InvalidArgument", "BadRequest", "Malformed", "InvalidDefinition", "InvalidArn", "SerializationException", "MissingParameter"):
		return Explanation{CategoryInvalidRequest,
			fmt.Sprintf("%s was rejected as invalid: %s", op, f.message),
			"fix the request parameters named in the message",
			true}
	case containsAny(code, "NotAuthorized", "AccessDenied", "Unauthorized", "InvalidClientTokenId", "UnrecognizedClient", "SignatureDoesNotMatch", "ExpiredToken"):
		return Explanation{CategoryAuth,
			"the request was rejected for identity or permission reasons; unless strict signature mode is on (STRATUS_SIGV4=strict), Stratus accepts any signature, so a signature or token error usually means the request went to real AWS or another endpoint",
			"check that AWS_ENDPOINT_URL points at Stratus; with STRATUS_IAM=enforce, check the caller's policies; for Cognito, check the user's credentials and state",
			true}
	case f.status >= 500 && isEnvironmentProblem(f.message):
		return Explanation{CategoryEnvironment,
			fmt.Sprintf("%s could not run on this machine: %s", op, f.message),
			"fix the environment as the message says (start Docker, or pull the image it names), then retry; this is not a bug in your code",
			false}
	case f.status >= 500:
		return Explanation{CategoryEmulator,
			fmt.Sprintf("Stratus failed while handling %s: %s", op, f.message),
			"this is likely an emulator bug; look up the request ID in Stratus logs or the operator portal and report it",
			false}
	default:
		return Explanation{CategoryClient,
			fmt.Sprintf("%s failed with %s: %s", op, code, f.message),
			"read the error message and check the request",
			true}
	}
}

// isEnvironmentProblem matches server errors caused by the host rather
// than by Stratus or the caller: the Lambda runtime needs Docker and its
// runtime images.
func isEnvironmentProblem(message string) bool {
	lower := strings.ToLower(message)
	return containsAny(lower, "docker", "image is not available", "lambda runtime is not configured")
}

// isMissingObjectLock matches the errors S3 returns when a request needs a
// bucket's Object Lock configuration and there is none. Whether that is the
// caller's setup or the emulator cannot be told from the failure alone, so
// it is not reported as a bug in the code under test.
func isMissingObjectLock(f failure) bool {
	switch f.code {
	case "ObjectLockConfigurationNotFoundError":
		return true
	case "InvalidRequest":
		return strings.Contains(strings.ToLower(f.message), "object lock configuration")
	}
	return false
}

func describeOp(service, operation string) string {
	switch {
	case service != "" && operation != "":
		return service + " " + operation
	case service != "":
		return service
	default:
		return "the request"
	}
}

var listCommands = map[string]string{
	"s3":             "`aws s3api list-buckets`",
	"sqs":            "`aws sqs list-queues`",
	"sns":            "`aws sns list-topics`",
	"dynamodb":       "`aws dynamodb list-tables`",
	"lambda":         "`aws lambda list-functions`",
	"events":         "`aws events list-rules`",
	"kinesis":        "`aws kinesis list-streams`",
	"secretsmanager": "`aws secretsmanager list-secrets`",
	"ssm":            "`aws ssm describe-parameters`",
	"stepfunctions":  "`aws stepfunctions list-state-machines`",
	"iam":            "`aws iam list-roles`",
	"logs":           "`aws logs describe-log-groups`",
	"kms":            "`aws kms list-keys`",
	"cloudformation": "`aws cloudformation describe-stacks`",
}

func listHint(service string) string { return listCommands[service] }

// explainDelivery explains an event delivery that failed for real (not by
// chaos). Nobody sees these fail: the producer's API call succeeded, and
// the event never had an effect.
func explainDelivery(f Failure) Explanation {
	route := fmt.Sprintf("an event from %s to %s %s", f.Delivery.Source, f.Service, f.Operation)
	if f.Code == FunctionErrorCode {
		function := lastSegment(f.Delivery.TargetARN)
		return Explanation{
			Category:    CategoryFunctionError,
			LikelyCause: fmt.Sprintf("function %s failed while handling %s: %s", function, route, f.Message),
			NextStep: fmt.Sprintf("read what the function printed with `aws logs filter-log-events --log-group-name /aws/lambda/%s` (or invoke it with the same event and --log-type Tail), fix the handler, and resend the event; "+
				"the producer's call succeeded, so nothing upstream reports this failure", function),
			YourBug: true,
		}
	}
	exp := explain(failure{service: f.Service, operation: f.Operation, code: f.Code, message: f.Message, status: f.Status})
	exp.LikelyCause = fmt.Sprintf("Stratus could not deliver %s: %s", route, exp.LikelyCause)
	exp.NextStep += "; the producer's call succeeded, so nothing upstream reports this failure"
	return exp
}

func lastSegment(arn string) string {
	if i := strings.LastIndex(arn, ":"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// explainInjected describes a failure that a chaos rule caused.
// explainLimit explains a throttle from the limits profile.
func explainLimit(ruleID, kind string) Explanation {
	switch ruleID {
	case "dynamodb-partition-capacity":
		return Explanation{
			Category:    CategoryLimit,
			LikelyCause: "one DynamoDB partition ran out of capacity while the table had room: a hot partition key, as it would be in AWS",
			NextStep:    "spread writes across more partition key values (add a suffix or shard the key), cache hot reads, and retry throttled requests and UnprocessedItems with backoff",
			YourBug:     false,
		}
	case "lambda-reserved-concurrency", "lambda-account-concurrency":
		return Explanation{
			Category:    CategoryLimit,
			LikelyCause: "the Lambda function had no free concurrent execution (" + ruleID + "), as it would in AWS at this load",
			NextStep:    "retry synchronous invokes with backoff or move them behind a queue; raise the function's reserved concurrency, free account concurrency, or make the handler faster so executions finish sooner",
			YourBug:     false,
		}
	case "dynamodb-table-capacity":
		return Explanation{
			Category:    CategoryLimit,
			LikelyCause: "the DynamoDB table used up its provisioned (or on-demand maximum) capacity, as it would in AWS at this load",
			NextStep:    "retry with backoff and resubmit UnprocessedItems; raise the table's capacity, switch it to on-demand, or smooth the load with a queue",
			YourBug:     false,
		}
	}
	what := "request rate"
	if kind == "concurrency" {
		what = "concurrency"
	}
	return Explanation{
		Category:    CategoryLimit,
		LikelyCause: fmt.Sprintf("the request exceeded the %s quota of limits rule %q, as it would in AWS at this load; the throttle itself is expected", what, ruleID),
		NextStep:    "check the caller retried with backoff (AWS SDKs do by default); if the work still failed, add retries, batch or cache the calls, or put a queue in front; raise the quota in the profile only if AWS would allow it (`stratus limits status` shows the rule)",
		YourBug:     false,
	}
}

func explainInjected(ruleID, action string) Explanation {
	next := "this is expected during a chaos experiment; check that the code under test handles it"
	switch action {
	case "throttle":
		next = "expected during the chaos experiment; verify the code retries with backoff instead of failing"
	case "drop":
		next = "expected during the chaos experiment; verify the client has timeouts and retries idempotent calls"
	case "internal_error", "unavailable", "error":
		next = "expected during the chaos experiment; verify the code retries or degrades gracefully"
	}
	return Explanation{
		Category:    CategoryInjected,
		LikelyCause: fmt.Sprintf("injected on purpose by chaos rule %q (%s)", ruleID, action),
		NextStep:    next + "; replay with the same seed to reproduce",
		YourBug:     false,
	}
}

// explainSignature explains a request strict signature mode refused. The
// signing bug is in the caller (its SDK setup, a custom signer, a proxy
// that rewrites requests), and AWS would refuse the same request.
func explainSignature(kind, detail string) Explanation {
	cause, next := "the request's SigV4 signature did not verify", "compare your signer's canonical request with the one in `stratus sigv4 status`"
	switch kind {
	case "signature_mismatch":
		cause = "the signature does not match the request: something was signed differently from what was sent (a header, query parameter, path encoding or body changed after signing, the wrong region or service in the credential scope, or the wrong secret key)"
		next = "run `stratus sigv4 status` and diff its canonical_request against what your signer built; the first differing line is the bug"
	case "unknown_access_key":
		cause = "the access key is not one Stratus knows, so AWS would not know it either"
		next = "sign with an IAM user key, STS credentials, a root key (\"test\" or a twelve-digit account ID, secret \"test\"), or list the key in STRATUS_SIGV4_CREDENTIALS"
	case "invalid_token", "expired_token":
		cause = "the session token is missing, wrong, unexpected or expired"
		next = "send X-Amz-Security-Token with STS credentials (and only with them), and refresh credentials before they expire"
	case "request_time_skewed":
		cause = "the request was signed more than 15 minutes away from the server clock: a stale cached signature, a retried request that was not re-signed, or a wrong clock"
		next = "sign each request (and each retry) just before sending it"
	case "presign_expired":
		cause = "the presigned URL was used after X-Amz-Expires ran out"
		next = "presign closer to use or with a longer expiry (at most 604800 seconds)"
	case "missing_authentication":
		cause = "the request was not signed at all"
		next = "make sure the client has credentials configured and is not sending anonymous requests"
	case "unsigned_headers", "missing_content_hash", "invalid_content_hash", "content_hash_mismatch", "malformed_chunk":
		cause = "the S3 request breaks S3's signing rules for headers or payload"
	case "incomplete_signature", "unsupported_algorithm":
		cause = "the Authorization header or presigned query is malformed or uses an unsupported algorithm"
		next = "sign with AWS4-HMAC-SHA256 and include Credential, SignedHeaders and Signature"
	}
	if detail != "" && kind != "signature_mismatch" {
		cause += ": " + detail
	}
	return Explanation{Category: CategorySignature, LikelyCause: cause, NextStep: next, YourBug: true}
}
