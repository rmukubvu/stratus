package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const loadUsage = `usage:
  stratus load start [--name n] [--limits profile] [--chaos experiment.json]   start recording a load run
  stratus load status [run-id]                                                 the current (or a past) run's summary and timeline
  stratus load stop [--json]                                                   stop the run, undo its limits and chaos, print a report
  stratus load list                                                            recent runs
  stratus load run [--name n] [--limits profile] [--chaos file] -- <command>   start a run, run the command (e.g. k6 run load.js), stop the run

--limits takes aws-default, strict or a profile file; --chaos a chaos experiment file;
--json prints the final summary as JSON instead of a report.
The command gets STRATUS_RUN_ID and STRATUS_ENDPOINT in its environment, and
'stratus load run' exits with its exit code.
`

// commandRunner runs a load generator; tests replace it.
var commandRunner = defaultCommandRunner

func defaultCommandRunner(ctx context.Context, argv []string, env []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	cmd.Env = append(os.Environ(), env...)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// errCommandFailed carries a load generator's non-zero exit code.
type errCommandFailed struct{ code int }

func (e errCommandFailed) Error() string { return fmt.Sprintf("command exited with code %d", e.code) }

func runLoad(ctx context.Context, c ControlClient, endpoint string, args []string, stdout, stderr io.Writer) (json.RawMessage, error) {
	if len(args) == 0 {
		fmt.Fprint(stderr, loadUsage)
		return nil, errUsage
	}
	switch verb, rest := args[0], args[1:]; verb {
	case "start":
		in, err := parseLoadArgs(rest, stderr)
		if err != nil {
			return nil, err
		}
		return c.StartRun(ctx, in.spec)
	case "status", "show":
		return c.GetRun(ctx, runID(rest, "current"))
	case "stop":
		asJSON, rest := jsonFlag(rest)
		summary, err := c.StopRun(ctx, runID(rest, "current"))
		if err != nil {
			return nil, err
		}
		return summaryOutput(summary, asJSON, stdout)
	case "list", "ls":
		return c.ListRuns(ctx)
	case "run":
		in, err := parseLoadArgs(rest, stderr)
		if err != nil {
			return nil, err
		}
		if len(in.argv) == 0 {
			fmt.Fprint(stderr, loadUsage)
			return nil, errUsage
		}
		summary, err := runWithLoad(ctx, c, endpoint, in.spec, in.argv, stdout, stderr)
		if summary == nil {
			return nil, err
		}
		out, reportErr := summaryOutput(summary, in.asJSON, stdout)
		return out, errors.Join(err, reportErr)
	default:
		fmt.Fprintf(stderr, "stratus load: unknown command %q\n%s", verb, loadUsage)
		return nil, errUsage
	}
}

// runWithLoad wraps a load generator in a run. The run is stopped even
// when the command fails, so its limits and chaos never outlive it.
func runWithLoad(ctx context.Context, c ControlClient, endpoint string, spec json.RawMessage, argv []string, stdout, stderr io.Writer) (json.RawMessage, error) {
	started, err := c.StartRun(ctx, spec)
	if err != nil {
		return nil, err
	}
	var run struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(started, &run)
	fmt.Fprintf(stderr, "stratus load: recording %s; running %s\n", run.ID, strings.Join(argv, " "))

	code, runErr := commandRunner(ctx, argv, []string{"STRATUS_RUN_ID=" + run.ID, "STRATUS_ENDPOINT=" + endpoint}, stdout, stderr)
	// Stop with a fresh context: an interrupt cancels ctx, and the run
	// must still end.
	summary, stopErr := c.StopRun(context.WithoutCancel(ctx), run.ID)
	switch {
	case runErr != nil:
		return summary, runErr
	case stopErr != nil:
		return nil, stopErr
	case code != 0:
		return summary, errCommandFailed{code: code}
	}
	return summary, nil
}

// loadArgs are the parsed flags of start and run.
type loadArgs struct {
	// spec is the run to start, as the control API takes it.
	spec json.RawMessage
	// argv is the command after the flags, for run.
	argv []string
	// asJSON prints the final summary as JSON instead of a report.
	asJSON bool
}

func parseLoadArgs(args []string, stderr io.Writer) (loadArgs, error) {
	fs := flag.NewFlagSet("stratus load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "Label for the run, e.g. the script name")
	limitsArg := fs.String("limits", "", "Limits profile for the run: aws-default, strict or a file")
	chaosArg := fs.String("chaos", "", "Chaos experiment file to run during the run")
	asJSON := fs.Bool("json", false, "Print the final summary as JSON instead of a report")
	if err := fs.Parse(args); err != nil {
		return loadArgs{}, errUsage
	}
	spec := map[string]json.RawMessage{}
	if *name != "" {
		spec["name"], _ = json.Marshal(*name)
	}
	if *limitsArg != "" {
		l, err := limitsSpec(*limitsArg)
		if err != nil {
			return loadArgs{}, err
		}
		spec["limits"] = l
	}
	if *chaosArg != "" {
		raw, err := readSpec(*chaosArg)
		if err != nil {
			return loadArgs{}, err
		}
		if !json.Valid(raw) {
			return loadArgs{}, fmt.Errorf("%s is not valid JSON", *chaosArg)
		}
		spec["chaos"] = raw
	}
	out, _ := json.Marshal(spec)
	return loadArgs{spec: out, argv: fs.Args(), asJSON: *asJSON}, nil
}

func runID(args []string, fallback string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return fallback
}

// jsonFlag strips a --json flag from args.
func jsonFlag(args []string) (bool, []string) {
	rest := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--json" || a == "-json" {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// summaryOutput prints a final run summary as a report and returns
// nothing more to print, or returns the JSON for the caller to print.
func summaryOutput(summary json.RawMessage, asJSON bool, stdout io.Writer) (json.RawMessage, error) {
	if asJSON {
		return summary, nil
	}
	return nil, writeRunReport(stdout, summary)
}
