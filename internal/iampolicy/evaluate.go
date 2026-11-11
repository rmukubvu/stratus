package iampolicy

import "strings"

// Request is what is being authorised.
type Request struct {
	// Action is "service:Operation", e.g. "sqs:SendMessage".
	Action string
	// Resource is the ARN acted on. AnyResource means the action has no
	// resource Stratus can name, so statements match on action only.
	Resource string
	// Principal is the caller, for resource-based policies.
	Principal PrincipalInfo
	// Context holds the condition keys, e.g. "aws:SourceIp".
	Context map[string][]string
}

// AnyResource marks a request whose resource is unknown.
const AnyResource = ""

// PrincipalInfo identifies the caller for Principal matching.
type PrincipalInfo struct {
	// ARN is the caller's ARN; for an assumed role, both the session ARN
	// and the role ARN match.
	ARN     string
	RoleARN string
	Account string
	// Service is set when an AWS service is the caller, e.g.
	// "lambda.amazonaws.com".
	Service string
}

// Outcome is the result of an evaluation.
type Outcome int

const (
	// ImplicitDeny means no statement allowed the request.
	ImplicitDeny Outcome = iota
	// Allowed means a statement allowed it and none denied it.
	Allowed
	// ExplicitDeny means a Deny statement matched.
	ExplicitDeny
)

func (o Outcome) String() string {
	switch o {
	case Allowed:
		return "Allow"
	case ExplicitDeny:
		return "ExplicitDeny"
	default:
		return "ImplicitDeny"
	}
}

// Decision explains an evaluation.
type Decision struct {
	Outcome Outcome
	// Statement is the Sid of the deciding statement, when it has one.
	Statement string
}

// Evaluate applies policies to a request. checkPrincipal is true for
// resource-based policies, whose statements name who they apply to.
func Evaluate(docs []Document, req Request, checkPrincipal bool) Decision {
	decision := Decision{Outcome: ImplicitDeny}
	for _, doc := range docs {
		for _, st := range doc.Statement {
			if !st.applies(req, checkPrincipal) {
				continue
			}
			if st.Effect == Deny {
				return Decision{Outcome: ExplicitDeny, Statement: st.Sid}
			}
			if decision.Outcome != Allowed {
				decision = Decision{Outcome: Allowed, Statement: st.Sid}
			}
		}
	}
	return decision
}

func (st Statement) applies(req Request, checkPrincipal bool) bool {
	if !st.matchesAction(req.Action) || !st.matchesResource(req.Resource, req.Context) {
		return false
	}
	if checkPrincipal && !st.matchesPrincipal(req.Principal) {
		return false
	}
	return st.Condition.Satisfied(req.Context)
}

func (st Statement) matchesAction(action string) bool {
	if len(st.NotAction) > 0 {
		return !anyMatch(st.NotAction, func(p string) bool { return matchAction(p, action) })
	}
	return anyMatch(st.Action, func(p string) bool { return matchAction(p, action) })
}

func (st Statement) matchesResource(resource string, ctx map[string][]string) bool {
	if resource == AnyResource {
		// Stratus cannot name the resource, so the statement is judged on
		// its action and conditions alone.
		return true
	}
	if len(st.NotResource) > 0 {
		return !anyMatch(st.NotResource, func(p string) bool { return matchResource(p, resource, ctx) })
	}
	if len(st.Resource) == 0 {
		// Trust and resource policies may omit Resource: it is the
		// resource the policy is attached to.
		return true
	}
	return anyMatch(st.Resource, func(p string) bool { return matchResource(p, resource, ctx) })
}

func (st Statement) matchesPrincipal(p PrincipalInfo) bool {
	if st.NotPrincipal != nil {
		return !st.NotPrincipal.matches(p)
	}
	return st.Principal != nil && st.Principal.matches(p)
}

func (pr *Principal) matches(p PrincipalInfo) bool {
	if pr.Wildcard {
		return true
	}
	for _, s := range pr.Service {
		if p.Service != "" && strings.EqualFold(s, p.Service) {
			return true
		}
	}
	for _, a := range pr.AWS {
		switch {
		case a == p.Account && p.Account != "":
			return true
		case a == "arn:aws:iam::"+p.Account+":root" && p.Account != "":
			return true
		case p.ARN != "" && wildcardMatch(a, p.ARN):
			return true
		case p.RoleARN != "" && a == p.RoleARN:
			return true
		}
	}
	return false
}

func anyMatch(patterns []string, match func(string) bool) bool {
	for _, p := range patterns {
		if match(p) {
			return true
		}
	}
	return false
}
