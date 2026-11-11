package iam

import (
	"crypto/rand"
	"time"

	"github.com/stratus/internal/store"
)

const accessKeysBucket = "iam-access-keys"

// AccessKey is a long-term credential of an IAM user.
type AccessKey struct {
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	Account         string    `json:"account"`
	UserName        string    `json:"user_name"`
	UserARN         string    `json:"user_arn"`
	UserID          string    `json:"user_id"`
	Status          string    `json:"status"`
	CreateDate      time.Time `json:"create_date"`
}

// KeyDirectory indexes every user access key across all accounts, so a
// request signed with one can be routed to its account and attributed to
// its user. It is shared by the per-account IAM services.
type KeyDirectory struct {
	store store.Store
}

// NewKeyDirectory keeps the index in the unscoped metadata store.
func NewKeyDirectory(metadata store.Store) *KeyDirectory {
	return &KeyDirectory{store: metadata}
}

// Lookup returns the access key record for an access key ID.
func (d *KeyDirectory) Lookup(accessKeyID string) (AccessKey, bool) {
	if d == nil || accessKeyID == "" {
		return AccessKey{}, false
	}
	key, err := getRecord[AccessKey](d.store, accessKeysBucket, accessKeyID, "access key")
	if err != nil {
		return AccessKey{}, false
	}
	return key, true
}

// AccountFor returns the account of a user access key.
func (d *KeyDirectory) AccountFor(accessKeyID string) (string, bool) {
	key, ok := d.Lookup(accessKeyID)
	return key.Account, ok
}

// UserFor returns the account, ARN and unique ID of the user an access key
// belongs to.
func (d *KeyDirectory) UserFor(accessKeyID string) (account, userARN, userID string, ok bool) {
	key, found := d.Lookup(accessKeyID)
	if !found {
		return "", "", "", false
	}
	return key.Account, key.UserARN, key.UserID, true
}

func (d *KeyDirectory) put(key AccessKey) error {
	return putRecord(d.store, accessKeysBucket, key.AccessKeyID, key)
}

func (d *KeyDirectory) delete(accessKeyID string) error {
	if err := d.store.Delete(accessKeysBucket, accessKeyID); err != nil {
		return internal(err)
	}
	return nil
}

// forUser lists a user's keys in one account.
func (d *KeyDirectory) forUser(account, userName string) ([]AccessKey, error) {
	all, err := scanRecords[AccessKey](d.store, accessKeysBucket, "")
	if err != nil {
		return nil, err
	}
	var out []AccessKey
	for _, k := range all {
		if k.Account == account && k.UserName == userName {
			out = append(out, k)
		}
	}
	return sortedBy(out, func(k AccessKey) string { return k.AccessKeyID }), nil
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomAlnum(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	for i, b := range buf {
		buf[i] = alnum[int(b)%len(alnum)]
	}
	return string(buf)
}

const secretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func randomSecret() string {
	buf := make([]byte, 40)
	_, _ = rand.Read(buf)
	for i, b := range buf {
		buf[i] = secretAlphabet[int(b)%len(secretAlphabet)]
	}
	return string(buf)
}
