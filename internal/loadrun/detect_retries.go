package loadrun

import "fmt"

// Retry thresholds.
const (
	retryTrackedCalls   = 200_000 // SDK calls followed per run
	retryMinFailures    = 5       // calls that failed for good before reporting
	retryMinUntracked   = 20      // failed requests from non-SDK clients
	retryStormShare     = 0.2     // retries as a share of all requests
	retryStormMinTotals = 50
)

// retries follows AWS SDK calls across their attempts, using the
// amz-sdk-invocation-id and amz-sdk-request headers every AWS SDK sends,
// to find calls that failed for good: with retries turned off, or after
// using up every attempt. It also spots retry storms, where retries are a
// large share of the traffic. Counters are kept as calls change state, so
// reporting costs nothing however many calls the run made.
type retries struct {
	// calls is capped; calls past the cap are not followed.
	calls map[string]*sdkCall

	requests, retryAttempts int
	// Calls whose latest attempt failed in a way an SDK retries, by why
	// no further attempt is expected.
	failedNoRetries, failedExhausted int
	// failedOther counts retryable failures from clients that send no SDK
	// headers, so whether they retried is unknown.
	failedOther int
}

type sdkCall struct {
	attempt, max int
	state        callState
}

type callState int

const (
	callOK callState = iota
	callFailedNoRetries
	callFailedExhausted
	callFailedRetrying
)

func newRetries() *retries { return &retries{calls: map[string]*sdkCall{}} }

func (r *retries) observe(o Observation, oc outcome, _ int) {
	r.requests++
	if o.Attempt > 1 {
		r.retryAttempts++
	}
	failed := retryable(o, oc)
	if o.InvocationID == "" {
		if failed {
			r.failedOther++
		}
		return
	}
	c := r.calls[o.InvocationID]
	if c == nil {
		if len(r.calls) >= retryTrackedCalls {
			return
		}
		c = &sdkCall{}
		r.calls[o.InvocationID] = c
	}
	if o.Attempt < c.attempt {
		return // an earlier attempt arriving late
	}
	r.move(c, callOK)
	c.attempt, c.max = o.Attempt, o.MaxAttempts
	switch {
	case !failed:
	case o.MaxAttempts == 1:
		r.move(c, callFailedNoRetries)
	case o.MaxAttempts > 0 && o.Attempt >= o.MaxAttempts:
		r.move(c, callFailedExhausted)
	default:
		r.move(c, callFailedRetrying)
	}
}

// move changes a call's state, keeping the counters in step.
func (r *retries) move(c *sdkCall, to callState) {
	r.adjust(c.state, -1)
	c.state = to
	r.adjust(to, 1)
}

func (r *retries) adjust(s callState, d int) {
	switch s {
	case callFailedNoRetries:
		r.failedNoRetries += d
	case callFailedExhausted:
		r.failedExhausted += d
	}
}

func (r *retries) findings(runFacts) []Finding {
	var out []Finding
	if r.failedNoRetries >= retryMinFailures {
		out = append(out, Finding{
			Detector: DetectorMissingRetries, Severity: SeverityHigh,
			Title:  fmt.Sprintf("Retries are off: %d calls failed on their only attempt", r.failedNoRetries),
			Detail: fmt.Sprintf("%d AWS SDK calls were made with max attempts set to 1 and were throttled or failed with a server error, so the error went straight to the application.", r.failedNoRetries),
			Suggestion: "Throttles and 5xx errors are expected at scale and AWS SDKs retry them with backoff by default. " +
				"Restore the default retry strategy (3 attempts, or adaptive mode for throttling) unless the caller retries itself.",
			Evidence: map[string]any{"failed_calls": r.failedNoRetries},
		})
	}
	if r.failedExhausted >= retryMinFailures {
		out = append(out, Finding{
			Detector: DetectorMissingRetries, Severity: SeverityHigh,
			Title:  fmt.Sprintf("%d calls failed after every retry", r.failedExhausted),
			Detail: fmt.Sprintf("%d AWS SDK calls were still throttled or failing on their last attempt.", r.failedExhausted),
			Suggestion: "Retrying harder will not fix sustained throttling. Lower the request rate the backend sees: " +
				"buffer the work in a queue, raise the capacity or quota, or use the SDK's adaptive retry mode to slow down client-side.",
			Evidence: map[string]any{"failed_calls": r.failedExhausted},
		})
	}
	if r.failedOther >= retryMinUntracked {
		out = append(out, Finding{
			Detector: DetectorMissingRetries, Severity: SeverityLow,
			Title:  fmt.Sprintf("%d failed requests came from clients without SDK retry headers", r.failedOther),
			Detail: "These requests were throttled or failed with a server error, but carried no amz-sdk-invocation-id, so Stratus cannot tell whether they were retried.",
			Suggestion: "If these come from your application, check that it retries throttles and 5xx errors with backoff and jitter. " +
				"Requests a load generator sends to AWS directly can be ignored.",
			Evidence: map[string]any{"failed_requests": r.failedOther},
		})
	}
	if r.retryAttempts >= retryStormMinTotals && float64(r.retryAttempts) >= retryStormShare*float64(r.requests) {
		share := float64(r.retryAttempts) / float64(r.requests)
		out = append(out, Finding{
			Detector: DetectorRetryStorm, Severity: SeverityMedium,
			Title:  fmt.Sprintf("%s of requests were retries", pct(share)),
			Detail: fmt.Sprintf("%d of %d requests were second or later attempts of an AWS SDK call.", r.retryAttempts, r.requests),
			Suggestion: "Retries add load to a backend that is already struggling. Check that retries back off with jitter, " +
				"cap them at the SDK default, and fix the throttling at its source.",
			Evidence: map[string]any{"retries": r.retryAttempts, "requests": r.requests, "share": round(share)},
		})
	}
	return out
}
