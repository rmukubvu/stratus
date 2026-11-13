package s3

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
)

var lockNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func refused(err error) bool {
	apiErr, ok := err.(*apierror.Error)
	return ok && apiErr.Code == "AccessDenied" && apiErr.StatusCode == http.StatusForbidden
}

func TestCheckDelete(t *testing.T) {
	future, past := lockNow.Add(time.Hour), lockNow.Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		lock    objectLock
		bypass  bool
		refused bool
	}{
		{"unlocked", objectLock{}, false, false},
		{"legal hold", objectLock{LegalHold: true}, true, true},
		{"compliance active", objectLock{Mode: lockModeCompliance, RetainUntil: future}, true, true},
		{"compliance expired", objectLock{Mode: lockModeCompliance, RetainUntil: past}, false, false},
		{"governance active", objectLock{Mode: lockModeGovernance, RetainUntil: future}, false, true},
		{"governance bypassed", objectLock{Mode: lockModeGovernance, RetainUntil: future}, true, false},
		{"governance bypass does not lift a hold", objectLock{Mode: lockModeGovernance, RetainUntil: future, LegalHold: true}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.lock.checkDelete(lockNow, tc.bypass, "/b/k")
			if refused(err) != tc.refused || (!tc.refused && err != nil) {
				t.Fatalf("checkDelete = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}

func TestCheckRetentionChange(t *testing.T) {
	until := lockNow.Add(24 * time.Hour)
	later, earlier := until.Add(time.Hour), until.Add(-time.Hour)
	gov := objectLock{Mode: lockModeGovernance, RetainUntil: until}
	comp := objectLock{Mode: lockModeCompliance, RetainUntil: until}
	for _, tc := range []struct {
		name    string
		current objectLock
		next    retention
		bypass  bool
		refused bool
	}{
		{"compliance extended", comp, retention{lockModeCompliance, later}, false, false},
		{"compliance shortened", comp, retention{lockModeCompliance, earlier}, true, true},
		{"compliance to governance", comp, retention{lockModeGovernance, later}, true, true},
		{"compliance removed", comp, retention{}, true, true},
		{"governance extended", gov, retention{lockModeGovernance, later}, false, false},
		{"governance raised to compliance", gov, retention{lockModeCompliance, until}, false, false},
		{"governance shortened", gov, retention{lockModeGovernance, earlier}, false, true},
		{"governance shortened with bypass", gov, retention{lockModeGovernance, earlier}, true, false},
		{"governance removed with bypass", gov, retention{}, true, false},
		{"expired compliance changes freely", objectLock{Mode: lockModeCompliance, RetainUntil: lockNow.Add(-time.Second)}, retention{lockModeGovernance, later}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.current.checkRetentionChange(tc.next, lockNow, tc.bypass, "/b/k")
			if refused(err) != tc.refused || (!tc.refused && err != nil) {
				t.Fatalf("checkRetentionChange = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}

func TestParseLockConfig(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		code      string
		days      int
		years     int
	}{
		{"enabled only", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`, "", 0, 0},
		{"days", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>7</Days></DefaultRetention></Rule></ObjectLockConfiguration>`, "", 7, 0},
		{"years", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`, "", 0, 1},
		{"not enabled", `<ObjectLockConfiguration></ObjectLockConfiguration>`, "MalformedXML", 0, 0},
		{"days and years", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`, "MalformedXML", 0, 0},
		{"bad mode", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>STRICT</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`, "MalformedXML", 0, 0},
		{"zero days", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>0</Days></DefaultRetention></Rule></ObjectLockConfiguration>`, "InvalidArgument", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseLockConfig([]byte(tc.doc), "/b")
			if tc.code != "" {
				if apiErr, ok := err.(*apierror.Error); !ok || apiErr.Code != tc.code {
					t.Fatalf("want %s, got %v", tc.code, err)
				}
				return
			}
			if err != nil || !cfg.Enabled || cfg.DefaultDays != tc.days || cfg.DefaultYears != tc.years {
				t.Fatalf("parseLockConfig = %+v, %v", cfg, err)
			}
		})
	}
}

func TestLockFromHeaders(t *testing.T) {
	enabled := bucketLockConfig{Enabled: true, DefaultMode: lockModeGovernance, DefaultDays: 2}
	future := lockNow.Add(48 * time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name    string
		cfg     bucketLockConfig
		headers map[string]string
		want    objectLock
		code    string
	}{
		{"no lock bucket, no headers", bucketLockConfig{}, nil, objectLock{}, ""},
		{"default retention", enabled, nil, objectLock{Mode: lockModeGovernance, RetainUntil: lockNow.AddDate(0, 0, 2)}, ""},
		{"explicit retention", enabled, map[string]string{headerLockMode: lockModeCompliance, headerLockRetainUntil: future}, objectLock{Mode: lockModeCompliance, RetainUntil: lockNow.Add(48 * time.Hour)}, ""},
		{"legal hold keeps default retention", enabled, map[string]string{headerLockLegalHold: "ON"}, objectLock{Mode: lockModeGovernance, RetainUntil: lockNow.AddDate(0, 0, 2), LegalHold: true}, ""},
		{"headers on a bucket without lock", bucketLockConfig{}, map[string]string{headerLockLegalHold: "ON"}, objectLock{}, "InvalidRequest"},
		{"mode without date", enabled, map[string]string{headerLockMode: lockModeCompliance}, objectLock{}, "InvalidArgument"},
		{"date in the past", enabled, map[string]string{headerLockMode: lockModeCompliance, headerLockRetainUntil: lockNow.Add(-time.Hour).Format(time.RFC3339)}, objectLock{}, "InvalidArgument"},
		{"bad legal hold", enabled, map[string]string{headerLockLegalHold: "MAYBE"}, objectLock{}, "InvalidArgument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/b/k", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got, err := lockFromHeaders(r, tc.cfg, lockNow, "/b/k")
			if tc.code != "" {
				if apiErr, ok := err.(*apierror.Error); !ok || apiErr.Code != tc.code {
					t.Fatalf("want %s, got %v", tc.code, err)
				}
				return
			}
			if err != nil || got.Mode != tc.want.Mode || !got.RetainUntil.Equal(tc.want.RetainUntil) || got.LegalHold != tc.want.LegalHold {
				t.Fatalf("lockFromHeaders = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}
