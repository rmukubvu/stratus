package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/stratus/internal/check"
)

// RunCheck runs `stratus check [--json] [dir]`: it scans dir (default ".")
// for AWS SDK calls and reports which Stratus implements. It exits 0 when
// every call is implemented, 1 when some are not, and 2 on bad usage.
func RunCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stratus check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: stratus check [--json] [dir]")
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	report, err := check.Run(root)
	if err != nil {
		fmt.Fprintf(stderr, "stratus check: %v\n", err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return 2
		}
	} else {
		fmt.Fprint(stdout, report.Markdown())
	}
	if !report.OK() {
		return 1
	}
	return 0
}
