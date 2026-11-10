package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/stratus/internal/backing"
)

// Backing runs service engines (databases, caches) as Docker containers. It is
// the Docker adapter for the backing.Runner port.
//
// Every container carries the owner labels, so the orphan reaper removes
// engines left behind by a crashed Stratus, plus a kind and resource label so
// an engine can be found again by the service that owns it.
type Backing struct {
	logger *slog.Logger
	client *dockerclient.Client
	owner  Owner

	mu    sync.Mutex
	kinds map[string]struct{}
}

// NewBacking connects to Docker for engine containers owned by dataDir's
// Stratus instance. Docker may be unreachable; calls then fail with
// backing.ErrUnavailable until it comes back.
func NewBacking(dataDir string, logger *slog.Logger) (*Backing, error) {
	if logger == nil {
		logger = slog.Default()
	}
	client, err := newDockerClient()
	if err != nil {
		return nil, err
	}
	return &Backing{
		logger: logger,
		client: client,
		owner:  NewOwner(dataDir),
		kinds:  make(map[string]struct{}),
	}, nil
}

var _ backing.Runner = (*Backing)(nil)

// Start runs the engine and waits until its probe passes. A stale container
// with the same kind and name is replaced first.
func (b *Backing) Start(ctx context.Context, spec backing.Spec) (backing.Endpoint, error) {
	if spec.Kind == "" || spec.Name == "" || spec.Image == "" || spec.Port <= 0 {
		return backing.Endpoint{}, fmt.Errorf("backing: spec needs kind, name, image and port")
	}
	if spec.Seed != "" && spec.Data == nil {
		return backing.Endpoint{}, fmt.Errorf("backing: engine %s has a seed but no data path", spec.Name)
	}
	if err := b.ensureImage(ctx, spec.Image); err != nil {
		return backing.Endpoint{}, err
	}
	if err := b.Stop(ctx, spec.Kind, spec.Name); err != nil {
		return backing.Endpoint{}, err
	}

	containerPort := nat.Port(fmt.Sprintf("%d/tcp", spec.Port))
	hostPort := ""
	if spec.HostPort > 0 {
		hostPort = strconv.Itoa(spec.HostPort)
	}
	labels := b.owner.Labels()
	labels[LabelKind] = spec.Kind
	labels[LabelResource] = spec.Name
	cfg := &container.Config{
		Image:        spec.Image,
		Env:          spec.Env,
		Cmd:          spec.Cmd,
		ExposedPorts: nat.PortSet{containerPort: struct{}{}},
		Labels:       labels,
	}
	hostCfg := &container.HostConfig{
		PortBindings: nat.PortMap{
			containerPort: []nat.PortBinding{{HostIP: loopback, HostPort: hostPort}},
		},
	}

	name := b.owner.ContainerNameFor(spec.Kind, spec.Name)
	resp, err := b.client.ContainerCreate(ctx, cfg, hostCfg, &network.NetworkingConfig{}, &ocispec.Platform{}, name)
	if err != nil {
		return backing.Endpoint{}, dockerError("create container", err)
	}
	b.remember(spec.Kind)

	if err := b.seed(ctx, resp.ID, spec); err != nil {
		b.remove(resp.ID)
		return backing.Endpoint{}, err
	}
	if err := b.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		b.remove(resp.ID)
		return backing.Endpoint{}, dockerError("start container", err)
	}
	inspect, err := b.client.ContainerInspect(ctx, resp.ID)
	if err != nil {
		b.remove(resp.ID)
		return backing.Endpoint{}, dockerError("inspect container", err)
	}
	port, err := publishedPort(inspect.NetworkSettings.Ports[containerPort])
	if err != nil {
		b.remove(resp.ID)
		return backing.Endpoint{}, err
	}

	endpoint := backing.Endpoint{Address: loopback, Port: port}
	if err := b.waitReady(ctx, resp.ID, spec, endpoint); err != nil {
		b.remove(resp.ID)
		return backing.Endpoint{}, err
	}
	b.logger.Info("engine started", "kind", spec.Kind, "name", spec.Name, "image", spec.Image, "endpoint", endpoint.String())
	return endpoint, nil
}

