package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/ci"
	"github.com/stratus/internal/control/client"
)

// A boot benchmark answers the question a fresh CI machine (a microVM, an
// ephemeral pod, a hosted runner) asks: how long from nothing running until
// Stratus serves the suite's seeded state? Each run starts a brand-new
// stratus process on an empty data directory, seeded from a snapshot, and
// times three moments:
//
//	ready:  the control API reports ready (snapshot restored, engines up)
//	usable: the first check of the seeded state passes
//	gone:   the process has exited and removed its containers after SIGTERM

// BootScenario is one starting point for a cold boot.
type BootScenario struct {
	Name string
	// Description says what the snapshot holds.
	Description string
	// Prepare builds the snapshot against a running Stratus. Nil boots an
	// empty Stratus with no seed.
	Prepare func(ctx context.Context, endpoint string) error
	// Verify checks the booted instance serves the seeded state.
	Verify func(ctx context.Context, endpoint string) error
}

// BootResult is one scenario's measurements.
type BootResult struct {
	Scenario    string          `json:"scenario"`
	Description string          `json:"description"`
	SeedBytes   int64           `json:"seed_bytes"`
	Ready       []time.Duration `json:"ready_ns"`
	Usable      []time.Duration `json:"usable_ns"`
	Gone        []time.Duration `json:"gone_ns"`
}

// Booted is a Stratus process that has been started.
type Booted interface {
	Endpoint() string
	// Stop asks the process to shut down and waits until it has.
	Stop() error
}

// Booter starts Stratus from scratch on dataDir, seeded from seed when it is
// not empty. It returns as soon as the process is started, not when it is
// ready, so the wait is measured.
type Booter interface {
	Boot(ctx context.Context, dataDir, seed string) (Booted, error)
}

// BootHost is what MeasureBoot needs from the outside world; tests inject
// fakes for all of it.
type BootHost struct {
	Booter  Booter
	Ready   ci.ReadyWaiter
	TempDir func() (string, error)
	Remove  func(path string) error
	Now     func() time.Time
}

// MeasureBoot cold-boots Stratus runs times from seed and verifies each boot.
func MeasureBoot(ctx context.Context, h BootHost, s BootScenario, seed string, runs int) (BootResult, error) {
	res := BootResult{Scenario: s.Name, Description: s.Description, SeedBytes: dirSize(seed)}
	for i := 0; i < runs; i++ {
		ready, usable, gone, err := h.bootOnce(ctx, s, seed)
		if err != nil {
			return res, fmt.Errorf("%s: run %d: %w", s.Name, i+1, err)
		}
		res.Ready = append(res.Ready, ready)
		res.Usable = append(res.Usable, usable)
		res.Gone = append(res.Gone, gone)
	}
	return res, nil
}

func (h BootHost) bootOnce(ctx context.Context, s BootScenario, seed string) (ready, usable, gone time.Duration, err error) {
	dir, err := h.TempDir()
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { err = errors.Join(err, h.Remove(dir)) }()

	start := h.Now()
	booted, err := h.Booter.Boot(ctx, dir, seed)
	if err != nil {
		return 0, 0, 0, err
	}
	stopped := false
	defer func() {
		if !stopped {
			err = errors.Join(err, booted.Stop())
		}
	}()

	if err := h.Ready(ctx, booted.Endpoint()); err != nil {
		return 0, 0, 0, fmt.Errorf("wait until ready: %w", err)
	}
	ready = h.Now().Sub(start)
	if s.Verify != nil {
		if err := s.Verify(ctx, booted.Endpoint()); err != nil {
			return 0, 0, 0, fmt.Errorf("seeded state: %w", err)
		}
	}
	usable = h.Now().Sub(start)

	stopAt := h.Now()
	stopped = true
	if err := booted.Stop(); err != nil {
		return 0, 0, 0, fmt.Errorf("stop: %w", err)
	}
	gone = h.Now().Sub(stopAt)
	return ready, usable, gone, nil
}

// PrepareSeed builds a scenario's snapshot with an in-process Stratus and
// returns its directory, which outlives the server. An empty path means
// the scenario boots unseeded.
func PrepareSeed(ctx context.Context, host Host, s BootScenario, name string) (seed, dataDir string, err error) {
	if s.Prepare == nil {
		return "", "", nil
	}
	inst, err := host.launch(ctx, "")
	if err != nil {
		return "", "", err
	}
	stopErr := func() error { return inst.server.Stop() }
	if err := s.Prepare(ctx, inst.server.Endpoint()); err != nil {
		return "", "", errors.Join(fmt.Errorf("%s: prepare: %w", s.Name, err), stopErr(), host.Remove(inst.dir))
	}
	c, err := client.New(inst.server.Endpoint(), nil)
	if err == nil {
		_, err = c.CreateSnapshot(ctx, name)
	}
	if err != nil {
		return "", "", errors.Join(fmt.Errorf("%s: snapshot: %w", s.Name, err), stopErr(), host.Remove(inst.dir))
	}
	if err := stopErr(); err != nil {
		return "", "", errors.Join(err, host.Remove(inst.dir))
	}
	return filepath.Join(inst.dir, "snapshots", name), inst.dir, nil
}

