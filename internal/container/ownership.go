package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// Labels Stratus puts on every container it starts, so it can always find and
// remove its own containers, including ones left behind by a crashed run.
const (
	LabelManaged   = "io.stratus.managed"
	LabelInstance  = "io.stratus.instance"
	LabelOwnerPID  = "io.stratus.owner-pid"
	LabelOwnerHost = "io.stratus.owner-host"
	// LabelKind and LabelResource say what a container runs: the service that
	// owns it ("lambda", "rds") and the resource within it (a function name,
	// a DB instance identifier).
	LabelKind     = "io.stratus.kind"
	LabelResource = "io.stratus.resource"
)

// Owner identifies the Stratus process that owns a container.
//
// Instance is derived from the data directory. Only one process can hold a
// data directory at a time (the metadata database is file-locked), so any
// container labelled with our instance that we did not start ourselves is
// necessarily left over from an earlier process and safe to remove.
type Owner struct {
	Instance string
	PID      int
	Host     string
}

// NewOwner derives the owner identity for this process and data directory.
func NewOwner(dataDir string) Owner {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(abs))
	host, _ := os.Hostname()
	return Owner{Instance: hex.EncodeToString(sum[:])[:10], PID: os.Getpid(), Host: host}
}

// Labels returns the labels to put on a container owned by o.
func (o Owner) Labels() map[string]string {
	return map[string]string{
		LabelManaged:   "true",
		LabelInstance:  o.Instance,
		LabelOwnerPID:  strconv.Itoa(o.PID),
		LabelOwnerHost: o.Host,
	}
}

// ContainerName returns a Docker container name for a function that is
// unique per Stratus instance, so parallel instances never collide.
func (o Owner) ContainerName(functionName string) string {
	return fmt.Sprintf("stratus-%s-%s", o.Instance, sanitizeName(functionName))
}

// ContainerNameFor names an engine container by the service that owns it and
// the resource it backs, unique per Stratus instance.
func (o Owner) ContainerNameFor(kind, resource string) string {
	return fmt.Sprintf("stratus-%s-%s-%s", o.Instance, sanitizeName(kind), sanitizeName(resource))
}

// ProcessAlive reports whether a process with the given PID exists.
type ProcessAlive func(pid int) bool

// IsOrphan decides whether a container labelled with labels should be
// removed by self. It removes:
//   - containers from an earlier process that used the same data directory;
//   - containers whose owning process ran on this host and no longer exists.
//
// Containers owned by a live process, or by a process on another host
// (a shared remote Docker daemon), are left alone.
func IsOrphan(labels map[string]string, self Owner, alive ProcessAlive) bool {
	if labels[LabelManaged] != "true" {
		return false
	}
	pid, err := strconv.Atoi(labels[LabelOwnerPID])
	sameProcess := err == nil && pid == self.PID && labels[LabelOwnerHost] == self.Host
	if labels[LabelInstance] == self.Instance {
		return !sameProcess
	}
	if labels[LabelOwnerHost] != self.Host || err != nil || pid <= 0 {
		return false
	}
	return pid != self.PID && !alive(pid)
}

// containerAPI is the part of the Docker client the reaper needs.
type containerAPI interface {
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
}

// ReapOrphans removes containers left behind by crashed or killed Stratus
// processes and returns how many it removed.
func ReapOrphans(ctx context.Context, api containerAPI, self Owner, alive ProcessAlive) (int, error) {
	list, err := api.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelManaged+"=true")),
	})
	if err != nil {
		return 0, fmt.Errorf("list stratus containers: %w", err)
	}
	removed := 0
	var firstErr error
	for _, c := range list {
		if !IsOrphan(c.Labels, self, alive) {
			continue
		}
		if err := api.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove orphan container %s: %w", c.ID, err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
