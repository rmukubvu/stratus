package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/stratus/internal/control/client"
)

// EndpointEnv overrides the default control endpoint.
const EndpointEnv = "STRATUS_ENDPOINT"

// ControlClient is the subset of the control client the CLI needs.
type ControlClient interface {
	Reset(ctx context.Context, services ...string) (json.RawMessage, error)
	CreateSnapshot(ctx context.Context, name string) (json.RawMessage, error)
	ListSnapshots(ctx context.Context) (json.RawMessage, error)
	RestoreSnapshot(ctx context.Context, name string) (json.RawMessage, error)
	DeleteSnapshot(ctx context.Context, name string) error
	WaitReady(ctx context.Context, interval time.Duration) error
	StartChaos(ctx context.Context, spec json.RawMessage) (json.RawMessage, error)
	ChaosReport(ctx context.Context) (json.RawMessage, error)
	StopChaos(ctx context.Context) (json.RawMessage, error)
	Diagnose(ctx context.Context, query url.Values) (json.RawMessage, error)
}

// ClientFactory builds a client for an endpoint; tests substitute a fake.
type ClientFactory func(endpoint string) (ControlClient, error)

// DefaultClientFactory returns real HTTP clients.
func DefaultClientFactory(endpoint string) (ControlClient, error) {
	return client.New(endpoint, nil)
}

// RunControl executes a control subcommand against a running Stratus and
// returns the process exit code. Successful responses are written to stdout
// as JSON so scripts and agents can parse them.
func RunControl(ctx context.Context, mode Mode, args []string, newClient ClientFactory, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stratus "+string(mode), flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultEndpoint := os.Getenv(EndpointEnv)
	if defaultEndpoint == "" {
		defaultEndpoint = client.DefaultEndpoint
	}
	endpoint := fs.String("endpoint", defaultEndpoint, "Stratus endpoint (env "+EndpointEnv+")")
	timeout := fs.Duration("timeout", 60*time.Second, "How long to wait (wait only)")
	since := fs.String("since", "", "Only failures this recent, e.g. 5m (diagnose only)")
	service := fs.String("service", "", "Only this service (diagnose only)")
	excludeInjected := fs.Bool("exclude-injected", false, "Leave out chaos faults (diagnose only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	c, err := newClient(*endpoint)
	if err != nil {
		fmt.Fprintf(stderr, "stratus: %v\n", err)
		return 2
	}

	rest := fs.Args()
	var out json.RawMessage
	switch mode {
	case ModeWait:
		waitCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		err = c.WaitReady(waitCtx, 200*time.Millisecond)
		if err == nil {
			out = json.RawMessage(`{"ready":true}`)
		}
	case ModeReset:
		out, err = c.Reset(ctx, rest...)
	case ModeSnapshot:
		out, err = runSnapshot(ctx, c, rest, stderr)
		if errors.Is(err, errUsage) {
			return 2
		}
	case ModeDiagnose:
		q := url.Values{}
		if *since != "" {
			q.Set("since", *since)
		}
		if *service != "" {
			q.Set("service", *service)
		}
		if *excludeInjected {
			q.Set("exclude_injected", "true")
		}
		out, err = c.Diagnose(ctx, q)
	case ModeChaos:
		out, err = runChaos(ctx, c, rest, stderr)
		if errors.Is(err, errUsage) {
			return 2
		}
	default:
		fmt.Fprintf(stderr, "stratus: %q is not a control command\n", mode)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "stratus %s: %v\n", mode, err)
		return 1
	}
	if len(out) > 0 {
		fmt.Fprintln(stdout, string(out))
	}
	return 0
}

var errUsage = errors.New("usage")

const snapshotUsage = `usage:
  stratus snapshot save [name]       capture current state
  stratus snapshot restore <name>    replace current state with a snapshot
  stratus snapshot list              list snapshots
  stratus snapshot delete <name>     delete a snapshot
`

func runSnapshot(ctx context.Context, c ControlClient, args []string, stderr io.Writer) (json.RawMessage, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, snapshotUsage)
		return nil, errUsage
	}
	verb, rest := args[0], args[1:]
	needName := func() (string, error) {
		if len(rest) != 1 {
			fmt.Fprint(stderr, snapshotUsage)
			return "", errUsage
		}
		return rest[0], nil
	}
	switch verb {
	case "save", "create":
		if len(rest) > 1 {
			fmt.Fprint(stderr, snapshotUsage)
			return nil, errUsage
		}
		name := ""
		if len(rest) == 1 {
			name = rest[0]
		}
		return c.CreateSnapshot(ctx, name)
	case "restore":
		name, err := needName()
		if err != nil {
			return nil, err
		}
		return c.RestoreSnapshot(ctx, name)
	case "list", "ls":
		return c.ListSnapshots(ctx)
	case "delete", "rm":
		name, err := needName()
		if err != nil {
			return nil, err
		}
		return nil, c.DeleteSnapshot(ctx, name)
	default:
		fmt.Fprintf(stderr, "stratus snapshot: unknown command %q\n%s", verb, snapshotUsage)
		return nil, errUsage
	}
}

const chaosUsage = `usage:
  stratus chaos start <experiment.json | ->   start a chaos experiment (replaces any current one)
  stratus chaos status                        show per-rule counts and recent faults
  stratus chaos stop                          stop and print the final report
`

func runChaos(ctx context.Context, c ControlClient, args []string, stderr io.Writer) (json.RawMessage, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, chaosUsage)
		return nil, errUsage
	}
	switch verb, rest := args[0], args[1:]; verb {
	case "start":
		if len(rest) != 1 {
			fmt.Fprint(stderr, chaosUsage)
			return nil, errUsage
		}
		spec, err := readSpec(rest[0])
		if err != nil {
			return nil, err
		}
		if !json.Valid(spec) {
			return nil, fmt.Errorf("%s is not valid JSON", rest[0])
		}
		return c.StartChaos(ctx, spec)
	case "status":
		return c.ChaosReport(ctx)
	case "stop":
		return c.StopChaos(ctx)
	default:
		fmt.Fprintf(stderr, "stratus chaos: unknown command %q\n%s", verb, chaosUsage)
		return nil, errUsage
	}
}

func readSpec(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
