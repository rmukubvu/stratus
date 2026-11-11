package sts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"regexp"
	"strings"
	"time"

	"github.com/stratus/internal/scope"
)

// Session is a set of temporary credentials STS issued for a role.
type Session struct {
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SessionToken    string    `json:"session_token"`
	Account         string    `json:"account"`
	RoleARN         string    `json:"role_arn"`
	RoleName        string    `json:"role_name"`
	RoleID          string    `json:"role_id"`
	SessionName     string    `json:"session_name"`
	Expiration      time.Time `json:"expiration"`
}

// AssumedRoleARN is the ARN a caller using this session has, as
// GetCallerIdentity reports it.
func (s Session) AssumedRoleARN() string {
	return "arn:aws:sts::" + s.Account + ":assumed-role/" + s.RoleName + "/" + s.SessionName
}

// AssumedRoleID is the "role-id:session-name" identifier AWS reports.
func (s Session) AssumedRoleID() string {
	return s.RoleID + ":" + s.SessionName
}

// RoleRef is a parsed IAM role ARN.
type RoleRef struct {
	ARN     string
	Account string
	Name    string
}

var (
	roleARNPattern     = regexp.MustCompile(`^arn:aws:iam::(\d{12}):role/(?:[\x21-\x7e]*/)?([\w+=,.@-]{1,64})$`)
	sessionNamePattern = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)
)

// ParseRoleARN validates an IAM role ARN and splits out its account and name.
func ParseRoleARN(arn string) (RoleRef, bool) {
	m := roleARNPattern.FindStringSubmatch(arn)
	if m == nil || !scope.ValidAccount(m[1]) {
		return RoleRef{}, false
	}
	return RoleRef{ARN: arn, Account: m[1], Name: m[2]}, true
}

// ValidSessionName reports whether name is an acceptable RoleSessionName.
func ValidSessionName(name string) bool {
	return sessionNamePattern.MatchString(name)
}

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newAccessKeyID returns an "ASIA…" key, the prefix AWS uses for temporary
// credentials, so it never looks like a twelve-digit account.
func newAccessKeyID() string {
	return "ASIA" + randomBase32(16)
}

// roleID derives the stable "AROA…" identifier of a role from its ARN, the
// way a real role keeps one ID for its lifetime.
func roleID(roleARN string) string {
	sum := sha256.Sum256([]byte(roleARN))
	return "AROA" + keyEncoding.EncodeToString(sum[:])[:17]
}

func newSecret() string {
	return randomBase64(30)
}

func newSessionToken() string {
	return "FwoGZXIvYXdzE" + randomBase64(120)
}

func randomBase32(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return keyEncoding.EncodeToString(buf)[:n]
}

func randomBase64(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return strings.TrimRight(base64.StdEncoding.EncodeToString(buf), "=")[:n]
}
