package bench

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock advances by a fixed step every time it is read.
type fakeClock struct {
	now  time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time { c.now = c.now.Add(c.step); return c.now }

type fakeBooted struct {
	stops   *int
	stopErr error
}

func (b fakeBooted) Endpoint() string { return "http://127.0.0.1:1" }
func (b fakeBooted) Stop() error      { *b.stops++; return b.stopErr }

type fakeBooter struct {
	seeds []string
	dirs  []string
	stops int
	err   error
}

func (f *fakeBooter) Boot(_ context.Context, dir, seed string) (Booted, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.dirs = append(f.dirs, dir)
	f.seeds = append(f.seeds, seed)
	return fakeBooted{stops: &f.stops}, nil
}

func fakeBootHost(b *fakeBooter, removed *[]string, ready error) BootHost {
	n := 0
	clock := &fakeClock{now: time.Unix(0, 0), step: 10 * time.Millisecond}
	return BootHost{
		Booter: b,
		Ready:  func(context.Context, string) error { return ready },
		TempDir: func() (string, error) {
			n++
			return "dir-" + string(rune('0'+n)), nil
		},
		Remove: func(p string) error { *removed = append(*removed, p); return nil },
		Now:    clock.Now,
	}
}

func TestMeasureBootTimesEveryColdBootAndCleansUp(t *testing.T) {
	booter := &fakeBooter{}
	var removed []string
	verified := 0
	s := BootScenario{Name: "s", Verify: func(context.Context, string) error { verified++; return nil }}

	res, err := MeasureBoot(context.Background(), fakeBootHost(booter, &removed, nil), s, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	// Clock reads per run: start, ready, usable, stop-at, gone; 10 ms apart.
	for name, samples := range map[string][]time.Duration{"ready": res.Ready, "usable": res.Usable, "gone": res.Gone} {
		if len(samples) != 3 {
			t.Fatalf("%s samples = %v", name, samples)
		}
	}
	if res.Ready[0] != 10*time.Millisecond || res.Usable[0] != 20*time.Millisecond || res.Gone[0] != 10*time.Millisecond {
		t.Fatalf("got ready %v usable %v gone %v", res.Ready, res.Usable, res.Gone)
	}
	if verified != 3 || booter.stops != 3 {
		t.Fatalf("verified %d, stopped %d", verified, booter.stops)
	}
	if strings.Join(removed, ",") != "dir-1,dir-2,dir-3" || strings.Join(booter.dirs, ",") != "dir-1,dir-2,dir-3" {
		t.Fatalf("each boot needs its own fresh data dir: booted %v removed %v", booter.dirs, removed)
	}
}

func TestMeasureBootFailsWhenTheSeedIsNotServed(t *testing.T) {
	booter := &fakeBooter{}
	var removed []string
	s := BootScenario{Name: "s", Verify: func(context.Context, string) error { return errors.New("bucket missing") }}

	_, err := MeasureBoot(context.Background(), fakeBootHost(booter, &removed, nil), s, "seed", 2)
	if err == nil || !strings.Contains(err.Error(), "bucket missing") {
		t.Fatalf("err = %v", err)
	}
	if booter.stops != 1 || len(removed) != 1 {
		t.Fatalf("a failed boot must still be stopped and removed: stops %d removed %v", booter.stops, removed)
	}
}

func TestMeasureBootStopsABootThatNeverBecomesReady(t *testing.T) {
	booter := &fakeBooter{}
	var removed []string
	_, err := MeasureBoot(context.Background(), fakeBootHost(booter, &removed, errors.New("timeout")), BootScenario{Name: "s"}, "", 1)
	if err == nil || booter.stops != 1 || len(removed) != 1 {
		t.Fatalf("err %v stops %d removed %v", err, booter.stops, removed)
	}
}

func TestMeasureBootPassesTheSeed(t *testing.T) {
	booter := &fakeBooter{}
	var removed []string
	if _, err := MeasureBoot(context.Background(), fakeBootHost(booter, &removed, nil), BootScenario{Name: "s"}, "/snap", 1); err != nil {
		t.Fatal(err)
	}
	if booter.seeds[0] != "/snap" {
		t.Fatalf("seed = %q", booter.seeds[0])
	}
}

func TestPrepareSeedWithoutPrepareBootsUnseeded(t *testing.T) {
	seed, dir, err := PrepareSeed(context.Background(), Host{}, BootScenario{Name: "empty"}, "boot")
	if err != nil || seed != "" || dir != "" {
		t.Fatalf("seed %q dir %q err %v", seed, dir, err)
	}
}

func TestBootScenariosAddTheEngineOnlyWhenAsked(t *testing.T) {
	if n := len(BootScenarios(tinyFixture(), false)); n != 2 {
		t.Fatalf("scenarios without engine = %d", n)
	}
	s := BootScenarios(tinyFixture(), true)
	if len(s) != 3 || !strings.Contains(s[2].Name, "RDS") {
		t.Fatalf("scenarios with engine = %+v", s)
	}
}

func TestWriteBootMarkdown(t *testing.T) {
	var buf bytes.Buffer
	err := WriteBootMarkdown(&buf, 2, []BootResult{{
		Scenario: "fixture snapshot", Description: "d", SeedBytes: 7 << 20,
		Ready:  []time.Duration{16 * time.Millisecond, 18 * time.Millisecond},
		Usable: []time.Duration{32 * time.Millisecond, 35 * time.Millisecond},
		Gone:   []time.Duration{time.Millisecond, time.Millisecond},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "| fixture snapshot | d | 7.0 MiB | 16.0 ms (18.0 ms) | 32.0 ms (35.0 ms) | 1.0 ms (1.0 ms) |"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("missing %q in:\n%s", want, buf.String())
	}
}

// The seed for a real boot comes from an in-process Stratus: check that the
// snapshot it writes is what a cold boot restores.
func TestPrepareSeedWritesABootableSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Stratus")
	}
	host := realHost(t)
	s := BootScenarios(tinyFixture(), false)[1]
	seed, dir, err := PrepareSeed(context.Background(), host, s, "boot")
	if err != nil {
		t.Fatal(err)
	}
	if seed == "" || dir == "" || dirSize(seed) == 0 {
		t.Fatalf("seed %q dir %q size %d", seed, dir, dirSize(seed))
	}
	inst, err := host.launch(context.Background(), seed)
	if err != nil {
		t.Fatal(err)
	}
	defer host.stop(inst)
	if err := s.Verify(context.Background(), inst.server.Endpoint()); err != nil {
		t.Fatalf("a boot from the prepared seed does not serve the fixture: %v", err)
	}
}
