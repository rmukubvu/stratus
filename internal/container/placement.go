package container

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
)

// Placement is where this Stratus process runs as seen by the host: what
// detection needs to read from the process's own environment.
type Placement struct {
	// InContainer means Stratus runs in some container (Docker, Podman,
	// Kubernetes), so its filesystem is not the daemon host's.
	InContainer bool
	// ContainerIDs are candidate IDs of Stratus's own container, from
	// /proc/self/mountinfo and /proc/self/cgroup.
	ContainerIDs []string
	// Hostname is the process's hostname, which Docker sets to the short
	// container ID unless told otherwise.
	Hostname string
}

// DetectPlacement inspects the running process.
func DetectPlacement() Placement {
	mountinfo, _ := os.ReadFile("/proc/self/mountinfo")
	cgroup, _ := os.ReadFile("/proc/self/cgroup")
	hostname, _ := os.Hostname()
	return placementFrom(string(mountinfo), string(cgroup), hostname, fileExists, os.Getenv)
}

func placementFrom(mountinfo, cgroup, hostname string, exists func(string) bool, getenv func(string) string) Placement {
	ids := containerIDs(mountinfo, cgroup)
	in := len(ids) > 0 ||
		exists("/.dockerenv") ||
		exists("/run/.containerenv") ||
		getenv("KUBERNETES_SERVICE_HOST") != "" ||
		strings.Contains(cgroup, "kubepods")
	return Placement{InContainer: in, ContainerIDs: ids, Hostname: hostname}
}

// Docker keeps a container's hostname, hosts and resolv.conf files under
// .../containers/<id>/, and bind-mounts them in, so the ID shows up in the
// mount table. cgroup v1 paths carry it too.
var (
	mountContainerID  = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)
	cgroupContainerID = regexp.MustCompile(`(?:docker[-/])([0-9a-f]{64})`)
)

