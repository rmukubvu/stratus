package httpapi

import (
	"net/http"

	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/scope"
)

// AccountResolver maps an access key ID to the account it belongs to, for
// keys Stratus issued itself, such as STS session credentials.
type AccountResolver interface {
	AccountFor(accessKeyID string) (string, bool)
}

// ResolveScope works out the account and region a request acts in.
//
// The region is the one the request was signed for. The account is the
// access key itself when that is a twelve-digit account ID (the convention
// local AWS emulators share), the account of a key Stratus issued, or the
// default account. Requests that are not signed act in the default scope.
func ResolveScope(r *http.Request, sig *awscompat.SigV4Identity, accounts AccountResolver) scope.Scope {
	if sig == nil {
		sig = awscompat.ParseSigV4Credential(r.URL.Query().Get("X-Amz-Credential"))
	}
	sc := scope.Default()
	if sig == nil {
		return sc
	}
	if scope.ValidRegion(sig.Region) {
		sc.Region = sig.Region
	}
	switch {
	case scope.ValidAccount(sig.AccessKeyID):
		sc.Account = sig.AccessKeyID
	case accounts != nil:
		if account, ok := accounts.AccountFor(sig.AccessKeyID); ok {
			sc.Account = account
		}
	}
	return sc
}
