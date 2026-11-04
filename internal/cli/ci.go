package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/stratus/internal/ci"
)

// CIRunner runs a command against a throwaway Stratus.
type CIRunner interface {
	Run(ctx context.Context, opts ci.Options) (int, error)
}

const ciUsage = `usage: stratus ci [flags] -- <command> [args...]

Starts a private Stratus on a free port with a fresh data directory, waits
until it is ready, runs the command with AWS_ENDPOINT_URL and STRATUS_ENDPOINT
pointing at it, then shuts it down and removes its data and Lambda containers.
The exit code is the command's, or 125 if Stratus itself failed.

Flags:
`

// RunCI parses `stratus ci` arguments and returns the process exit code.
func RunCI(ctx context.Context, args []string, runner CIRunner, stderr io.Writer) int {
	fs := flag.NewFlagSet("stratus ci", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, ciUsage)
		fs.PrintDefaults()
	}
	seed := fs.String("seed", "", "Snapshot directory to start from, e.g. ./data/snapshots/seeded")
	keep := fs.Bool("keep-data", false, "Keep the data directory after the run for debugging")
	logLevel := fs.String("log-level", "warn", "Stratus log level: debug, info, warn, error")
	timeout := fs.Duration("ready-timeout", 60*time.Second, "How long to wait for Stratus to become ready")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}

	code, err := runner.Run(ctx, ci.Options{
		Command:      fs.Args(),
		Seed:         *seed,
		KeepData:     *keep,
		LogLevel:     *logLevel,
		ReadyTimeout: *timeout,
		Stderr:       stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "stratus ci: %v\n", err)
	}
	return code
}
