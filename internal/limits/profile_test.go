package limits

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stratus/internal/control"
)

var known = map[string]bool{"s3": true, "ssm": true, "kms": true, "secretsmanager": true, "lambda": true, "sqs": true, "dynamodb": true}

func controlCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var ce *control.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a control error", err)
	}
	return ce.Code
}

func TestResolveBaseProfiles(t *testing.T) {
	def, err := Resolve(Spec{}, known)
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != BaseAWSDefault || def.Scale != 1 || len(def.Rates) != len(defaultRates()) {
		t.Fatalf("aws-default: %+v", def)
	}
	strict, _ := Resolve(Spec{Profile: "STRICT", Scale: 0.5}, known)
	if strict.Name != BaseStrict || strict.Scale != strictScale*0.5 {
		t.Fatalf("strict: name %q scale %v", strict.Name, strict.Scale)
	}
}

func TestResolveOverridesAddsAndDisablesRules(t *testing.T) {
	p, err := Resolve(Spec{Rates: []RateRule{
		{ID: "ssm-get-parameters", Service: "ssm", Operations: []string{"GetParameter"}, PerSecond: 1000},
		{ID: "kms-cryptographic-operations", Disabled: true},
		{Service: "sqs", Operations: []string{"SendMessage"}, PerSecond: 300, Per: ScopeResource},
	}}, known)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]RateRule{}
	for _, r := range p.Rates {
		byID[r.ID] = r
	}
	if byID["ssm-get-parameters"].PerSecond != 1000 {
		t.Fatalf("override not applied: %+v", byID["ssm-get-parameters"])
	}
	if _, ok := byID["kms-cryptographic-operations"]; ok {
		t.Fatal("disabled rule still active")
	}
	added, ok := byID["rate-3"]
	if !ok || added.BurstSeconds != 1 || added.Per != ScopeResource {
		t.Fatalf("added rule: %+v (ok=%v)", added, ok)
	}
	if len(p.Rates) != len(defaultRates()) {
		t.Fatalf("got %d rules, want %d (one replaced, one removed, one added)", len(p.Rates), len(defaultRates()))
	}
}

func TestResolveRejectsBadSpecs(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"unknown base", Spec{Profile: "fast"}, "InvalidLimitsProfile"},
		{"negative scale", Spec{Scale: -1}, "InvalidLimitsProfile"},
		{"empty none", Spec{Profile: BaseNone}, "InvalidLimitsProfile"},
		{"no service", Spec{Rates: []RateRule{{ID: "x", PerSecond: 1}}}, "InvalidRateRule"},
		{"unknown service", Spec{Rates: []RateRule{{ID: "x", Service: "nope", PerSecond: 1}}}, "InvalidRateRule"},
		{"zero rate", Spec{Rates: []RateRule{{ID: "x", Service: "ssm"}}}, "InvalidRateRule"},
		{"prefix outside s3", Spec{Rates: []RateRule{{ID: "x", Service: "ssm", PerSecond: 1, Per: ScopePrefix}}}, "InvalidRateRule"},
		{"bad per", Spec{Rates: []RateRule{{ID: "x", Service: "ssm", PerSecond: 1, Per: "table"}}}, "InvalidRateRule"},
		{"duplicate id", Spec{Rates: []RateRule{{ID: "x", Service: "ssm", PerSecond: 1}, {ID: "x", Service: "ssm", PerSecond: 2}}}, "InvalidRateRule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.spec, known)
			if got := controlCode(t, err); got != tc.want {
				t.Fatalf("code = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestThrottleErrorMatchesEachService(t *testing.T) {
	cases := []struct {
		service string
		kind    Kind
		status  int
		code    string
	}{
		{"s3", KindRate, http.StatusServiceUnavailable, "SlowDown"},
		{"lambda", KindConcurrency, http.StatusTooManyRequests, "TooManyRequestsException"},
		{"sns", KindRate, http.StatusBadRequest, "Throttling"},
		{"ssm", KindRate, http.StatusBadRequest, "ThrottlingException"},
		{"kms", KindRate, http.StatusBadRequest, "ThrottlingException"},
		{"events", KindRate, http.StatusBadRequest, "ThrottlingException"},
	}
	for _, tc := range cases {
		e := ThrottleError(Breach{Service: tc.service, Kind: tc.kind, RuleID: "rule-x"})
		if e.StatusCode != tc.status || e.Code != tc.code || !strings.Contains(e.Message, "rule-x") {
			t.Errorf("%s: got %d %s %q", tc.service, e.StatusCode, e.Code, e.Message)
		}
	}
}

func TestParseSetting(t *testing.T) {
	files := map[string]string{
		"ok.json":  `{"profile":"none","scale":0.5,"rates":[{"service":"ssm","per_second":5}]}`,
		"bad.json": `{"profil":"none"}`,
	}
	read := func(path string) ([]byte, error) {
		if s, ok := files[path]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
	if _, ok, err := ParseSetting("", read); ok || err != nil {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	if spec, ok, err := ParseSetting("Strict", read); !ok || err != nil || spec.Profile != BaseStrict {
		t.Fatalf("strict: %+v ok=%v err=%v", spec, ok, err)
	}
	if spec, ok, err := ParseSetting("ok.json", read); !ok || err != nil || spec.Scale != 0.5 || len(spec.Rates) != 1 {
		t.Fatalf("file: %+v ok=%v err=%v", spec, ok, err)
	}
	if _, _, err := ParseSetting("bad.json", read); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, _, err := ParseSetting("missing.json", read); err == nil {
		t.Fatal("missing file accepted")
	}
}
