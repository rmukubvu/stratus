package limits

import (
	"fmt"
	"math"
	"strings"

	"github.com/stratus/internal/control"
)

// Base profile names.
const (
	BaseAWSDefault = "aws-default"
	BaseStrict     = "strict"
	BaseNone       = "none"
)

// strictScale is how far the strict profile scales the default quotas down,
// so missing retries and backoff show up with very little load.
const strictScale = 0.01

// RateRule caps the request rate of some operations of one service.
type RateRule struct {
	// ID names the rule in breaches, errors and reports. A rule with the
	// ID of a base rule replaces it.
	ID string `json:"id"`
	// Service is the AWS service name, e.g. "ssm" or "s3".
	Service string `json:"service"`
	// Operations share the rule's bucket; empty means every operation.
	Operations []string `json:"operations,omitempty"`
	// PerSecond is the sustained rate, before scaling.
	PerSecond float64 `json:"per_second"`
	// BurstSeconds is how many seconds of PerSecond may arrive at once;
	// zero means 1.
	BurstSeconds float64 `json:"burst_seconds,omitempty"`
	// Per says what shares one bucket: "account" (default), "resource" or
	// "prefix" (S3 key prefixes).
	Per KeyScope `json:"per,omitempty"`
	// Disabled turns a base rule off.
	Disabled bool `json:"disabled,omitempty"`
}

// Spec is the caller's description of a limits profile.
type Spec struct {
	// Profile is the base: "aws-default" (when empty), "strict" or "none".
	Profile string `json:"profile,omitempty"`
	// Scale multiplies every rate; zero means 1. It applies on top of the
	// base's own scale, so strict with scale 0.5 runs at 0.005.
	Scale float64 `json:"scale,omitempty"`
	// Rates adds rules, or replaces base rules with the same ID.
	Rates []RateRule `json:"rates,omitempty"`
}

// Profile is a resolved, validated set of quotas.
type Profile struct {
	Name string `json:"profile"`
	// Scale is the effective scale, base and caller's combined.
	Scale float64 `json:"scale"`
	// Rates are the active rules, as declared (before scaling).
	Rates []RateRule `json:"rates"`
}

// EffectiveRate is a rule's rate per second after scaling.
func (p Profile) EffectiveRate(r RateRule) float64 { return r.PerSecond * p.Scale }

// capacity is the bucket size for a rule after scaling. It never drops
// below one request, so a heavily scaled rule still admits something.
func (p Profile) capacity(r RateRule) float64 {
	return math.Max(1, p.EffectiveRate(r)*r.BurstSeconds)
}

func invalid(code, format string, args ...any) *control.Error {
	return &control.Error{
		Kind:    control.KindInvalid,
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		Hint:    "see docs/limits.md for the profile format",
	}
}

const maxRules = 128

// Resolve validates a spec against the known services and builds the
// profile it describes.
func Resolve(spec Spec, known map[string]bool) (Profile, error) {
	name := strings.ToLower(strings.TrimSpace(spec.Profile))
	if name == "" {
		name = BaseAWSDefault
	}
	var base []RateRule
	scale := 1.0
	switch name {
	case BaseAWSDefault:
		base = defaultRates()
	case BaseStrict:
		base, scale = defaultRates(), strictScale
	case BaseNone:
	default:
		return Profile{}, invalid("InvalidLimitsProfile", "unknown profile %q; use %q, %q or %q", spec.Profile, BaseAWSDefault, BaseStrict, BaseNone)
	}
	if spec.Scale < 0 || spec.Scale > 1000 {
		return Profile{}, invalid("InvalidLimitsProfile", "scale must be between 0 and 1000")
	}
	if spec.Scale > 0 {
		scale *= spec.Scale
	}
	if len(spec.Rates) > maxRules {
		return Profile{}, invalid("InvalidLimitsProfile", "a profile can have at most %d rate rules", maxRules)
	}

	rules := append([]RateRule{}, base...)
	index := make(map[string]int, len(rules))
	for i, r := range rules {
		index[r.ID] = i
	}
	seen := map[string]bool{}
	for i, r := range spec.Rates {
		n, err := r.normalise(i, known)
		if err != nil {
			return Profile{}, err
		}
		if seen[n.ID] {
			return Profile{}, invalid("InvalidRateRule", "duplicate rule id %q", n.ID)
		}
		seen[n.ID] = true
		if at, ok := index[n.ID]; ok {
			rules[at] = n
			continue
		}
		index[n.ID] = len(rules)
		rules = append(rules, n)
	}

	active := rules[:0]
	for _, r := range rules {
		if !r.Disabled {
			active = append(active, r)
		}
	}
	if len(active) == 0 {
		return Profile{}, invalid("InvalidLimitsProfile", "the profile has no active rules")
	}
	return Profile{Name: name, Scale: scale, Rates: active}, nil
}

func (r RateRule) normalise(index int, known map[string]bool) (RateRule, error) {
	r.Service = strings.ToLower(strings.TrimSpace(r.Service))
	if r.ID == "" {
		r.ID = fmt.Sprintf("rate-%d", index+1)
	}
	if r.Disabled {
		// Turning a base rule off needs nothing but its ID.
		return r, nil
	}
	if r.Service == "" {
		return RateRule{}, invalid("InvalidRateRule", "rule %q: service is required", r.ID)
	}
	if known != nil && !known[r.Service] {
		return RateRule{}, invalid("InvalidRateRule", "rule %q: unknown service %q", r.ID, r.Service)
	}
	if r.PerSecond <= 0 {
		return RateRule{}, invalid("InvalidRateRule", "rule %q: per_second must be greater than 0", r.ID)
	}
	if r.BurstSeconds < 0 {
		return RateRule{}, invalid("InvalidRateRule", "rule %q: burst_seconds must not be negative", r.ID)
	}
	if r.BurstSeconds == 0 {
		r.BurstSeconds = 1
	}
	switch r.Per {
	case "":
		r.Per = ScopeAccount
	case ScopeAccount, ScopeResource:
	case ScopePrefix:
		if r.Service != "s3" {
			return RateRule{}, invalid("InvalidRateRule", "rule %q: per \"prefix\" only applies to service \"s3\"", r.ID)
		}
	default:
		return RateRule{}, invalid("InvalidRateRule", "rule %q: per must be \"account\", \"resource\" or \"prefix\"", r.ID)
	}
	r.Operations = append([]string{}, r.Operations...)
	return r, nil
}

// matches reports whether the rule covers the request.
func (r RateRule) matches(req Request) bool {
	if r.Service != req.Service {
		return false
	}
	if len(r.Operations) == 0 {
		return true
	}
	for _, op := range r.Operations {
		if strings.EqualFold(op, req.Operation) {
			return true
		}
	}
	return false
}

// keyFor names the bucket a request draws from.
func (r RateRule) keyFor(req Request) QuotaKey {
	key := QuotaKey{RuleID: r.ID, Account: req.Account, Region: req.Region}
	switch r.Per {
	case ScopeResource:
		key.Resource = req.Resource
	case ScopePrefix:
		key.Resource = req.Prefix
	}
	return key
}
