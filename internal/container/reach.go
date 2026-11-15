package container

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"

	"github.com/stratus/internal/backing"
)

// ReachMode says how Stratus reaches the containers it starts, which depends
// on where Stratus runs relative to the Docker daemon.
type ReachMode string

const (
	// ReachLoopback publishes container ports on the daemon host's loopback
	// interface and dials 127.0.0.1. It works when Stratus shares the daemon
	// host's network namespace: a VM, bare metal, Docker Desktop, a
	// Docker-in-Docker sidecar in the same pod, or a --network host container.
	ReachLoopback ReachMode = "loopback"
	// ReachNetwork attaches every container to a user-defined Docker network
	// that Stratus is also on, and dials containers by name on their own
	// ports. It is for Stratus running in a container that shares the host's
	// docker.sock, such as a GitHub Actions container job.
	ReachNetwork ReachMode = "network"
	// ReachHost publishes container ports on every interface of the daemon
	// host and dials a fixed address of that host. It is for Stratus running
	// somewhere the daemon has no network in common with, such as a
	// Kubernetes runner pod that mounts the node's docker.sock.
	ReachHost ReachMode = "host"
)

// Reach is the resolved addressing policy: a value object every container
// start and every endpoint lookup goes through, so the services that own
// engines never need to know where Stratus runs.
type Reach struct {
	Mode ReachMode
	// Network is the Docker network containers join (ReachNetwork).
	Network string
	// HostAddress is the daemon host's address as Stratus sees it (ReachHost).
	HostAddress string
}

// LoopbackReach is the policy for Stratus running beside its daemon.
func LoopbackReach() Reach { return Reach{Mode: ReachLoopback} }

// Validate reports a policy that cannot work.
func (r Reach) Validate() error {
	switch r.Mode {
	case ReachLoopback:
		return nil
	case ReachNetwork:
		if r.Network == "" {
			return fmt.Errorf("container reach %q needs a Docker network", r.Mode)
		}
		return nil
	case ReachHost:
		if r.HostAddress == "" {
			return fmt.Errorf("container reach %q needs the Docker host's address", r.Mode)
		}
		return nil
	default:
		return fmt.Errorf("unknown container reach %q", r.Mode)
	}
}

// PortBindings publishes containerPort for this policy. hostPort may be
// empty to let Docker pick one. On a shared network nothing is published:
// containers are dialled on their own ports, so parallel instances never
// compete for host ports.
func (r Reach) PortBindings(containerPort nat.Port, hostPort string) nat.PortMap {
	switch r.Mode {
	case ReachNetwork:
		return nil
	case ReachHost:
		return nat.PortMap{containerPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: hostPort}}}
	default:
		return nat.PortMap{containerPort: []nat.PortBinding{{HostIP: loopback, HostPort: hostPort}}}
	}
}

// NetworkingConfig attaches a new container to the shared network, if any.
func (r Reach) NetworkingConfig() *network.NetworkingConfig {
	if r.Mode != ReachNetwork {
		return &network.NetworkingConfig{}
	}
	return &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{r.Network: {}}}
}

// Endpoint is where Stratus, and clients beside it, reach a container named
// name that listens on containerPort and was published on hostPort.
func (r Reach) Endpoint(name string, containerPort, hostPort int) backing.Endpoint {
	switch r.Mode {
	case ReachNetwork:
		return backing.Endpoint{Address: name, Port: containerPort}
	case ReachHost:
		return backing.Endpoint{Address: r.HostAddress, Port: hostPort}
	default:
		return backing.Endpoint{Address: loopback, Port: hostPort}
	}
}

// Publishes reports whether containers get host ports under this policy.
func (r Reach) Publishes() bool { return r.Mode != ReachNetwork }

// String describes the policy for logs.
func (r Reach) String() string {
	switch r.Mode {
	case ReachNetwork:
		return string(r.Mode) + ":" + r.Network
	case ReachHost:
		return string(r.Mode) + ":" + r.HostAddress
	default:
		return string(r.Mode)
	}
}

// FileDelivery says how a Lambda container gets its code, layers and, for
// images without one, the Runtime Interface Emulator.
type FileDelivery string

const (
	// DeliverBind bind-mounts Stratus's directories. It needs Stratus and the
	// daemon to share a filesystem.
	DeliverBind FileDelivery = "bind"
	// DeliverCopy copies the files into the container before it starts. It
	// works whatever filesystem the daemon sees.
	DeliverCopy FileDelivery = "copy"
)

// Environment variables that override detection.
const (
	EnvReach       = "STRATUS_DOCKER_REACH"
	EnvNetwork     = "STRATUS_DOCKER_NETWORK"
	EnvHostAddress = "STRATUS_DOCKER_HOST_ADDRESS"
	EnvFiles       = "STRATUS_DOCKER_FILES"
)

// ReachSettings is what the operator asked for. Empty fields mean "detect".
type ReachSettings struct {
	Mode        ReachMode // empty: auto
	Network     string
	HostAddress string
	Files       FileDelivery // empty: auto
}

// ParseReachSettings reads the overrides from the environment.
func ParseReachSettings(getenv func(string) string) (ReachSettings, error) {
	s := ReachSettings{
		Network:     strings.TrimSpace(getenv(EnvNetwork)),
		HostAddress: strings.TrimSpace(getenv(EnvHostAddress)),
	}
	switch mode := ReachMode(strings.ToLower(strings.TrimSpace(getenv(EnvReach)))); mode {
	case "", "auto":
	case ReachLoopback, ReachNetwork, ReachHost:
		s.Mode = mode
	default:
		return ReachSettings{}, fmt.Errorf("%s=%q: want auto, loopback, network or host", EnvReach, mode)
	}
	switch files := FileDelivery(strings.ToLower(strings.TrimSpace(getenv(EnvFiles)))); files {
	case "", "auto":
	case DeliverBind, DeliverCopy:
		s.Files = files
	default:
		return ReachSettings{}, fmt.Errorf("%s=%q: want auto, bind or copy", EnvFiles, files)
	}

	if s.Network != "" && s.HostAddress != "" {
		return ReachSettings{}, fmt.Errorf("set only one of %s and %s", EnvNetwork, EnvHostAddress)
	}
	// Naming a network or a host address picks its mode.
	if s.Mode == "" && s.Network != "" {
		s.Mode = ReachNetwork
	}
	if s.Mode == "" && s.HostAddress != "" {
		s.Mode = ReachHost
	}
	if s.Mode == ReachHost && s.HostAddress == "" {
		return ReachSettings{}, fmt.Errorf("%s=host needs %s, the Docker host's address", EnvReach, EnvHostAddress)
	}
	return s, nil
}

// portLabel records the port an engine listens on inside its container, so
// its endpoint can be found again from a container listing.
const portLabel = "io.stratus.port"

func portLabelValue(port int) string { return strconv.Itoa(port) }