// seed loads a snapshot's data into a created container before it starts, so
// the engine boots on that data instead of initialising an empty directory.
// The archive holds the data directory itself, so it unpacks into its parent.
// Entries keep their recorded owners; the engine images also fix ownership of
// their data directory on start.
func (b *Backing) seed(ctx context.Context, id string, spec backing.Spec) error {
	if spec.Seed == "" {
		return nil
	}
	archive, err := backing.OpenArchive(spec.Seed)
	if err != nil {
		return fmt.Errorf("backing: open seed for %s: %w", spec.Name, err)
	}
	defer archive.Close()
	parent := path.Dir(spec.Data.Path)
	if err := b.client.CopyToContainer(ctx, id, parent, archive, container.CopyToContainerOptions{}); err != nil {
		return dockerError("load seed data", err)
	}
	return nil
}

// Export copies the engine's data directory into the archive dst. The engine
// first flushes in-memory state, then is paused for the copy, so the archive
// is a crash-consistent point-in-time image that the engine recovers from on
// its next start, the same way it would after a power loss.
func (b *Backing) Export(ctx context.Context, kind, name string, data backing.Data, dst string) error {
	running, err := b.runningContainer(ctx, kind, name)
	if err != nil {
		return err
	}
	if data.Flush != nil {
		endpoint, err := endpointOf(running)
		if err != nil {
			return err
		}
		flushCtx, cancel := context.WithTimeout(ctx, exportFlushTimeout)
		err = data.Flush(flushCtx, endpoint.String())
		cancel()
		if err != nil {
			return fmt.Errorf("backing: flush %s before export: %w", name, err)
		}
	}

	if err := b.client.ContainerPause(ctx, running.ID); err != nil {
		return dockerError("pause container", err)
	}
	copyErr := b.copyOut(ctx, running.ID, data, dst)
	// The pause must end even if the caller gave up, or the engine would
	// stay frozen for every client.
	unpauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := b.client.ContainerUnpause(unpauseCtx, running.ID); err != nil && copyErr == nil {
		return dockerError("unpause container", err)
	}
	return copyErr
}

// copyOut reads the data directory to the end before returning: Docker holds
// the container until the stream is drained, and the pause must cover it all.
func (b *Backing) copyOut(ctx context.Context, id string, data backing.Data, dst string) error {
	stream, _, err := b.client.CopyFromContainer(ctx, id, data.Path)
	if err != nil {
		return dockerError("copy engine data", err)
	}
	defer stream.Close()
	if err := backing.WriteArchive(dst, stream, data.Exclude); err != nil {
		return fmt.Errorf("backing: archive engine data: %w", err)
	}
	return nil
}

func (b *Backing) runningContainer(ctx context.Context, kind, name string) (container.Summary, error) {
	found, err := b.find(ctx, kind, name)
	if err != nil {
		return container.Summary{}, err
	}
	for _, c := range found {
		if c.State == container.StateRunning {
			return c, nil
		}
	}
	return container.Summary{}, fmt.Errorf("%w: %s/%s", backing.ErrNotRunning, kind, name)
}

// endpointOf finds the loopback port an engine container publishes.
func endpointOf(c container.Summary) (backing.Endpoint, error) {
	for _, p := range c.Ports {
		if p.PublicPort > 0 {
			return backing.Endpoint{Address: loopback, Port: int(p.PublicPort)}, nil
		}
	}
	return backing.Endpoint{}, errors.New("backing: container does not publish a port")
}

// exportFlushTimeout bounds an engine's flush to disk, which for a large
// in-memory dataset can take a while.
const exportFlushTimeout = 2 * time.Minute

