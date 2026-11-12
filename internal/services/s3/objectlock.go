package s3

import (
	"encoding/xml"
	"net/http"
	"strings"
	"time"
)

// Object Lock protects object versions in a bucket created with Object Lock
// (which turns versioning on). The rules mirror S3:
//
//   - A version under a legal hold cannot be deleted until the hold is OFF.
//   - A version under COMPLIANCE retention cannot be deleted, and its
//     retention cannot be shortened or removed, by anyone, until it expires.
//   - A version under GOVERNANCE retention behaves the same unless the request
//     sends x-amz-bypass-governance-retention: true (which, under
//     STRATUS_IAM=enforce, also needs s3:BypassGovernanceRetention).
//   - Overwriting or deleting a key without a version ID is always allowed: it
//     adds a new version or a delete marker and the locked version survives.
const (
	lockModeGovernance = "GOVERNANCE"
	lockModeCompliance = "COMPLIANCE"

	legalHoldOn  = "ON"
	legalHoldOff = "OFF"

	headerLockMode         = "X-Amz-Object-Lock-Mode"
	headerLockRetainUntil  = "X-Amz-Object-Lock-Retain-Until-Date"
	headerLockLegalHold    = "X-Amz-Object-Lock-Legal-Hold"
	headerBypassGovernance = "X-Amz-Bypass-Governance-Retention"
	headerBucketLock       = "X-Amz-Bucket-Object-Lock-Enabled"
)

// objectLock is the lock state of one object version.
type objectLock struct {
	Mode        string    `json:"mode,omitempty"`
	RetainUntil time.Time `json:"retain_until,omitempty"`
	LegalHold   bool      `json:"legal_hold,omitempty"`
}

// retention is a requested retention setting; an empty Mode removes it.
type retention struct {
	Mode        string
	RetainUntil time.Time
}

func (l objectLock) retentionActive(now time.Time) bool {
	return l.Mode != "" && now.Before(l.RetainUntil)
}

// checkDelete refuses to delete a version the lock still protects.
func (l objectLock) checkDelete(now time.Time, bypassGovernance bool, resource string) error {
	if l.LegalHold {
		return lockedObject(resource)
	}
	if !l.retentionActive(now) {
		return nil
	}
	if l.Mode == lockModeGovernance && bypassGovernance {
		return nil
	}
	return lockedObject(resource)
}

// checkRetentionChange decides whether next may replace the current
// retention. Expired retention can be changed freely. Active retention can
// always be extended in the same mode, and governance can be raised to
// compliance; shortening, removing or weakening governance retention needs
// the bypass header, and active compliance retention never weakens.
func (l objectLock) checkRetentionChange(next retention, now time.Time, bypassGovernance bool, resource string) error {
	if !l.retentionActive(now) {
		return nil
	}
	notShorter := next.Mode != "" && !next.RetainUntil.Before(l.RetainUntil)
	switch {
	case notShorter && next.Mode == l.Mode:
		return nil
	case notShorter && l.Mode == lockModeGovernance && next.Mode == lockModeCompliance:
		return nil
	case l.Mode == lockModeGovernance && bypassGovernance:
		return nil
	}
	return lockedObject(resource)
}

// validateRetention checks a requested retention the way S3 does: a known
// mode and a retain-until date in the future.
func validateRetention(r retention, now time.Time, resource string) error {
	if r.Mode == "" {
		return nil
	}
	if r.Mode != lockModeGovernance && r.Mode != lockModeCompliance {
		return malformedXML(resource)
	}
	if !r.RetainUntil.After(now) {
		return apiError(http.StatusBadRequest, "InvalidArgument", "The retain until date must be in the future!", resource)
	}
	return nil
}

// bucketLockConfig is a parsed ObjectLockConfiguration document.
type bucketLockConfig struct {
	Enabled bool
	// DefaultMode and the default period apply to new versions that name no
	// retention of their own.
	DefaultMode  string
	DefaultDays  int
	DefaultYears int
}

type objectLockConfigurationXML struct {
	XMLName           xml.Name `xml:"ObjectLockConfiguration"`
	ObjectLockEnabled string   `xml:"ObjectLockEnabled"`
	Rule              *struct {
		DefaultRetention struct {
			Mode  string `xml:"Mode"`
			Days  *int   `xml:"Days"`
			Years *int   `xml:"Years"`
		} `xml:"DefaultRetention"`
	} `xml:"Rule"`
}

const objectLockEnabledDoc = `<ObjectLockConfiguration xmlns="` + xmlNamespace + `"><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`

