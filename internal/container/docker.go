package container

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

const (
	// initAllowance is added to the function timeout for the HTTP call to
	// the emulator, which enforces the timeout itself; it covers cold start.
	initAllowance = 10 * time.Second
	// logSettleTimeout is how long to wait for an invocation's REPORT line
	// to reach the container log after the response.
	logSettleTimeout = time.Second
)

// Manager runs Lambda functions in Docker containers built from AWS's Lambda
// base images (or the function's own image), each fronted by the Runtime
// Interface Emulator. Containers stay warm between invocations.
type Manager struct {
	logger     *slog.Logger
	client     *dockerclient.Client
	blobRoot   string
	runtimeDir string
	catalog    RuntimeCatalog
	pool       *WarmPool
	httpClient *http.Client
	owner      Owner
	reach      Reach
	files      FileDelivery

	rieMu   sync.Mutex
	rieHost string

	credentials RoleCredentialIssuer
	endpoint    string
}

// RoleCredentialIssuer issues a function's execution role credentials.
type RoleCredentialIssuer interface {
	RoleCredentials(account, roleARN, sessionName string) (accessKeyID, secretAccessKey, sessionToken string, err error)
}

// SetCredentialIssuer makes new containers start with their execution
// role's session credentials, as in Lambda.
func (m *Manager) SetCredentialIssuer(issuer RoleCredentialIssuer) {
	m.credentials = issuer
}

// SetEndpoint gives every new container Stratus's URL as containers see
// it, in AWS_ENDPOINT_URL and STRATUS_ENDPOINT, unless the function sets
// those variables itself.
func (m *Manager) SetEndpoint(endpoint string) {
	m.endpoint = endpoint
}

// NewManager runs functions on daemon for dataDir's Stratus instance.
func NewManager(daemon *Daemon, dataDir, blobRoot string, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	runtimeDir := filepath.Join(dataDir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create runtime dir: %w", err)
	}

	m := &Manager{
		logger:     logger,
		client:     daemon.client,
		blobRoot:   blobRoot,
		runtimeDir: runtimeDir,
		catalog:    NewRuntimeCatalog(os.LookupEnv),
		pool:       NewWarmPool(5 * time.Minute),
		// Invocations set their own deadline from the function timeout.
		httpClient: &http.Client{},
		owner:      daemon.owner,
		reach:      daemon.Reach(),
		files:      daemon.Files(),
	}
	m.reapOrphans()
	return m, nil
}

// reapOrphans removes containers left behind by earlier Stratus processes.
// Docker may be unavailable at startup; that is not fatal, since Lambda is
// the only feature that needs it.
func (m *Manager) reapOrphans() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	removed, err := ReapOrphans(ctx, m.client, m.owner, processAlive)
	if err != nil {
		m.logger.Debug("orphan container cleanup skipped", "error", err)
	}
	if removed > 0 {
		m.logger.Info("removed orphaned stratus containers", "count", removed)
	}
}

func (m *Manager) Close() error {
	return m.pool.CloseAll(context.Background(), m.removeContainer)
}

// SetMaxContainers caps how many warm containers one function may have.
func (m *Manager) SetMaxContainers(n int) { m.pool.SetMax(n) }

// PoolStats reports how hard the warm pool is working.
func (m *Manager) PoolStats() PoolStats { return m.pool.Stats() }

// Reset stops every warm Lambda container. The next invocation of each
// function starts a fresh container, so restored or reset state is picked up.
func (m *Manager) Reset(ctx context.Context) error {
	return m.pool.CloseAll(ctx, m.removeContainer)
}

// Ping reports whether the Docker daemon used for Lambda runtimes is reachable.
func (m *Manager) Ping(ctx context.Context) error {
	if _, err := m.client.Ping(ctx); err != nil {
		return fmt.Errorf("docker daemon unreachable: %w", err)
	}
	return nil
}

