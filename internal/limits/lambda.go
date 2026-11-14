package limits

import "math"

// Lambda throttling reasons, as Lambda names them in a 429's Reason.
const (
	ReasonReservedConcurrency = "ReservedFunctionConcurrentInvocationLimitExceeded"
	ReasonAccountConcurrency  = "ConcurrentInvocationLimitExceeded"
)

// Rule IDs of the Lambda concurrency quotas.
const (
	RuleLambdaAccountConcurrency  = "lambda-account-concurrency"
	RuleLambdaReservedConcurrency = "lambda-reserved-concurrency"
)

// defaultAccountConcurrency is Lambda's default concurrent executions per
// account and region.
const defaultAccountConcurrency = 1000

// LambdaSpec tunes Lambda concurrency. Every field is optional.
type LambdaSpec struct {
	// Disabled turns concurrency enforcement off while other limits apply.
	Disabled bool `json:"disabled,omitempty"`
	// AccountConcurrency is the concurrent executions an account has in a
	// region, before scaling: 1000 by default in AWS.
	AccountConcurrency float64 `json:"account_concurrency,omitempty"`
}

func (l LambdaSpec) normalise() (LambdaSpec, error) {
	if l.AccountConcurrency < 0 {
		return LambdaSpec{}, invalid("InvalidLimitsProfile", "lambda.account_concurrency must not be negative")
	}
	l.AccountConcurrency = orDefault(l.AccountConcurrency, defaultAccountConcurrency)
	return l, nil
}

// resolveLambda builds the concurrency model: on by default for the AWS
// bases, off for none unless the spec has a lambda section.
func resolveLambda(base string, spec *LambdaSpec) (*LambdaSpec, error) {
	var l LambdaSpec
	switch {
	case spec != nil:
		l = *spec
	case base == BaseNone:
		return nil, nil
	}
	if l.Disabled {
		return nil, nil
	}
	n, err := l.normalise()
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// FunctionRequest is one invocation of a Lambda function.
type FunctionRequest struct {
	Request
	// Reserved is the function's reserved concurrency; nil when it draws
	// on the account's unreserved pool.
	Reserved *int
	// ReservedTotal is the concurrency every function in the account and
	// region has reserved, which the unreserved pool does not get.
	ReservedTotal int
}

// AcquireFunction takes one concurrent execution for an invocation: from
// the function's reserved concurrency when it has some, otherwise from the
// account's unreserved pool. It returns a release function, or a breach
// when no execution is free. Reserved concurrency is what the function's
// owner chose, so it is never scaled; the account pool is.
func (g *Governor) AcquireFunction(req FunctionRequest) (release func(), breach *Breach) {
	st := g.active.Load()
	if st == nil || st.profile.Lambda == nil {
		return func() {}, nil
	}
	key := QuotaKey{Account: req.Account, Region: req.Region}
	var max int
	var reason string
	if req.Reserved != nil {
		key.RuleID, key.Resource = RuleLambdaReservedConcurrency, req.Resource
		max, reason = *req.Reserved, ReasonReservedConcurrency
	} else {
		key.RuleID = RuleLambdaAccountConcurrency
		pool := math.Floor(st.profile.Lambda.AccountConcurrency*st.profile.Scale) - float64(req.ReservedTotal)
		max, reason = int(math.Max(1, pool)), ReasonAccountConcurrency
	}
	return g.acquire(key, max, req.Request, reason)
}