// parseLockConfig validates an ObjectLockConfiguration document.
func parseLockConfig(body []byte, resource string) (bucketLockConfig, error) {
	var doc objectLockConfigurationXML
	if err := xml.Unmarshal(body, &doc); err != nil || doc.ObjectLockEnabled != "Enabled" {
		return bucketLockConfig{}, malformedXML(resource)
	}
	cfg := bucketLockConfig{Enabled: true}
	if doc.Rule == nil {
		return cfg, nil
	}
	rule := doc.Rule.DefaultRetention
	if rule.Mode != lockModeGovernance && rule.Mode != lockModeCompliance {
		return bucketLockConfig{}, malformedXML(resource)
	}
	if (rule.Days == nil) == (rule.Years == nil) {
		return bucketLockConfig{}, malformedXML(resource)
	}
	cfg.DefaultMode = rule.Mode
	if rule.Days != nil {
		cfg.DefaultDays = *rule.Days
	} else {
		cfg.DefaultYears = *rule.Years
	}
	if cfg.DefaultDays < 0 || cfg.DefaultYears < 0 || cfg.DefaultDays+cfg.DefaultYears == 0 {
		return bucketLockConfig{}, apiError(http.StatusBadRequest, "InvalidArgument", "Default retention period must be a positive integer value", resource)
	}
	return cfg, nil
}

// defaultRetention is the retention a new version gets when the request
// names none.
func (c bucketLockConfig) defaultRetention(now time.Time) retention {
	if c.DefaultMode == "" {
		return retention{}
	}
	return retention{Mode: c.DefaultMode, RetainUntil: now.AddDate(c.DefaultYears, 0, c.DefaultDays)}
}

// lockFromHeaders reads the x-amz-object-lock-* headers of PutObject,
// CopyObject and CreateMultipartUpload, falling back to the bucket's default
// retention. Lock headers on a bucket without Object Lock are refused.
func lockFromHeaders(r *http.Request, cfg bucketLockConfig, now time.Time, resource string) (objectLock, error) {
	mode := r.Header.Get(headerLockMode)
	until := r.Header.Get(headerLockRetainUntil)
	hold := r.Header.Get(headerLockLegalHold)
	if mode == "" && until == "" && hold == "" {
		if !cfg.Enabled {
			return objectLock{}, nil
		}
		def := cfg.defaultRetention(now)
		return objectLock{Mode: def.Mode, RetainUntil: def.RetainUntil}, nil
	}
	if !cfg.Enabled {
		return objectLock{}, missingLockConfiguration(resource)
	}
	lock := objectLock{}
	if hold != "" {
		if hold != legalHoldOn && hold != legalHoldOff {
			return objectLock{}, apiError(http.StatusBadRequest, "InvalidArgument", "Legal Hold must be either of 'ON' or 'OFF'", resource)
		}
		lock.LegalHold = hold == legalHoldOn
	}
	switch {
	case mode == "" && until == "":
		def := cfg.defaultRetention(now)
		lock.Mode, lock.RetainUntil = def.Mode, def.RetainUntil
		return lock, nil
	case mode == "" || until == "":
		return objectLock{}, apiError(http.StatusBadRequest, "InvalidArgument", "x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied", resource)
	}
	retainUntil, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return objectLock{}, apiError(http.StatusBadRequest, "InvalidArgument", "The retain until date is not a valid ISO 8601 date", resource)
	}
	requested := retention{Mode: mode, RetainUntil: retainUntil.UTC()}
	if mode != lockModeGovernance && mode != lockModeCompliance {
		return objectLock{}, apiError(http.StatusBadRequest, "InvalidArgument", "Unknown wormMode directive.", resource)
	}
	if err := validateRetention(requested, now, resource); err != nil {
		return objectLock{}, err
	}
	lock.Mode, lock.RetainUntil = requested.Mode, requested.RetainUntil
	return lock, nil
}

func bypassGovernance(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get(headerBypassGovernance), "true")
}

func setLockHeaders(w http.ResponseWriter, lock objectLock) {
	if lock.Mode != "" {
		w.Header().Set(headerLockMode, lock.Mode)
		w.Header().Set(headerLockRetainUntil, lock.RetainUntil.UTC().Format(time.RFC3339))
	}
	if lock.LegalHold {
		w.Header().Set(headerLockLegalHold, legalHoldOn)
	}
}

func lockedObject(resource string) error {
	return apiError(http.StatusForbidden, "AccessDenied", "Access Denied because object protected by object lock.", resource)
}

func missingLockConfiguration(resource string) error {
	return apiError(http.StatusBadRequest, "InvalidRequest", "Bucket is missing Object Lock Configuration", resource)
}