func (m *Manager) CleanupFunction(ctx context.Context, functionName string) error {
	return m.pool.Remove(ctx, functionName, m.removeContainer)
}

func (m *Manager) Invoke(ctx context.Context, spec lambdasvc.FunctionSpec, payload []byte) (lambdasvc.InvokeResult, error) {
	rt, imageName, err := m.imageFor(spec)
	if err != nil {
		return lambdasvc.InvokeResult{}, err
	}
	if err := m.ensureImage(ctx, imageName); err != nil {
		return lambdasvc.InvokeResult{}, err
	}

	if err := m.pool.ExpireIdle(ctx, m.removeContainer); err != nil {
		m.logger.Warn("warm pool expiration failed", "error", err)
	}

	warm, err := m.pool.Acquire(ctx, runtimeKey(spec), func(ctx context.Context, slot int) (*WarmContainer, error) {
		return m.startContainer(ctx, spec, rt, imageName, slot)
	}, func(ctx context.Context, existing *WarmContainer) (bool, error) {
		return m.isHealthy(ctx, existing)
	}, m.removeContainer)
	if err != nil {
		return lambdasvc.InvokeResult{}, err
	}

	result, err := m.invokeRIE(ctx, warm, spec, payload)
	if err == nil {
		m.pool.Release(warm)
		return result, nil
	}

	var timedOut errRuntimeTimedOut
	if errors.As(err, &timedOut) {
		// Lambda discards the execution environment after a timeout.
		_ = m.pool.Discard(context.Background(), warm, m.removeContainer)
		return lambdasvc.InvokeResult{Payload: timedOut.payload, FunctionError: functionErrorUnhandled}, nil
	}

	_ = m.pool.Discard(context.Background(), warm, m.removeContainer)
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		return lambdasvc.InvokeResult{}, err
	}
	return lambdasvc.InvokeResult{}, serviceError(http.StatusInternalServerError, err.Error())
}

// imageFor resolves the image a function runs in: the runtime's base image
// for zip functions, the function's own for container-image functions.
func (m *Manager) imageFor(spec lambdasvc.FunctionSpec) (Runtime, string, error) {
	if spec.PackageType == lambdasvc.PackageTypeImage {
		if spec.ImageURI == "" {
			return Runtime{}, "", serviceError(http.StatusBadRequest, "container-image function has no ImageUri")
		}
		return Runtime{}, spec.ImageURI, nil
	}
	return m.catalog.Resolve(spec.Runtime)
}

// ensureImage checks the daemon has imageName. Stratus never pulls, so runs
// stay deterministic and work offline; the error names the image to pull.
func (m *Manager) ensureImage(ctx context.Context, imageName string) error {
	_, err := m.client.ImageInspect(ctx, imageName)
	if err == nil {
		return nil
	}
	if dockerclient.IsErrNotFound(err) {
		return serviceError(http.StatusServiceUnavailable,
			"lambda image is not available locally: "+imageName+" (run: docker pull "+imageName+")")
	}
	return serviceError(http.StatusServiceUnavailable, "docker image inspection failed: "+err.Error())
}

