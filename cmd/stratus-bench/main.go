// Command stratus-bench measures how long a test suite spends getting back to
// a known state between tests, for each isolation strategy Stratus supports.
//
//	go run ./cmd/stratus-bench -tests 20 -suite 200
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/stratus/internal/bench"
	"github.com/stratus/internal/ci"
)

func main() {
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
