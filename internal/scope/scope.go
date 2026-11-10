// Package scope models the AWS account and region a request acts in.
//
// Every resource in AWS lives in one account and, for regional services,
// one region. Stratus keeps a separate set of resources per scope, so tests
// can model landing zones, cross-account roles and multi-region setups.
package scope

import (
	"context"
	"regexp"
)

// Defaults used when a request names no account or region.
const (
	DefaultAccount = "000000000000"
	DefaultRegion  = "us-east-1"
)

// Scope is an AWS account and region.
type Scope struct {
	Account string
	Region  string
}

// Default is the scope Stratus used before it knew about accounts.
func Default() Scope {
	return Scope{Account: DefaultAccount, Region: DefaultRegion}
}

// IsDefault reports whether s is the default scope, whose data keeps the
// layout Stratus has always used.
func (s Scope) IsDefault() bool {
	return s == Default()
}

// AccountOnly drops the region, for global services such as IAM and S3.
func (s Scope) AccountOnly() Scope {
	return Scope{Account: s.Account, Region: DefaultRegion}
}

// ARN builds a regional ARN, arn:aws:<service>:<region>:<account>:<resource>.
func (s Scope) ARN(service, resource string) string {
	return "arn:aws:" + service + ":" + s.Region + ":" + s.Account + ":" + resource
}

// GlobalARN builds an ARN for a global service, which has no region.
func (s Scope) GlobalARN(service, resource string) string {
	return "arn:aws:" + service + "::" + s.Account + ":" + resource
}

// String is "<account>/<region>".
func (s Scope) String() string {
	return s.Account + "/" + s.Region
}

var (
	accountRe = regexp.MustCompile(`^\d{12}$`)
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)
)

// ValidAccount reports whether id is a twelve-digit account ID.
func ValidAccount(id string) bool { return accountRe.MatchString(id) }

// ValidRegion reports whether name looks like an AWS region name.
func ValidRegion(name string) bool { return regionRe.MatchString(name) }

type contextKey struct{}

// WithScope returns a context carrying s.
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// FromContext returns the scope in ctx, or the default scope.
func FromContext(ctx context.Context) Scope {
	if s, ok := ctx.Value(contextKey{}).(Scope); ok {
		return s
	}
	return Default()
}

// Prefix is what a scope's metadata buckets and blob directories are
// prefixed with: nothing for the default scope, so existing data directories
// and snapshots keep working, and "@<account>/<region>/" otherwise.
func Prefix(s Scope) string {
	if s.IsDefault() {
		return ""
	}
	return "@" + s.Account + "/" + s.Region + "/"
}

// SplitPrefixed splits a prefixed bucket or directory name into its scope
// and the unprefixed name.
func SplitPrefixed(name string) (Scope, string) {
	if len(name) < 2 || name[0] != '@' {
		return Default(), name
	}
	rest := name[1:]
	account, rest, ok := cut(rest, '/')
	if !ok {
		return Default(), name
	}
	region, base, ok := cut(rest, '/')
	if !ok || !ValidAccount(account) {
		return Default(), name
	}
	return Scope{Account: account, Region: region}, base
}

func cut(s string, sep byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
