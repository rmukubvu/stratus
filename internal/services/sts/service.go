package sts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
)

const (
	sessionsBucket = "sts-sessions"

	defaultDuration = time.Hour
	minDuration     = 15 * time.Minute
	maxDuration     = 12 * time.Hour
)

// Service is the Security Token Service. It is global: one instance serves
// every account, because the credentials it issues are how a caller moves
// between accounts.
type Service struct {
	sessions store.Store
	now      func() time.Time
}

// NewService returns an STS service that keeps issued sessions in metadata,
// so credentials survive restarts and snapshots.
func NewService(metadata store.Store) *Service {
	return &Service{sessions: metadata, now: time.Now}
}

// Caller identifies who sent a request.
type Caller struct {
	AccessKeyID string
	// Account is the account the request acts in.
	Account string
}

type GetCallerIdentityOutput struct {
	Account string
	ARN     string
	UserID  string
}

// GetCallerIdentity reports the caller: the assumed role for credentials STS
// issued, and a generic user of the caller's account otherwise.
func (s *Service) GetCallerIdentity(caller Caller) (GetCallerIdentityOutput, error) {
	session, ok, err := s.session(caller.AccessKeyID)
	if err != nil {
		return GetCallerIdentityOutput{}, err
	}
	if ok {
		return GetCallerIdentityOutput{Account: session.Account, ARN: session.AssumedRoleARN(), UserID: session.AssumedRoleID()}, nil
	}
	account := caller.Account
	if account == "" {
		account = scope.DefaultAccount
	}
	accessKey := caller.AccessKeyID
	if accessKey == "" {
		accessKey = "STRATUSLOCAL"
	}
	return GetCallerIdentityOutput{
		Account: account,
		ARN:     fmt.Sprintf("arn:aws:iam::%s:user/stratus", account),
		UserID:  accessKey + ":stratus",
	}, nil
}

type AssumeRoleInput struct {
	RoleARN         string
	RoleSessionName string
	// DurationSeconds is the session lifetime; zero means one hour.
	DurationSeconds int
}

// AssumeRole issues temporary credentials that act in the role's account.
func (s *Service) AssumeRole(input AssumeRoleInput) (Session, error) {
	role, ok := ParseRoleARN(input.RoleARN)
	if !ok {
		return Session{}, validation("RoleArn is not a valid IAM role ARN: " + input.RoleARN)
	}
	if !ValidSessionName(input.RoleSessionName) {
		return Session{}, validation("RoleSessionName must be 2 to 64 characters of [\\w+=,.@-]")
	}
	duration := defaultDuration
	if input.DurationSeconds != 0 {
		duration = time.Duration(input.DurationSeconds) * time.Second
	}
	if duration < minDuration || duration > maxDuration {
		return Session{}, validation("DurationSeconds must be between 900 and 43200")
	}

	session := Session{
		AccessKeyID:     newAccessKeyID(),
		SecretAccessKey: newSecret(),
		SessionToken:    newSessionToken(),
		Account:         role.Account,
		RoleARN:         role.ARN,
		RoleName:        role.Name,
		RoleID:          roleID(role.ARN),
		SessionName:     input.RoleSessionName,
		Expiration:      s.now().UTC().Add(duration).Truncate(time.Second),
	}
	raw, err := json.Marshal(session)
	if err != nil {
		return Session{}, internal(err)
	}
	if err := s.sessions.Put(sessionsBucket, session.AccessKeyID, raw); err != nil {
		return Session{}, internal(err)
	}
	return session, nil
}

// AccountFor returns the account a key STS issued acts in. It lets the
// request router send calls signed with session credentials to the role's
// account.
func (s *Service) AccountFor(accessKeyID string) (string, bool) {
	session, ok, err := s.session(accessKeyID)
	if err != nil || !ok {
		return "", false
	}
	return session.Account, true
}

// GetAccessKeyInfo returns the account an access key belongs to.
func (s *Service) GetAccessKeyInfo(accessKeyID string) (string, error) {
	if accessKeyID == "" {
		return "", validation("AccessKeyId is required")
	}
	if scope.ValidAccount(accessKeyID) {
		return accessKeyID, nil
	}
	if account, ok := s.AccountFor(accessKeyID); ok {
		return account, nil
	}
	return scope.DefaultAccount, nil
}

func (s *Service) session(accessKeyID string) (Session, bool, error) {
	if accessKeyID == "" || scope.ValidAccount(accessKeyID) {
		return Session{}, false, nil
	}
	raw, err := s.sessions.Get(sessionsBucket, accessKeyID)
	if err != nil {
		return Session{}, false, internal(err)
	}
	if raw == nil {
		return Session{}, false, nil
	}
	var session Session
	if err := json.Unmarshal(raw, &session); err != nil {
		return Session{}, false, internal(err)
	}
	return session, true, nil
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationError", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}
