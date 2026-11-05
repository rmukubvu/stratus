package bench

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Result is the measured cost of one strategy.
type Result struct {
	Strategy    string          `json:"strategy"`
	Description string          `json:"description"`
	Setup       time.Duration   `json:"setup_ns"`
	Samples     []time.Duration `json:"samples_ns"`
}

// Median is the typical time to get back to the seeded state between tests.
func (r Result) Median() time.Duration { return r.percentile(50) }

// P90 is the slow end of the same.
func (r Result) P90() time.Duration { return r.percentile(90) }

func (r Result) percentile(p int) time.Duration {
	if len(r.Samples) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), r.Samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	// Nearest-rank: the smallest sample with at least p% of samples at or below it.
	idx := (p*len(s)+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	return s[idx]
}

// Run measures one strategy over the given number of simulated tests. Each
// test mutates state, then the strategy isolates; only isolation is timed.
// After every isolation the fixture is verified, so a strategy that leaks
// state fails the run instead of looking fast.
func Run(ctx context.Context, s Strategy, f Fixture, tests int) (res Result, err error) {
	res = Result{Strategy: s.Name(), Description: s.Description()}
	defer func() {
		if cerr := s.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("%s: close: %w", s.Name(), cerr)
		}
	}()

	start := time.Now()
	endpoint, err := s.Setup(ctx)
	if err != nil {
		return res, fmt.Errorf("%s: setup: %w", s.Name(), err)
	}
	res.Setup = time.Since(start)
	if err := f.Verify(ctx, AWS{Endpoint: endpoint}); err != nil {
		return res, fmt.Errorf("%s: after setup: %w", s.Name(), err)
	}

	for i := 0; i < tests; i++ {
		if err := f.Mutate(ctx, AWS{Endpoint: endpoint}, i); err != nil {
			return res, fmt.Errorf("%s: test %d: %w", s.Name(), i, err)
		}
		t0 := time.Now()
		endpoint, err = s.Isolate(ctx)
		if err != nil {
			return res, fmt.Errorf("%s: isolate after test %d: %w", s.Name(), i, err)
		}
		res.Samples = append(res.Samples, time.Since(t0))
		if err := f.Verify(ctx, AWS{Endpoint: endpoint}); err != nil {
			return res, fmt.Errorf("%s: state leaked after test %d: %w", s.Name(), i, err)
		}
	}
	return res, nil
}

// WriteMarkdown renders results as a table, with each strategy's speed-up
// relative to the first (the baseline) and the isolation overhead for a
// suite of suiteSize tests.
func WriteMarkdown(w io.Writer, f Fixture, tests, suiteSize int, results []Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Fixture: %d buckets x %d objects (%s each), %d tables x %d items, %d queues x %d messages. %d tests per strategy; times are per test.\n\n",
		f.Buckets, f.ObjectsPerBucket, bytesLabel(f.ObjectBytes), f.Tables, f.ItemsPerTable, f.Queues, f.MessagesPerQueue, tests)
	fmt.Fprintf(&b, "| Strategy | Between tests | Median | p90 | Speed-up | Overhead for %d tests |\n", suiteSize)
	b.WriteString("| --- | --- | ---: | ---: | ---: | ---: |\n")
	var base time.Duration
	for i, r := range results {
		if i == 0 {
			base = r.Median()
		}
		speedup := "1.0x"
		if i > 0 && r.Median() > 0 {
			speedup = fmt.Sprintf("%.1fx", float64(base)/float64(r.Median()))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			r.Strategy, r.Description, ms(r.Median()), ms(r.P90()), speedup, secs(r.Median()*time.Duration(suiteSize)))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

func secs(d time.Duration) string {
	return fmt.Sprintf("%.1f s", d.Seconds())
}

func bytesLabel(n int) string {
	if n >= 1<<10 && n%(1<<10) == 0 {
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
