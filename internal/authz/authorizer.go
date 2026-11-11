// Package authz enforces IAM policies on AWS API requests when Stratus runs
// with STRATUS_IAM=enforce (or reports what it would refuse with audit).
package authz

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/iampolicy"
)

// Mode selects what the authorizer does with a request policies refuse.
type Mode string

const (
	// Off evaluates nothing; every caller may do everything.
	Off Mode = "off"
	// Audit evaluates policies and logs refusals, but lets requests through.
	Audit Mode = "audit"
	// Enforce refuses requests policies do not allow.
	Enforce Mode = "enforce"
)

// ParseMode reads STRATUS_IAM.
func ParseMode(raw string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", Off:
		return Off, nil
	case Audit:
		return Audit, nil
	case Enforce:
		return Enforce, nil
	}
	return Off, fmt.Errorf("STRATUS_IAM must be off, audit or enforce, got %q", raw)
}

// PolicySource returns the identity-based policies of IAM principals.
type PolicySource interface {
	UserPolicies(account, userName string) ([]string, error)
	RolePolicies(account, roleName string) ([]string, error)
}

// Authorizer evaluates each request against its caller's policies.
type Authorizer struct {
	mode     Mode
	callers  Callers
	policies PolicySource
	logger   *slog.Logger
	now      func() time.Time
}

// New returns an authorizer in the given mode.
func New(mode Mode, callers Callers, policies PolicySource, logger *slog.Logger) *Authorizer {
	return &Authorizer{mode: mode, callers: callers, policies: policies, logger: logger, now: time.Now}
}

var _ httpapi.Authorizer = (*Authorizer)(nil)

// Authorize implements httpapi.Authorizer.
func (a *Authorizer) Authorize(r *http.Request, md httpapi.RequestMetadata) error {
	service := md.Classification.Service
	if a.mode == Off || service == "" || md.Classification.Protocol == httpapi.ProtocolInternal {
		return nil
	}
	accessKey := ""
	if md.SigV4 != nil {
		accessKey = md.SigV4.AccessKeyID
	}
	caller := a.callers.Resolve(accessKey, md.Scope)
	if caller.Kind == Root {
		return nil
	}
	if err := a.checkCredentials(caller); err != nil {
		return a.refuse(err, caller, "", "")
	}

	action := Action(service, md.Classification.Operation, r, md.Classification.Key)
	if isUnauthenticated(action) {
		return nil
	}
	p := readParams(r, md.Classification.Protocol == httpapi.ProtocolJSON || md.Classification.Protocol == httpapi.ProtocolREST)
	resource := Resource(service, md.Classification.Operation, r, md.Scope, md.Classification.Bucket, md.Classification.Key, p)

	docs, err := a.identityPolicies(caller)
	if err != nil {
		return err
	}
	req := iampolicy.Request{
		Action:    action,
		Resource:  resource,
		Principal: iampolicy.PrincipalInfo{ARN: caller.ARN, RoleARN: caller.RoleARN, Account: caller.Account},
		Context:   a.context(r, md, caller),
	}
	decision := iampolicy.Evaluate(docs, req, false)
	if decision.Outcome == iampolicy.Allowed {
		return nil
	}
	return a.refuse(accessDenied(md.Classification.Protocol, caller, action, resource, decision), caller, action, resource)
}

func (a *Authorizer) checkCredentials(c Caller) error {
	if !c.Active {
		return &apierror.Error{StatusCode: http.StatusForbidden, Code: "InvalidClientTokenId", Message: "The security token included in the request is invalid: the access key is inactive."}
	}
	if !c.Expires.IsZero() && a.now().After(c.Expires) {
		return &apierror.Error{StatusCode: http.StatusForbidden, Code: "ExpiredToken", Message: "The security token included in the request is expired"}
	}
	return nil
}

// refuse returns the error in enforce mode and logs it in both modes.
func (a *Authorizer) refuse(err error, caller Caller, action, resource string) error {
	if a.logger != nil {
		a.logger.Warn("iam refused request",
			"mode", string(a.mode), "principal", caller.ARN, "action", action, "resource", resource, "error", err.Error())
	}
	if a.mode == Audit {
		return nil
	}
	return err
}

func (a *Authorizer) identityPolicies(c Caller) ([]iampolicy.Document, error) {
	var raw []string
	var err error
	switch c.Kind {
	case User:
		raw, err = a.policies.UserPolicies(c.Account, c.Name)
	case AssumedRole:
		raw, err = a.policies.RolePolicies(c.Account, c.Name)
	}
	if err != nil {
		return nil, err
	}
	docs := make([]iampolicy.Document, 0, len(raw))
	for _, r := range raw {
		// IAM validated documents when they were stored; one that no longer
		// parses grants nothing.
		if doc, err := iampolicy.Parse(r); err == nil {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

// context builds the global condition keys of a request.
func (a *Authorizer) context(r *http.Request, md httpapi.RequestMetadata, c Caller) map[string][]string {
	now := a.now().UTC()
	ctx := map[string][]string{
		"aws:CurrentTime":      {now.Format(time.RFC3339)},
		"aws:EpochTime":        {strconv.FormatInt(now.Unix(), 10)},
		"aws:RequestedRegion":  {md.Scope.Region},
		"aws:SecureTransport":  {strconv.FormatBool(r.TLS != nil)},
		"aws:PrincipalAccount": {c.Account},
		"aws:PrincipalArn":     {c.principalArn()},
		"aws:userid":           {c.UserID},
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ctx["aws:SourceIp"] = []string{host}
	}
	switch c.Kind {
	case User:
		ctx["aws:PrincipalType"] = []string{"User"}
		ctx["aws:username"] = []string{c.Name}
	case AssumedRole:
		ctx["aws:PrincipalType"] = []string{"AssumedRole"}
	}
	return ctx
}

func accessDenied(protocol httpapi.Protocol, c Caller, action, resource string, d iampolicy.Decision) error {
	code := "AccessDenied"
	if protocol == httpapi.ProtocolJSON || protocol == httpapi.ProtocolREST {
		code = "AccessDeniedException"
	}
	if resource == iampolicy.AnyResource {
		resource = "*"
	}
	reason := "because no identity-based policy allows the " + action + " action"
	if d.Outcome == iampolicy.ExplicitDeny {
		reason = "with an explicit deny in an identity-based policy"
	}
	return &apierror.Error{
		StatusCode: http.StatusForbidden,
		Code:       code,
		Message:    fmt.Sprintf("User: %s is not authorized to perform: %s on resource: %s %s", c.ARN, action, resource, reason),
	}
}
