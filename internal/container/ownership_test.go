package container

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

var self = Owner{Instance: "aaaaaaaaaa", PID: 100, Host: "ci-runner"}

func labels(instance string, pid int, host string) map[string]string {
	return map[string]string{
		LabelManaged:   "true",
		LabelInstance:  instance,
		LabelOwnerPID:  strconv.Itoa(pid),
		LabelOwnerHost: host,
	}
}

func aliveSet(pids ...int) ProcessAlive {
	return func(pid int) bool {
		for _, p := range pids {
			if p == pid {
				return true
			}
		}
		return false
	}
}

func TestNewOwnerIsStablePerDataDirAndDistinctAcrossDirs(t *testing.T) {
	a1, a2, b := NewOwner("/tmp/run-a"), NewOwner("/tmp/run-a"), NewOwner("/tmp/run-b")
	if a1.Instance != a2.Instance {
		t.Fatal("same data dir must give the same instance")
	}
	if a1.Instance == b.Instance {
		t.Fatal("different data dirs must give different instances")
	}
	if len(a1.Instance) != 10 {
		t.Fatalf("instance length = %d", len(a1.Instance))
	}
}

func TestContainerNameIsUniquePerInstanceAndSanitised(t *testing.T) {
	a := Owner{Instance: "aaaaaaaaaa"}
	b := Owner{Instance: "bbbbbbbbbb"}
	if a.ContainerName("Orders.Fn") == b.ContainerName("Orders.Fn") {
		t.Fatal("parallel instances must not share container names")
	}
	if got := a.ContainerName("Orders.Fn"); got != "stratus-aaaaaaaaaa-orders-fn" {
		t.Fatalf("got %q", got)
	}
	long := a.ContainerName(strings.Repeat("x", 100))
	if len(long) > len("stratus-aaaaaaaaaa-")+48 {
		t.Fatalf("name too long: %d", len(long))
	}
}

func TestLabelsRoundTripThroughIsOrphan(t *testing.T) {
	if IsOrphan(self.Labels(), self, aliveSet()) {
		t.Fatal("a container we own right now is never an orphan")
	}
}

func TestIsOrphan(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		alive  ProcessAlive
		want   bool
	}{
		{"unmanaged container", map[string]string{"foo": "bar"}, aliveSet(), false},
		{"same data dir, earlier process", labels(self.Instance, 42, "ci-runner"), aliveSet(42), true},
		{"same data dir, other host", labels(self.Instance, 42, "laptop"), aliveSet(), true},
		{"same process", labels(self.Instance, 100, "ci-runner"), aliveSet(), false},
		{"other instance, dead owner on this host", labels("bbbbbbbbbb", 42, "ci-runner"), aliveSet(), true},
		{"other instance, live owner on this host", labels("bbbbbbbbbb", 42, "ci-runner"), aliveSet(42), false},
		{"other instance, other host", labels("bbbbbbbbbb", 42, "laptop"), aliveSet(), false},
		{"other instance, bad pid", map[string]string{LabelManaged: "true", LabelInstance: "b", LabelOwnerPID: "x", LabelOwnerHost: "ci-runner"}, aliveSet(), false},
	}
	for _, tc := range cases {
		if got := IsOrphan(tc.labels, self, tc.alive); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

type fakeDocker struct {
	list      []container.Summary
	removed   []string
	listErr   error
	removeErr map[string]error
	filter    string
}

func (f *fakeDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	f.filter = strings.Join(opts.Filters.Get("label"), ",")
	return f.list, f.listErr
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, opts container.RemoveOptions) error {
	if !opts.Force {
		return errors.New("expected forced removal")
	}
	if err := f.removeErr[id]; err != nil {
		return err
	}
	f.removed = append(f.removed, id)
	return nil
}

func TestReapOrphansRemovesOnlyOrphans(t *testing.T) {
	docker := &fakeDocker{list: []container.Summary{
		{ID: "mine", Labels: self.Labels()},
		{ID: "stale-same-dir", Labels: labels(self.Instance, 7, "ci-runner")},
		{ID: "dead-peer", Labels: labels("bbbbbbbbbb", 8, "ci-runner")},
		{ID: "live-peer", Labels: labels("cccccccccc", 9, "ci-runner")},
	}}
	n, err := ReapOrphans(context.Background(), docker, self, aliveSet(9))
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	sort.Strings(docker.removed)
	if want := []string{"dead-peer", "stale-same-dir"}; n != 2 || !reflect.DeepEqual(docker.removed, want) {
		t.Fatalf("removed %v (%d), want %v", docker.removed, n, want)
	}
	if docker.filter != LabelManaged+"=true" {
		t.Fatalf("list filter = %q", docker.filter)
	}
}

func TestReapOrphansContinuesPastRemoveErrors(t *testing.T) {
	docker := &fakeDocker{
		list: []container.Summary{
			{ID: "a", Labels: labels(self.Instance, 1, "ci-runner")},
			{ID: "b", Labels: labels(self.Instance, 2, "ci-runner")},
		},
		removeErr: map[string]error{"a": errors.New("busy")},
	}
	n, err := ReapOrphans(context.Background(), docker, self, aliveSet())
	if err == nil || n != 1 || !reflect.DeepEqual(docker.removed, []string{"b"}) {
		t.Fatalf("n=%d err=%v removed=%v", n, err, docker.removed)
	}
}

func TestReapOrphansReportsListErrors(t *testing.T) {
	if _, err := ReapOrphans(context.Background(), &fakeDocker{listErr: errors.New("no docker")}, self, aliveSet()); err == nil {
		t.Fatal("expected error")
	}
}

func TestProcessAliveForSelfAndImpossiblePID(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("current process should be alive")
	}
	if processAlive(1 << 30) {
		t.Fatal("absurd pid should not be alive")
	}
}
