package authn

import (
	"fmt"
	"strings"

	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/services/iam"
	"github.com/stratus/internal/services/sts"
	"github.com/stratus/internal/sigv4"
)

// DefaultRootSecret is the secret of root credentials unless
// STRATUS_SIGV4_ROOT_SECRET says otherwise: the "test" every local AWS tool
// defaults to.
const DefaultRootSecret = "test"

// DefaultRootAccessKey is the placeholder access key local tools use. Like
// a twelve-digit account ID, it signs as the root user.
const DefaultRootAccessKey = "test"

// SessionStore finds STS session credentials by access key.
type SessionStore interface {
	Session(accessKeyID string) (sts.Session, bool)
}

// UserKeyStore finds IAM user access keys.
type UserKeyStore interface {
	Lookup(accessKeyID string) (iam.AccessKey, bool)
}

// StaticCredential is an extra access key configured with
// STRATUS_SIGV4_CREDENTIALS, for code that signs with fixed fake keys.
type StaticCredential struct {
	AccessKeyID     string
	SecretAccessKey string
	// Account the key acts in; empty means the default account.
	Account string
}

// Directory is every credential Stratus can verify a signature with:
// STS sessions, IAM user keys, configured static keys, and root keys (the
// "test" key and any twelve-digit account ID) with the root secret.
type Directory struct {
	Sessions   SessionStore
	Keys       UserKeyStore
	Static     StaticCredentials
	RootSecret string
}

var _ sigv4.CredentialStore = Directory{}

// Lookup implements sigv4.CredentialStore.
func (d Directory) Lookup(accessKeyID string) (sigv4.Credential, bool) {
	if d.Sessions != nil {
		if s, ok := d.Sessions.Session(accessKeyID); ok {
			return sigv4.Credential{SecretAccessKey: s.SecretAccessKey, SessionToken: s.SessionToken, Expires: s.Expiration}, true
		}
	}
	if d.Keys != nil {
		if k, ok := d.Keys.Lookup(accessKeyID); ok {
			return sigv4.Credential{SecretAccessKey: k.SecretAccessKey, Inactive: k.Status == "Inactive"}, true
		}
	}
	if c, ok := d.Static[accessKeyID]; ok {
		return sigv4.Credential{SecretAccessKey: c.SecretAccessKey, IgnoreToken: true}, true
	}
	if accessKeyID == DefaultRootAccessKey || scope.ValidAccount(accessKeyID) {
		secret := d.RootSecret
		if secret == "" {
			secret = DefaultRootSecret
		}
		return sigv4.Credential{SecretAccessKey: secret, IgnoreToken: true}, true
	}
	return sigv4.Credential{}, false
}

// StaticCredentials indexes configured keys. It is also an account
// resolver, so a static key with an account routes to that account.
type StaticCredentials map[string]StaticCredential

// AccountFor implements httpapi.AccountResolver.
func (s StaticCredentials) AccountFor(accessKeyID string) (string, bool) {
	c, ok := s[accessKeyID]
	if !ok || c.Account == "" {
		return "", false
	}
	return c.Account, true
}

// ParseStaticCredentials reads STRATUS_SIGV4_CREDENTIALS: a comma-separated
// list of ACCESS_KEY:SECRET or ACCESS_KEY:SECRET:ACCOUNT entries.
func ParseStaticCredentials(raw string) (StaticCredentials, error) {
	out := StaticCredentials{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("STRATUS_SIGV4_CREDENTIALS entries must be ACCESS_KEY:SECRET or ACCESS_KEY:SECRET:ACCOUNT, got %q", redact(entry))
		}
		c := StaticCredential{AccessKeyID: parts[0], SecretAccessKey: parts[1]}
		if len(parts) == 3 {
			if !scope.ValidAccount(parts[2]) {
				return nil, fmt.Errorf("STRATUS_SIGV4_CREDENTIALS: account for %s must be twelve digits, got %q", parts[0], parts[2])
			}
			c.Account = parts[2]
		}
		out[c.AccessKeyID] = c
	}
	return out, nil
}

// redact hides the secret in an entry quoted in an error.
func redact(entry string) string {
	key, _, found := strings.Cut(entry, ":")
	if !found {
		return entry
	}
	return key + ":***"
}