func containerIDs(mountinfo, cgroup string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range append(mountContainerID.FindAllStringSubmatch(mountinfo, -1), cgroupContainerID.FindAllStringSubmatch(cgroup, -1)...) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// daemonAPI is the part of the Docker client reach resolution needs.
type daemonAPI interface {
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	NetworkInspect(ctx context.Context, id string, opts network.InspectOptions) (network.Inspect, error)
	NetworkCreate(ctx context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(ctx context.Context, networkID, containerID string, cfg *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
	NetworkRemove(ctx context.Context, networkID string) error
}

// Resolution is the outcome of resolving reach: the policy, how files reach
// Lambda containers, the address containers use to call Stratus back, and
// what to undo on shutdown.
type Resolution struct {
	Reach Reach
	Files FileDelivery
	// CallbackHost is the host name or address a container uses to reach
	// Stratus: host.docker.internal beside the daemon, Stratus's own
	// address on the shared network or the host's network otherwise.
	CallbackHost string
	// cleanup removes a network Stratus created for itself.
	cleanup func(context.Context) error
}

// Close undoes what resolution set up.
func (r Resolution) Close(ctx context.Context) error {
	if r.cleanup == nil {
		return nil
	}
	return r.cleanup(ctx)
}

// hostDockerInternal is how containers reach the daemon host. Stratus adds
// the host-gateway mapping for it on Linux.
const hostDockerInternal = "host.docker.internal"

// resolver decides the reach policy. Its collaborators are injected so that
// every decision is unit tested without a daemon.
type resolver struct {
	api       daemonAPI
	placement Placement
	instance  string
	// localAddressFor is the address of this process's interface that
	// routes to a remote host, which containers use to call back.
	localAddressFor func(remote string) (string, error)
}

// Resolve picks the reach policy for settings. Explicit settings win;
// otherwise a Stratus container the daemon knows joins a network shared with
// its containers, and anything else keeps the loopback default.
func (r resolver) Resolve(ctx context.Context, s ReachSettings) (Resolution, error) {
	// A daemon that cannot be reached is not fatal: Stratus starts without
	// Docker, so detection falls back to what needs no daemon.
	self, selfErr := r.findSelf(ctx)
	known := selfErr == nil

	var res Resolution
	var err error
	switch {
	case s.Mode == ReachLoopback:
		res = Resolution{Reach: LoopbackReach(), CallbackHost: hostDockerInternal}
	case s.Mode == ReachHost:
		res, err = r.host(s.HostAddress)
	case s.Mode == ReachNetwork && s.Network != "":
		res, err = r.namedNetwork(ctx, s.Network, self, known)
	case s.Mode == ReachNetwork, known && !sharesHostNetwork(self):
		if !known {
			return Resolution{}, fmt.Errorf("%s=network needs %s, or Stratus running in a container the Docker daemon knows", EnvReach, EnvNetwork)
		}
		res, err = r.ownNetwork(ctx, self)
	default:
		res = Resolution{Reach: LoopbackReach(), CallbackHost: hostDockerInternal}
	}
	if err != nil {
		return Resolution{}, err
	}
	res.Files = r.files(s.Files, res.Reach)
	return res, res.Reach.Validate()
}

// files copies whenever Stratus's filesystem may not be the daemon's.
func (r resolver) files(asked FileDelivery, reach Reach) FileDelivery {
	if asked != "" {
		return asked
	}
	if r.placement.InContainer || reach.Mode != ReachLoopback {
		return DeliverCopy
	}
	return DeliverBind
}

func (r resolver) host(address string) (Resolution, error) {
	callback, err := r.localAddressFor(address)
	if err != nil {
		return Resolution{}, fmt.Errorf("find the address containers reach Stratus on: %w", err)
	}
	return Resolution{Reach: Reach{Mode: ReachHost, HostAddress: address}, CallbackHost: callback}, nil
}

// namedNetwork uses the operator's network, joining it if Stratus runs in a
// container that is not on it yet.
func (r resolver) namedNetwork(ctx context.Context, name string, self container.InspectResponse, known bool) (Resolution, error) {
	if _, err := r.api.NetworkInspect(ctx, name, network.InspectOptions{}); err != nil {
		return Resolution{}, fmt.Errorf("docker network %s: %w", name, err)
	}
	res := Resolution{Reach: Reach{Mode: ReachNetwork, Network: name}}
	if !known {
		// Stratus is on that network some other way; containers call it back
		// by hostname, which Docker's DNS resolves on user-defined networks.
		res.CallbackHost = r.placement.Hostname
		return res, nil
	}
	ip, err := r.join(ctx, name, self)
	if err != nil {
		return Resolution{}, err
	}
	res.CallbackHost = ip
	return res, nil
}

// ownNetwork shares a user-defined network with Stratus's containers: one
// Stratus is already on, or one it creates for itself. Only user-defined
// networks resolve container names, which engine endpoints rely on.
func (r resolver) ownNetwork(ctx context.Context, self container.InspectResponse) (Resolution, error) {
	if name, ip := userNetwork(self); name != "" {
		return Resolution{Reach: Reach{Mode: ReachNetwork, Network: name}, CallbackHost: ip}, nil
	}

	name := "stratus-" + r.instance
	created := false
	if _, err := r.api.NetworkInspect(ctx, name, network.InspectOptions{}); err != nil {
		if !dockerclient.IsErrNotFound(err) {
			return Resolution{}, fmt.Errorf("inspect docker network %s: %w", name, err)
		}
		labels := map[string]string{LabelManaged: "true", LabelInstance: r.instance}
		if _, err := r.api.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge", Labels: labels}); err != nil {
			return Resolution{}, fmt.Errorf("create docker network %s: %w", name, err)
		}
		created = true
	}
	ip, err := r.join(ctx, name, self)
	if err != nil {
		if created {
			_ = r.api.NetworkRemove(context.WithoutCancel(ctx), name)
		}
		return Resolution{}, err
	}
	id := self.ID
	return Resolution{
		Reach:        Reach{Mode: ReachNetwork, Network: name},
		CallbackHost: ip,
		cleanup: func(ctx context.Context) error {
			_ = r.api.NetworkDisconnect(ctx, name, id, true)
			if err := r.api.NetworkRemove(ctx, name); err != nil && !dockerclient.IsErrNotFound(err) {
				return fmt.Errorf("remove docker network %s: %w", name, err)
			}
			return nil
		},
	}, nil
}

// join connects Stratus's container to a network and returns its address
// there.
func (r resolver) join(ctx context.Context, name string, self container.InspectResponse) (string, error) {
	if ip := networkIP(self, name); ip != "" {
		return ip, nil
	}
	if err := r.api.NetworkConnect(ctx, name, self.ID, &network.EndpointSettings{}); err != nil {
		return "", fmt.Errorf("connect stratus container to docker network %s: %w", name, err)
	}
	joined, err := r.api.ContainerInspect(ctx, self.ID)
	if err != nil {
		return "", fmt.Errorf("inspect stratus container: %w", err)
	}
	ip := networkIP(joined, name)
	if ip == "" {
		return "", fmt.Errorf("stratus container has no address on docker network %s", name)
	}
	return ip, nil
}

var errSelfNotFound = errors.New("stratus is not in a container the daemon knows")

// findSelf finds Stratus's own container on the daemon. A hostname only
// counts when the container it names reports the same hostname, so a
// coincidental match on another host's container is ignored.
func (r resolver) findSelf(ctx context.Context) (container.InspectResponse, error) {
	if !r.placement.InContainer {
		return container.InspectResponse{}, errSelfNotFound
	}
	for _, id := range r.placement.ContainerIDs {
		found, err := r.api.ContainerInspect(ctx, id)
		if err == nil {
			return found, nil
		}
		if !dockerclient.IsErrNotFound(err) {
			return container.InspectResponse{}, err
		}
	}
	if h := r.placement.Hostname; h != "" {
		found, err := r.api.ContainerInspect(ctx, h)
		if err == nil && found.Config != nil && found.Config.Hostname == h {
			return found, nil
		}
		if err != nil && !dockerclient.IsErrNotFound(err) {
			return container.InspectResponse{}, err
		}
	}
	return container.InspectResponse{}, errSelfNotFound
}

// sharesHostNetwork reports a --network host container, which reaches
// published loopback ports like any host process. The mode is compared
// directly: NetworkMode.IsHost follows the client's OS, but the containers
// are Linux whatever Stratus runs on.
func sharesHostNetwork(self container.InspectResponse) bool {
	return self.HostConfig != nil && self.HostConfig.NetworkMode == "host"
}

// userNetwork is the first user-defined network a container is on, by name,
// and its address there.
func userNetwork(c container.InspectResponse) (string, string) {
	if c.NetworkSettings == nil {
		return "", ""
	}
	names := make([]string, 0, len(c.NetworkSettings.Networks))
	for name := range c.NetworkSettings.Networks {
		switch name {
		case "bridge", "host", "none":
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ip := networkIP(c, name); ip != "" {
			return name, ip
		}
	}
	return "", ""
}

func networkIP(c container.InspectResponse, name string) string {
	if c.NetworkSettings == nil {
		return ""
	}
	if ep := c.NetworkSettings.Networks[name]; ep != nil {
		return ep.IPAddress
	}
	return ""
}

// localAddressFor finds the local interface address that routes to remote,
// without sending anything: connecting a UDP socket only picks a route.
func localAddressFor(remote string) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(remote, "9"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
