package check

import (
	"fmt"
	"strings"

	"github.com/stratus/internal/awsmodel"
)

// Markdown renders the report for a person or an agent to read: what will
// fail against Stratus first, with where it is called, then what works.
func (r Report) Markdown() string {
	var b strings.Builder
	implemented := r.Count(awsmodel.Implemented)
	fmt.Fprintf(&b, "# Stratus check\n\nScanned %d files under %s: %d AWS operations called, %d implemented by Stratus.\n",
		r.Files, r.Root, len(r.Findings), implemented)
	if len(r.Findings) == 0 {
		b.WriteString("\nNo AWS SDK calls found (boto3, AWS SDK for JavaScript v3, AWS SDK for Go v2).\n")
		return b.String()
	}

	section := func(title, note string, support awsmodel.Support, withOperation bool) {
		if r.Count(support) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n\n", title, note)
		for _, f := range r.Findings {
			if f.Support != support {
				continue
			}
			name := f.Service + " " + f.Operation
			if !withOperation {
				name = f.Service
			}
			fmt.Fprintf(&b, "- `%s` at %s\n", name, locations(f.Locations))
		}
	}
	section("Not implemented", "AWS has these operations; Stratus answers 501 NotImplemented. Tests that call them will fail.", awsmodel.NotImplemented, true)
	section("Services Stratus does not emulate", "Calls to these services will not reach an emulated service.", awsmodel.UnknownService, false)
	section("Unknown operations", "The AWS model for this service has no such operation.", awsmodel.UnknownOperation, true)

	if implemented > 0 {
		b.WriteString("\n## Implemented\n\n")
		for _, f := range r.Findings {
			if f.Support == awsmodel.Implemented {
				fmt.Fprintf(&b, "- `%s %s` at %s\n", f.Service, f.Operation, locations(f.Locations))
			}
		}
	}
	return b.String()
}

func locations(locs []Location) string {
	const shown = 3
	parts := make([]string, 0, shown)
	for i, l := range locs {
		if i == shown {
			break
		}
		parts = append(parts, fmt.Sprintf("%s:%d", l.File, l.Line))
	}
	s := strings.Join(parts, ", ")
	if len(locs) > shown {
		s += fmt.Sprintf(" and %d more", len(locs)-shown)
	}
	return s
}