// BootScenarios are the standard starting points: an empty Stratus, the
// benchmark fixture, and, when withEngine is set, the fixture plus a
// PostgreSQL-backed RDS instance holding data (which needs Docker and the
// postgres:16-alpine image).
func BootScenarios(f Fixture, withEngine bool) []BootScenario {
	fixture := func(ctx context.Context, endpoint string) error { return f.Seed(ctx, AWS{Endpoint: endpoint}) }
	verify := func(ctx context.Context, endpoint string) error { return f.Verify(ctx, AWS{Endpoint: endpoint}) }
	out := []BootScenario{
		{
			Name:        "empty",
			Description: "no seed",
			Verify: func(ctx context.Context, endpoint string) error {
				_, err := AWS{Endpoint: endpoint}.do(ctx, "GET", "/", nil, nil) // ListBuckets
				return err
			},
		},
		{
			Name:        "fixture snapshot",
			Description: fmt.Sprintf("%d buckets x %d objects, %d tables x %d items, %d queues x %d messages", f.Buckets, f.ObjectsPerBucket, f.Tables, f.ItemsPerTable, f.Queues, f.MessagesPerQueue),
			Prepare:     fixture,
			Verify:      verify,
		},
	}
	if withEngine {
		out = append(out, BootScenario{
			Name:        "fixture + RDS snapshot",
			Description: "the fixture plus a PostgreSQL RDS instance restored from the snapshot",
			Prepare: func(ctx context.Context, endpoint string) error {
				if err := fixture(ctx, endpoint); err != nil {
					return err
				}
				return AWS{Endpoint: endpoint}.CreateDBInstance(ctx, benchDB, "postgres", benchDBUser, benchDBPassword)
			},
			Verify: func(ctx context.Context, endpoint string) error {
				if err := verify(ctx, endpoint); err != nil {
					return err
				}
				address, err := AWS{Endpoint: endpoint}.DBInstanceEndpoint(ctx, benchDB)
				if err != nil {
					return err
				}
				return backing.PostgresStartup(benchDBUser, "postgres")(ctx, address)
			},
		})
	}
	return out
}

const (
	benchDB         = "bench-db"
	benchDBUser     = "bench"
	benchDBPassword = "bench-password"
)

// ProcessBooter boots the stratus binary at Binary as a child process, as a
// CI job does, so the measurement includes process start-up.
type ProcessBooter struct {
	Binary string
	// Stderr receives the process's logs; nil discards them.
	Stderr io.Writer
}

func (p ProcessBooter) Boot(ctx context.Context, dataDir, seed string) (Booted, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "--port", strconv.Itoa(port), "--data-dir", dataDir, "--log-level", "error"}
	if seed != "" {
		args = append(args, "--seed", seed)
	}
	cmd := exec.CommandContext(ctx, p.Binary, args...)
	cmd.Stdout, cmd.Stderr = p.Stderr, p.Stderr
	cmd.Cancel = func() error { return interruptProcess(cmd.Process) }
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", p.Binary, err)
	}
	return &process{cmd: cmd, endpoint: fmt.Sprintf("http://127.0.0.1:%d", port)}, nil
}

type process struct {
	cmd      *exec.Cmd
	endpoint string
}

func (p *process) Endpoint() string { return p.endpoint }

func (p *process) Stop() error {
	if err := interruptProcess(p.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	err := p.cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && !exitErr.Success() && exitErr.ExitCode() < 0 {
		return nil // ended by the signal before installing its handler
	}
	return err
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// dirSize is the bytes under path, for reporting how big a seed was.
func dirSize(path string) int64 {
	if path == "" {
		return 0
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// WriteBootMarkdown renders boot results as a table.
func WriteBootMarkdown(w io.Writer, runs int, results []BootResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d cold boots per scenario; median (p90).\n\n", runs)
	b.WriteString("| Scenario | Snapshot | Seed size | Ready | First seeded request | Shutdown and cleanup |\n")
	b.WriteString("| --- | --- | ---: | ---: | ---: | ---: |\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			r.Scenario, r.Description, sizeLabel(r.SeedBytes), spread(r.Ready), spread(r.Usable), spread(r.Gone))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func spread(samples []time.Duration) string {
	r := Result{Samples: samples}
	return fmt.Sprintf("%s (%s)", ms(r.Median()), ms(r.P90()))
}

func sizeLabel(n int64) string {
	switch {
	case n == 0:
		return "-"
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
}

// PollReady waits for Stratus's control API to report ready, polling every
// interval. Boot timings need a short interval: ci.WaitReady polls every
// 100 ms, which would round every boot up to the next poll.
func PollReady(interval time.Duration) ci.ReadyWaiter {
	return func(ctx context.Context, endpoint string) error {
		c, err := client.New(endpoint, nil)
		if err != nil {
			return err
		}
		return c.WaitReady(ctx, interval)
	}
}