func (m *Manager) startContainer(ctx context.Context, spec lambdasvc.FunctionSpec, rt Runtime, imageName string, slot int) (*WarmContainer, error) {
	containerName := m.owner.ContainerName(runtimeKey(spec))
	if slot > 0 {
		// The first container keeps the name earlier versions used.
		containerName = fmt.Sprintf("%s-%d", containerName, slot)
	}
	_ = m.removeNamedContainer(ctx, containerName)

	spec.Credentials = m.roleCredentials(spec)
	in, err := m.launchInput(ctx, spec, rt, imageName)
	if err != nil {
		return nil, err
	}
	if needsMountedRIE(in) {
		if in.RIEHostPath, err = m.ensureRIE(ctx); err != nil {
			return nil, err
		}
	}
	plan, err := planLaunch(in)
	if err != nil {
		return nil, err
	}
	mounts, copies := plan.Mounts, []mount.Mount(nil)
	if m.files == DeliverCopy {
		mounts, copies = splitBinds(plan.Mounts)
	}

	labels := m.owner.Labels()
	labels[LabelKind] = "lambda"
	labels[LabelResource] = runtimeKey(spec)
	cfg := &container.Config{
		Image:        plan.Image,
		Entrypoint:   plan.Entrypoint,
		Cmd:          plan.Cmd,
		Env:          plan.Env,
		WorkingDir:   plan.WorkingDir,
		ExposedPorts: nat.PortSet{nat.Port(rieListenPort): struct{}{}},
		Labels:       labels,
	}
	hostCfg := &container.HostConfig{
		Mounts:       mounts,
		PortBindings: m.reach.PortBindings(nat.Port(rieListenPort), ""),
		ExtraHosts:   runtimeExtraHosts(plan.Env),
	}
	if spec.MemorySize > 0 {
		hostCfg.Memory = int64(spec.MemorySize) * 1024 * 1024
	}

	resp, err := m.client.ContainerCreate(ctx, cfg, hostCfg, m.reach.NetworkingConfig(), &ocispec.Platform{}, containerName)
	if err != nil {
		return nil, serviceError(http.StatusServiceUnavailable, "docker container create failed: "+err.Error())
	}
	discard := func() { _ = m.removeContainer(context.Background(), &WarmContainer{ID: resp.ID, Name: containerName}) }

	if err := m.copyIn(ctx, resp.ID, copies); err != nil {
		discard()
		return nil, err
	}

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		discard()
		return nil, serviceError(http.StatusServiceUnavailable, "docker container start failed: "+err.Error())
	}
	address, err := m.emulatorAddress(ctx, resp.ID, containerName)
	if err != nil {
		discard()
		return nil, err
	}

	warm := &WarmContainer{
		ID:        resp.ID,
		Name:      containerName,
		Function:  runtimeKey(spec),
		Slot:      slot,
		Address:   address,
		LastUsed:  time.Now(),
		CreatedAt: time.Now(),
	}
	if err := m.waitReady(ctx, warm); err != nil {
		discard()
		return nil, err
	}

	m.logger.Info("lambda runtime started", "function", spec.FunctionName, "image", plan.Image, "container_id", resp.ID, "address", address)
	return warm, nil
}

// launchInput gathers what planLaunch needs from the host and the image.
func (m *Manager) launchInput(ctx context.Context, spec lambdasvc.FunctionSpec, rt Runtime, imageName string) (launchInput, error) {
	in := launchInput{Spec: spec, Runtime: rt, Image: imageName, Endpoint: m.endpoint}

	if spec.PackageType == lambdasvc.PackageTypeImage {
		inspect, err := m.client.ImageInspect(ctx, imageName)
		if err != nil {
			return launchInput{}, serviceError(http.StatusServiceUnavailable, "docker image inspection failed: "+err.Error())
		}
		if inspect.Config != nil {
			in.Defaults = imageDefaults{
				Entrypoint: inspect.Config.Entrypoint,
				Cmd:        inspect.Config.Cmd,
				WorkingDir: inspect.Config.WorkingDir,
			}
		}
		return in, nil
	}

	codeDir := spec.CodeDir
	if codeDir == "" {
		codeDir = filepath.Join(m.blobRoot, "lambda", spec.FunctionName, "source")
	}
	if _, err := os.Stat(codeDir); err != nil {
		return launchInput{}, serviceError(http.StatusInternalServerError, "lambda code directory is missing: "+err.Error())
	}
	in.CodeDir = codeDir
	in.CodeHasBootstrap = isRegularFile(filepath.Join(codeDir, "bootstrap"))

	if len(spec.LayerDirs) > 0 {
		optDir := filepath.Join(m.runtimeDir, "layers", sanitizeName(runtimeKey(spec)))
		if err := mergeLayers(optDir, spec.LayerDirs); err != nil {
			return launchInput{}, serviceError(http.StatusInternalServerError, err.Error())
		}
		in.OptDir = optDir
	}
	return in, nil
}

