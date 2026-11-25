// Package check scans a codebase for the AWS SDK calls it makes and reports
// which ones Stratus implements, which it answers with 501, and which name
// services Stratus does not emulate. It reads source only; nothing runs.
//
// It recognises boto3 / botocore (Python), the AWS SDK for JavaScript v3
// (including the DynamoDB document client) and the AWS SDK for Go v2.
package check

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stratus/internal/awsmodel"
)

// Location is where a call appears.
type Location struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// Call is one AWS operation a source file calls. Service is the SDK's
// service name (a botocore model name such as "cognito-idp"); Operation is
// the AWS operation name.
type Call struct {
	Service   string
	Operation string
	Location  Location
}

// Scanner finds the AWS calls in one source file.
type Scanner interface {
	// Accepts reports whether the scanner reads files with this name.
	Accepts(name string) bool
	Scan(file string, src []byte) []Call
}

// Finding is one operation the codebase calls, and how Stratus supports it.
type Finding struct {
	Service   string           `json:"service"`
	Operation string           `json:"operation"`
	Support   awsmodel.Support `json:"support"`
	Locations []Location       `json:"locations"`
}

// Report is the result of a scan.
type Report struct {
	Root     string    `json:"root"`
	Files    int       `json:"files_scanned"`
	Findings []Finding `json:"findings"`
}

// Count returns how many findings have the given support.
func (r Report) Count(s awsmodel.Support) int {
	n := 0
	for _, f := range r.Findings {
		if f.Support == s {
			n++
		}
	}
	return n
}

// OK reports whether every call the codebase makes is implemented.
func (r Report) OK() bool { return r.Count(awsmodel.Implemented) == len(r.Findings) }

// skipDirs are directories that hold dependencies or build output, not the
// project's own code.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	".venv": true, "venv": true, "__pycache__": true, "target": true, ".terraform": true,
	"cdk.out": true, ".aws-sam": true, ".next": true,
}

const maxFileSize = 1 << 20

// DefaultScanners are the scanners Run uses when none are given.
func DefaultScanners() []Scanner {
	return []Scanner{Python{}, JavaScript{}, Go{}}
}

// Run scans every source file under root.
func Run(root string, scanners ...Scanner) (Report, error) {
	if len(scanners) == 0 {
		scanners = DefaultScanners()
	}
	report := Report{Root: root}
	var calls []Call
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		for _, s := range scanners {
			if !s.Accepts(d.Name()) {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			report.Files++
			calls = append(calls, s.Scan(filepath.ToSlash(rel), src)...)
			break
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	report.Findings = Assess(calls)
	return report, nil
}

// Assess groups calls by operation and looks each up in the catalogue.
func Assess(calls []Call) []Finding {
	index := modelIndex()
	byKey := map[string]*Finding{}
	for _, c := range calls {
		service, known := index[c.Service]
		support := awsmodel.UnknownService
		if known {
			support = awsmodel.Lookup(service, c.Operation)
		} else {
			service = c.Service
		}
		key := service + "." + c.Operation
		f, ok := byKey[key]
		if !ok {
			f = &Finding{Service: service, Operation: c.Operation, Support: support}
			byKey[key] = f
		}
		f.Locations = append(f.Locations, c.Location)
	}
	out := make([]Finding, 0, len(byKey))
	for _, f := range byKey {
		sort.Slice(f.Locations, func(i, j int) bool {
			if f.Locations[i].File != f.Locations[j].File {
				return f.Locations[i].File < f.Locations[j].File
			}
			return f.Locations[i].Line < f.Locations[j].Line
		})
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Operation < out[j].Operation
	})
	return out
}

// modelIndex maps SDK service names (botocore model names) to the Stratus
// service that serves them.
func modelIndex() map[string]string {
	index := map[string]string{}
	for _, name := range awsmodel.Services() {
		svc, err := awsmodel.Load(name)
		if err != nil {
			continue
		}
		for _, api := range svc.APIs {
			index[api.Model] = name
		}
	}
	return index
}

// operationsBy maps an SDK's spelling of each operation (for example
// boto3's snake_case) to the operation, for one botocore model. Models
// Stratus does not emulate yield nil.
func operationsBy(model string, spell func(string) string) map[string]string {
	service, ok := modelIndex()[model]
	if !ok {
		return nil
	}
	svc, err := awsmodel.Load(service)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, api := range svc.APIs {
		if api.Model != model {
			continue
		}
		for op := range api.Operations {
			out[spell(op)] = op
		}
	}
	return out
}

// lineOf is the 1-based line of byte offset off in src.
func lineOf(src []byte, off int) int {
	return strings.Count(string(src[:off]), "\n") + 1
}
