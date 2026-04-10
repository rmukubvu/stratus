package cli

import "testing"

func TestParseMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
		mode Mode
		rest []string
	}{
		{name: "default dev", args: nil, mode: ModeDev, rest: nil},
		{name: "explicit dev", args: []string{"dev", "--port", "9999"}, mode: ModeDev, rest: []string{"--port", "9999"}},
		{name: "explicit serve", args: []string{"serve", "--port", "9999"}, mode: ModeServe, rest: []string{"--port", "9999"}},
		{name: "legacy flags means serve", args: []string{"--port", "9999"}, mode: ModeServe, rest: []string{"--port", "9999"}},
		{name: "unknown command falls back to dev", args: []string{"sandbox"}, mode: ModeDev, rest: []string{"sandbox"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mode, rest := ParseMode(tc.args)
			if mode != tc.mode {
				t.Fatalf("mode = %q, want %q", mode, tc.mode)
			}
			if len(rest) != len(tc.rest) {
				t.Fatalf("rest len = %d, want %d", len(rest), len(tc.rest))
			}
			for i := range rest {
				if rest[i] != tc.rest[i] {
					t.Fatalf("rest[%d] = %q, want %q", i, rest[i], tc.rest[i])
				}
			}
		})
	}
}