// emulatorAddress is where the started container's emulator answers under
// the reach policy.
// On a shared network the container's own address is used rather than its
// name, so a replaced container is never reached through a stale DNS answer.
func (m *Manager) emulatorAddress(ctx context.Context, id, name string) (string, error) {
	inspect, err := m.client.ContainerInspect(ctx, id)
	if err != nil {
		return "", serviceError(http.StatusServiceUnavailable, "docker container inspect failed: "+err.Error())
	}
	if !m.reach.Publishes() {
		if ip := networkIP(inspect, m.reach.Network); ip != "" {
			name = ip
		}
		return m.reach.Endpoint(name, rieContainerPort, 0).String(), nil
	}
	port, err := publishedPort(inspect.NetworkSettings.Ports[nat.Port(rieListenPort)])
	if err != nil {
		return "", serviceError(http.StatusServiceUnavailable, err.Error())
	}
	return m.reach.Endpoint(name, rieContainerPort, port).String(), nil
}

// copyIn delivers bind sources into a created container, for daemons that
// cannot see Stratus's filesystem.
func (m *Manager) copyIn(ctx context.Context, id string, copies []mount.Mount) error {
	if len(copies) == 0 {
		return nil
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(archiveMounts(pw, copies)) }()
	err := m.client.CopyToContainer(ctx, id, "/", pr, container.CopyToContainerOptions{})
	pr.CloseWithError(err)
	if err != nil {
		return serviceError(http.StatusServiceUnavailable, "copy function files into container: "+err.Error())
	}
	return nil
}

// waitReady waits for the emulator to answer HTTP. A TCP connect is not
// enough: Docker's port proxy accepts before the emulator listens.
func (m *Manager) waitReady(ctx context.Context, warm *WarmContainer) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m.emulatorAnswers(ctx, warm) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return serviceError(http.StatusServiceUnavailable, "lambda runtime did not become ready: "+m.tailLogs(warm.ID))
}

// emulatorAnswers reports whether the emulator returned any HTTP response.
func (m *Manager) emulatorAnswers(ctx context.Context, warm *WarmContainer) bool {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, fmt.Sprintf("http://%s/", warm.Address), nil)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func (m *Manager) isHealthy(ctx context.Context, warm *WarmContainer) (bool, error) {
	inspect, err := m.client.ContainerInspect(ctx, warm.ID)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !inspect.State.Running {
		return false, nil
	}
	return m.emulatorAnswers(ctx, warm), nil
}

// invokeRIE runs one invocation through the emulator. The emulator handles
// one invocation at a time, so invocations of a warm container queue.
func (m *Manager) invokeRIE(ctx context.Context, warm *WarmContainer, spec lambdasvc.FunctionSpec, payload []byte) (lambdasvc.InvokeResult, error) {
	warm.invokeMu.Lock()
	defer warm.invokeMu.Unlock()

	timeout := time.Duration(spec.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutS * time.Second
	}
	invokeCtx, cancel := context.WithTimeout(ctx, timeout+initAllowance)
	defer cancel()

	requestID := uuid.NewString()
	req, err := http.NewRequestWithContext(invokeCtx, http.MethodPost,
		fmt.Sprintf("http://%s%s", warm.Address, rieInvokePath), bytes.NewReader(invokePayload(payload)))
	if err != nil {
		return lambdasvc.InvokeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amzn-RequestId", requestID)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		if errors.Is(invokeCtx.Err(), context.DeadlineExceeded) {
			return lambdasvc.InvokeResult{}, errRuntimeTimedOut{payload: timeoutPayload(spec.Timeout)}
		}
		return lambdasvc.InvokeResult{}, serviceError(http.StatusInternalServerError, "lambda runtime exited unexpectedly: "+err.Error())
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return lambdasvc.InvokeResult{}, serviceError(http.StatusInternalServerError, "read lambda runtime response: "+err.Error())
	}

	result, err := rieOutcome{Status: resp.StatusCode, Body: body}.toInvokeResult(spec.Timeout)
	// Read the output even when the invocation failed: it ran, and the next
	// invocation's output must not be mistaken for it.
	result.Logs = m.invocationLogs(warm)
	return result, err
}

