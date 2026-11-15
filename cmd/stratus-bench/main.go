// Command stratus-bench measures how long a test suite spends getting back to
// a known state between tests, for each isolation strategy Stratus supports,
// and with "boot", how long a fresh machine takes to get from nothing to a
// Stratus serving a restored snapshot.
//
//	go run ./cmd/stratus-bench -tests 20 -suite 200
//	go build -o stratus ./cmd/stratus && go run ./cmd/stratus-bench boot -binary ./stratus
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/stratus/internal/bench"
	"github.com/stratus/internal/ci"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "boot" {
		os.Exit(boot(os.Args[2:]))
	}
	tests := flag.Int("tests", 20, "Simulated tests per strategy")
	suite := flag.Int("suite", 200, "Suite size used for the total-overhead column")
	scale := flag.Int("scale", 1, "Multiply the default fixture's objects, items and messages")
	jsonOut := flag.String("json", "", "Also write raw results as JSON to this file")
	flag.Parse()
	if *tests < 1 || *suite < 1 || *scale < 1 {
		fmt.Fprintln(os.Stderr, "stratus-bench: -tests, -suite and -scale must be at least 1")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	f := bench.DefaultFixture()
	f.ObjectsPerBucket *= *scale
	f.ItemsPerTable *= *scale
	f.MessagesPerQueue *= *scale

	host := bench.Host{Start: ci.StartInProcess, Ready: ci.WaitReady, TempDir: ci.TempDataDir, Remove: os.RemoveAll, Fixture: f}
	var results []bench.Result
	for _, s := range bench.Strategies(host) {
		fmt.Fprintf(os.Stderr, "measuring %s...\n", s.Name())
		r, err := bench.Run(ctx, s, f, *tests)
		if err != nil {
			fmt.Fprintln(os.Stderr, "stratus-bench:", err)
			os.Exit(1)
		}
		results = append(results, r)
	}
	if err := bench.WriteMarkdown(os.Stdout, f, *tests, *suite, results); err != nil {
		fmt.Fprintln(os.Stderr, "stratus-bench:", err)
		os.Exit(1)
	}
	if *jsonOut != "" {
		raw, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(*jsonOut, raw, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "stratus-bench:", err)
			os.Exit(1)
		}
	}
}

// boot measures cold boots of a real stratus binary.
func boot(args []string) int {
	flags := flag.NewFlagSet("boot", flag.ExitOnError)
	runs := flags.Int("runs", 10, "Cold boots per scenario")
	binary := flags.String("binary", "stratus", "Path to the stratus binary to boot")
	engine := flags.Bool("engine", false, "Add a scenario with a PostgreSQL RDS instance (needs Docker and postgres:16-alpine)")
	scale := flags.Int("scale", 1, "Multiply the default fixture's objects, items and messages")
	jsonOut := flags.String("json", "", "Also write raw results as JSON to this file")
	_ = flags.Parse(args)
	if *runs < 1 || *scale < 1 {
		fmt.Fprintln(os.Stderr, "stratus-bench boot: -runs and -scale must be at least 1")
		return 2
	}
	path, err := exec.LookPath(*binary)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stratus-bench boot: find the stratus binary:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	f := bench.DefaultFixture()
	f.ObjectsPerBucket *= *scale
	f.ItemsPerTable *= *scale
	f.MessagesPerQueue *= *scale
	prep := bench.Host{Start: ci.StartInProcess, Ready: ci.WaitReady, TempDir: ci.TempDataDir, Remove: os.RemoveAll, Fixture: f}
	host := bench.BootHost{
		Booter:  bench.ProcessBooter{Binary: path},
		Ready:   bench.PollReady(2 * time.Millisecond),
		TempDir: ci.TempDataDir,
		Remove:  os.RemoveAll,
		Now:     time.Now,
	}

	var results []bench.BootResult
	for _, s := range bench.BootScenarios(f, *engine) {
		fmt.Fprintf(os.Stderr, "booting %s...\n", s.Name)
		seed, dataDir, err := bench.PrepareSeed(ctx, prep, s, "boot")
		if err != nil {
			fmt.Fprintln(os.Stderr, "stratus-bench boot:", err)
			return 1
		}
		r, err := bench.MeasureBoot(ctx, host, s, seed, *runs)
		if dataDir != "" {
			_ = os.RemoveAll(dataDir)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "stratus-bench boot:", err)
			return 1
		}
		results = append(results, r)
	}
	if err := bench.WriteBootMarkdown(os.Stdout, *runs, results); err != nil {
		fmt.Fprintln(os.Stderr, "stratus-bench boot:", err)
		return 1
	}
	if *jsonOut != "" {
		raw, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(*jsonOut, raw, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "stratus-bench boot:", err)
			return 1
		}
	}
	return 0
}