// Stop removes the engine if it exists.
func (b *Backing) Stop(ctx context.Context, kind, name string) error {
	found, err := b.find(ctx, kind, name)
	if err != nil {
		return err
	}
	for _, c := range found {
		if err := b.removeByID(ctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// Running reports whether the engine's container exists and is running.
func (b *Backing) Running(ctx context.Context, kind, name string) (bool, error) {
	found, err := b.find(ctx, kind, name)
	if err != nil {
		return false, err
	}
	for _, c := range found {
		if c.State == container.StateRunning {
			return true, nil
		}
	}
	return false, nil
}

// StopAll removes every engine of a kind owned by this instance.
func (b *Backing) StopAll(ctx context.Context, kind string) error {
	found, err := b.list(ctx, filters.Arg("label", LabelKind+"="+kind))
	if err != nil {
		return err
	}
	var firstErr error
	for _, c := range found {
		if err := b.removeByID(ctx, c.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Ping reports whether Docker is reachable.
func (b *Backing) Ping(ctx context.Context) error {
	if _, err := b.client.Ping(ctx); err != nil {
		return fmt.Errorf("%w: %v", backing.ErrUnavailable, err)
	}
	return nil
}

// Close removes every engine this process started, so a shutdown leaves no
// containers behind.
func (b *Backing) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b.mu.Lock()
	kinds := make([]string, 0, len(b.kinds))
	for kind := range b.kinds {
		kinds = append(kinds, kind)
	}
	b.mu.Unlock()

	var firstErr error
	for _, kind := range kinds {
		err := b.StopAll(ctx, kind)
		if err != nil && errors.Is(err, backing.ErrUnavailable) {
			// Nothing reachable to stop; the next start reaps what is left.
			return nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *Backing) remember(kind string) {
	b.mu.Lock()
	b.kinds[kind] = struct{}{}
	b.mu.Unlock()
}

func (b *Backing) ensureImage(ctx context.Context, image string) error {
	_, _, err := b.client.ImageInspectWithRaw(ctx, image)
	switch {
	case err == nil:
		return nil
	case dockerclient.IsErrNotFound(err):
		return &backing.ImageMissingError{Image: image}
	default:
		return dockerError("inspect image", err)
	}
}

func (b *Backing) waitReady(ctx context.Context, id string, spec backing.Spec, endpoint backing.Endpoint) error {
	probe := spec.Probe
	if probe == nil {
		probe = backing.TCP
	}
	timeout := spec.ReadyTimeout
	if timeout <= 0 {
		timeout = backing.DefaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := probe(probeCtx, endpoint.String())
		cancel()
		if err == nil {
			return nil
		}
		if exited, reason := b.exited(ctx, id); exited {
			return &backing.NotReadyError{Name: spec.Name, Timeout: timeout, Logs: reason}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return &backing.NotReadyError{Name: spec.Name, Timeout: timeout, Logs: b.logsTail(ctx, id)}
}

// exited reports whether the container stopped on its own, and why, so a
// misconfigured engine fails fast instead of waiting out the timeout.
func (b *Backing) exited(ctx context.Context, id string) (bool, string) {
	inspect, err := b.client.ContainerInspect(ctx, id)
	if err != nil || inspect.State == nil || inspect.State.Running {
		return false, ""
	}
	return true, fmt.Sprintf("exited with code %d; %s", inspect.State.ExitCode, b.logsTail(ctx, id))
}

func (b *Backing) logsTail(ctx context.Context, id string) string {
	logCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	reader, err := b.client.ContainerLogs(logCtx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "15"})
	if err != nil {
		return ""
	}
	defer reader.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, reader); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

func (b *Backing) find(ctx context.Context, kind, name string) ([]container.Summary, error) {
	return b.list(ctx, filters.Arg("label", LabelKind+"="+kind), filters.Arg("label", LabelResource+"="+name))
}

func (b *Backing) list(ctx context.Context, extra ...filters.KeyValuePair) ([]container.Summary, error) {
	args := []filters.KeyValuePair{
		filters.Arg("label", LabelManaged+"=true"),
		filters.Arg("label", LabelInstance+"="+b.owner.Instance),
	}
	args = append(args, extra...)
	found, err := b.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(args...)})
	if err != nil {
		return nil, dockerError("list containers", err)
	}
	return found, nil
}

func (b *Backing) removeByID(ctx context.Context, id string) error {
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = b.client.ContainerStop(stopCtx, id, container.StopOptions{Timeout: intPtr(2)})
	// Engine images declare their data directory as a volume. Remove it with
	// the container, or every engine ever started leaves a volume behind.
	err := b.client.ContainerRemove(stopCtx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !dockerclient.IsErrNotFound(err) {
		return dockerError("remove container", err)
	}
	return nil
}

// remove is best-effort cleanup after a failed start.
func (b *Backing) remove(id string) {
	_ = b.removeByID(context.Background(), id)
}

func publishedPort(bindings []nat.PortBinding) (int, error) {
	if len(bindings) == 0 {
		return 0, errors.New("backing: container did not publish a host port")
	}
	port, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("backing: invalid published port %q", bindings[0].HostPort)
	}
	return port, nil
}

// dockerError keeps a lost daemon distinguishable from every other failure.
func dockerError(action string, err error) error {
	if dockerclient.IsErrConnectionFailed(err) {
		return fmt.Errorf("%w: %s: %v", backing.ErrUnavailable, action, err)
	}
	return fmt.Errorf("backing: %s: %w", action, err)
}

const loopback = "127.0.0.1"