// invocationLogs returns the invocation's output from its START to its
// REPORT line, which is what Lambda's Tail log holds. Callers hold
// warm.invokeMu, so the container runs nothing else meanwhile.
func (m *Manager) invocationLogs(warm *WarmContainer) string {
	deadline := time.Now().Add(logSettleTimeout)
	for {
		logs, complete := sliceInvocation(m.containerOutput(warm.ID, time.Time{}), warm.reports)
		if complete || time.Now().After(deadline) {
			// Count the invocation even when its REPORT is late, so the
			// next one does not take this one's lines.
			warm.reports++
			return logs
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *Manager) containerOutput(containerID string, since time.Time) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := container.LogsOptions{ShowStdout: true, ShowStderr: true}
	if !since.IsZero() {
		opts.Since = fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond())
	}
	stream, err := m.client.ContainerLogs(ctx, containerID, opts)
	if err != nil {
		return ""
	}
	defer stream.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, stream); err != nil {
		return out.String()
	}
	return out.String()
}

// tailLogs is the last of a container's output, for start-up errors.
func (m *Manager) tailLogs(containerID string) string {
	out := strings.TrimSpace(m.containerOutput(containerID, time.Time{}))
	if len(out) > 2048 {
		out = out[len(out)-2048:]
	}
	if out == "" {
		return "no container output"
	}
	return out
}

// sliceInvocation cuts one invocation's lines out of a container's output:
// everything after the first `done` REPORT lines, up to and including the
// next one. It reports complete once that REPORT line has been seen.
//
// Invocations are told apart by counting REPORT lines rather than by
// request ID, because emulators disagree on which ID they print: newer ones
// print the caller's on START but their own on END and REPORT, and order
// START and their "(rapid)" diagnostics differently. A warm container runs
// one invocation at a time, so the next REPORT always closes the current
// one. The emulator's "(rapid)" diagnostics are left out, as in Lambda.
func sliceInvocation(output string, done int) (string, bool) {
	var b strings.Builder
	reports := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		isReport := strings.HasPrefix(line, "REPORT RequestId: ")
		if reports < done {
			if isReport {
				reports++
			}
			continue
		}
		if strings.Contains(line, "] (rapid) ") {
			continue
		}
		b.WriteString(line + "\n")
		if isReport {
			return b.String(), true
		}
	}
	return b.String(), false
}

// ensureRIE returns a host copy of the Runtime Interface Emulator, copied
// once out of the RIE source image, for images that do not bundle one.
func (m *Manager) ensureRIE(ctx context.Context) (string, error) {
	m.rieMu.Lock()
	defer m.rieMu.Unlock()
	if m.rieHost != "" {
		return m.rieHost, nil
	}

	dst := filepath.Join(m.runtimeDir, "rie", "aws-lambda-rie")
	if isRegularFile(dst) {
		m.rieHost = dst
		return dst, nil
	}

	source := m.catalog.RIEImage()
	if err := m.ensureImage(ctx, source); err != nil {
		return "", err
	}
	labels := m.owner.Labels()
	labels[LabelKind] = "lambda-rie"
	created, err := m.client.ContainerCreate(ctx, &container.Config{Image: source, Labels: labels}, nil, nil, nil, "")
	if err != nil {
		return "", serviceError(http.StatusServiceUnavailable, "create emulator source container: "+err.Error())
	}
	defer func() {
		_ = m.client.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	}()

	archive, _, err := m.client.CopyFromContainer(ctx, created.ID, imageRIEPath)
	if err != nil {
		return "", serviceError(http.StatusServiceUnavailable, "copy emulator out of "+source+": "+err.Error())
	}
	defer archive.Close()
	if err := extractSingleFile(archive, dst); err != nil {
		return "", serviceError(http.StatusServiceUnavailable, "extract emulator: "+err.Error())
	}
	m.rieHost = dst
	return dst, nil
}

