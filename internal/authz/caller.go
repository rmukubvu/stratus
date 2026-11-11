package authz

import (
	"time"

	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/services/iam"
	"github.com/stratus/internal/services/sts"
)

// CallerKind tells the kinds of caller apart.
type CallerKind int

const (
	// Root is an account's root user: a twelve-digit or unknown access
	// key. Like the real root user, it is never refused.
	Root CallerKind = iota
	// User is an IAM user signing with its access key.
	User
	// AssumedRole is a caller using STS session credentials.
	AssumedRole
)

// Caller is who sent a request.
type Caller struct {
	Kind    CallerKind
	Account string
	// ARN is the user ARN or the assumed-role session ARN.
	ARN string
	// Name is the user name or the role name.
	Name    string
	UserID  string
	RoleARN string
	// Active is false for a deactivated user key.
	Active bool
	// Expires is when session credentials expire; zero for other callers.
	Expires time.Time
}

// SessionStore finds STS sessions by access key.
type SessionStore interface {
	Session(accessKeyID string) (sts.Session, bool)
}

// UserKeyStore finds IAM user access keys.
type UserKeyStore interface {
	Lookup(accessKeyID string) (iam.AccessKey, bool)
}

// Callers resolves access keys to callers.
type Callers struct {
	Sessions SessionStore
	Keys     UserKeyStore
}

// Resolve identifies the caller of a request signed with accessKeyID that
// acts in sc.
func (c Callers) Resolve(accessKeyID string, sc scope.Scope) Caller {
	if c.Sessions != nil {
		if s, ok := c.Sessions.Session(accessKeyID); ok {
			return Caller{
				Kind:    AssumedRole,
				Account: s.Account,
				ARN:     s.AssumedRoleARN(),
				Name:    s.RoleName,
				UserID:  s.AssumedRoleID(),
				RoleARN: s.RoleARN,
				Active:  true,
				Expires: s.Expiration,
			}
		}
	}
	if c.Keys != nil {
		if k, ok := c.Keys.Lookup(accessKeyID); ok {
			return Caller{
				Kind:    User,
				Account: k.Account,
				ARN:     k.UserARN,
				Name:    k.UserName,
				UserID:  k.UserID,
				Active:  k.Status != "Inactive",
			}
		}
	}
	return Caller{Kind: Root, Account: sc.Account, ARN: "arn:aws:iam::" + sc.Account + ":root", Active: true}
}

// principalArn is the aws:PrincipalArn condition value: the role ARN for an
// assumed role, as in AWS.
func (c Caller) principalArn() string {
	if c.Kind == AssumedRole {
		return c.RoleARN
	}
	return c.ARN
}
