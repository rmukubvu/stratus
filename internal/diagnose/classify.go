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
	// CategoryThrottling failures were rate limited.
	CategoryThrottling Category = "throttling"
	// CategoryAuth failures were rejected for identity or permission reasons.
	CategoryAuth Category = "auth"
	// CategoryConnection failures never got a response.
	CategoryConnection Category = "connection"
	// CategoryEmulator failures are server-side errors inside Stratus.
	CategoryEmulator Category = "emulator_error"
	// CategoryClient covers other 4xx responses.
	CategoryClient Category = "client_error"
)

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
			"the request was rejected for identity or permission reasons; Stratus accepts any AWS credentials, so a signature or token error usually means the request went to real AWS or another endpoint",
			"check that AWS_ENDPOINT_URL points at Stratus; for Cognito, check the user's credentials and state",
			true}
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

// explainInjected describes a failure that a chaos rule caused.
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