func serviceError(status int, message string) *apierror.Error {
	return &apierror.Error{StatusCode: status, Code: "ServiceException", Message: message}
}

func (m *Manager) removeContainer(ctx context.Context, warm *WarmContainer) error {
	if warm == nil || warm.ID == "" {
		return nil
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = m.client.ContainerStop(stopCtx, warm.ID, container.StopOptions{Timeout: intPtr(1)})
	err := m.client.ContainerRemove(stopCtx, warm.ID, container.RemoveOptions{Force: true})
	if err != nil && !dockerclient.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (m *Manager) removeNamedContainer(ctx context.Context, name string) error {
	containers, err := m.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}
	for _, item := range containers {
		for _, candidate := range item.Names {
			if strings.TrimPrefix(candidate, "/") == name {
				return m.removeContainer(ctx, &WarmContainer{ID: item.ID, Name: name})
			}
		}
	}
	return nil
}

func sanitizeName(functionName string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, functionName)
	if len(name) > 48 {
		name = name[:48]
	}
	return name
}

func intPtr(v int) *int {
	return &v
}

// runtimeExtraHosts maps host.docker.internal to the daemon host when the
// container's environment refers to it (Linux has no such name by default).
func runtimeExtraHosts(env []string) []string {
	for _, kv := range env {
		if _, value, _ := strings.Cut(kv, "="); strings.Contains(value, hostDockerInternal) {
			return []string{"host.docker.internal:host-gateway"}
		}
	}
	return nil
}

// newDockerClient connects to the daemon named by DOCKER_HOST, Docker
// Desktop's per-user socket, or the default socket, in that order.
func newDockerClient() (*dockerclient.Client, error) {
	opts := []dockerclient.Opt{dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation()}
	if host := defaultDockerHost(); host != "" {
		opts = append(opts, dockerclient.WithHost(host))
	}
	client, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return client, nil
}

func defaultDockerHost() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	socketPath := filepath.Join(home, ".docker", "run", "docker.sock")
	if _, err := os.Stat(socketPath); err == nil {
		return "unix://" + socketPath
	}
	return ""
}

// runtimeKey is the warm-pool key of a function: unique across accounts and
// regions (see lambda.RuntimeKey), or the plain name for older callers.
func runtimeKey(spec lambdasvc.FunctionSpec) string {
	if spec.Key != "" {
		return spec.Key
	}
	return spec.FunctionName
}

// roleCredentials issues the execution role's credentials for a new
// container. A function still starts without them if they cannot be issued.
func (m *Manager) roleCredentials(spec lambdasvc.FunctionSpec) lambdasvc.Credentials {
	if m.credentials == nil || spec.Role == "" {
		return lambdasvc.Credentials{}
	}
	key, secret, token, err := m.credentials.RoleCredentials(spec.Account, spec.Role, spec.FunctionName)
	if err != nil {
		m.logger.Warn("lambda execution role credentials not issued", "function", spec.FunctionName, "role", spec.Role, "error", err)
		return lambdasvc.Credentials{}
	}
	return lambdasvc.Credentials{AccessKeyID: key, SecretAccessKey: secret, SessionToken: token}
}

// invokePayload is the event a function receives: an invoke with no payload
// delivers "{}", as Lambda does, so runtimes that parse JSON don't fail.
func invokePayload(payload []byte) []byte {
	if len(bytes.TrimSpace(payload)) == 0 {
		return []byte("{}")
	}
	return payload
}
