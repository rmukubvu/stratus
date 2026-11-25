package config

import (
	"flag"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func parse(t *testing.T, args ...string) *Config {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestIdleTimeoutIsOffByDefault(t *testing.T) {
	cfg := parse(t)
	if err := cfg.ApplyIdleTimeoutEnv(env(nil)); err != nil {
		t.Fatal(err)
	}
	if cfg.IdleTimeout != 0 {
		t.Fatalf("IdleTimeout = %v, want 0", cfg.IdleTimeout)
	}
}

func TestIdleTimeoutFlagWinsOverTheEnvironment(t *testing.T) {
	cfg := parse(t, "--idle-timeout", "5m")
	if err := cfg.ApplyIdleTimeoutEnv(env(map[string]string{"STRATUS_IDLE_TIMEOUT": "1h"})); err != nil {
		t.Fatal(err)
	}
	if cfg.IdleTimeout != 5*time.Minute {
		t.Fatalf("IdleTimeout = %v, want 5m", cfg.IdleTimeout)
	}
}

func TestIdleTimeoutFallsBackToTheEnvironment(t *testing.T) {
	cfg := parse(t)
	if err := cfg.ApplyIdleTimeoutEnv(env(map[string]string{"STRATUS_IDLE_TIMEOUT": "90s"})); err != nil {
		t.Fatal(err)
	}
	if cfg.IdleTimeout != 90*time.Second {
		t.Fatalf("IdleTimeout = %v, want 90s", cfg.IdleTimeout)
	}
}

func TestAnUnreadableIdleTimeoutInTheEnvironmentIsRefused(t *testing.T) {
	cfg := parse(t)
	if err := cfg.ApplyIdleTimeoutEnv(env(map[string]string{"STRATUS_IDLE_TIMEOUT": "soon"})); err == nil {
		t.Fatal("accepted STRATUS_IDLE_TIMEOUT=soon")
	}
}

func TestANegativeIdleTimeoutIsRefused(t *testing.T) {
	cfg := parse(t, "--idle-timeout", "-1m", "--data-dir", t.TempDir())
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted a negative idle timeout")
	}
}

func TestRequestValidationIsOnUnlessTurnedOff(t *testing.T) {
	cases := []struct {
		flag, env string
		want      bool
		wantErr   bool
	}{
		{"", "", true, false},
		{"off", "", false, false},
		{"", "off", false, false},
		{"on", "off", true, false},
		{"maybe", "", false, true},
	}
	for _, c := range cases {
		got, err := Config{Validation: c.flag}.ValidationEnabled(func(string) string { return c.env })
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("flag %q env %q: got %v, %v", c.flag, c.env, got, err)
		}
	}
}
