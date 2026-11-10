package kafka

import (
	"fmt"
	"strings"
	"time"

	"github.com/stratus/internal/backing"
)

const (
	// kind labels the broker containers this service owns.
	kind = "kafka"

	// EnvImage overrides the broker image, for teams that mirror images or
	// pin another Redpanda release.
	EnvImage     = "STRATUS_KAFKA_IMAGE"
	defaultImage = "redpandadata/redpanda:v24.2.7"

	brokerPort = 9092
	dataDir    = "/var/lib/redpanda/data"

	// readyTimeout bounds how long cluster creation waits for the broker to
	// elect its controller. Redpanda usually needs two or three seconds.
	readyTimeout = 60 * time.Second
)

// Engine runs the Kafka-compatible broker behind an MSK cluster. Redpanda
// speaks the Kafka protocol, starts in seconds and needs no ZooKeeper, so one
// container is a whole cluster.
type Engine struct {
	Image string
}

// DefaultEngine returns the broker Stratus ships with. getenv resolves the
// image override; pass os.Getenv in production.
func DefaultEngine(getenv func(string) string) Engine {
	image := defaultImage
	if getenv != nil {
		if override := strings.TrimSpace(getenv(EnvImage)); override != "" {
			image = override
		}
	}
	return Engine{Image: image}
}

// Spec builds the broker's container spec for a cluster. Kafka clients
// connect to whatever address the broker advertises, so the host port must be
// known before the broker starts: hostPort cannot be zero. A non-empty seed is
// an archive of the cluster's data to start from.
func (e Engine) Spec(cluster string, hostPort int, seed string) backing.Spec {
	data := e.Data()
	return backing.Spec{
		Kind:         kind,
		Name:         cluster,
		Image:        e.Image,
		Cmd:          brokerCommand(hostPort),
		Port:         brokerPort,
		HostPort:     hostPort,
		Probe:        backing.KafkaController,
		ReadyTimeout: readyTimeout,
		Data:         &data,
		Seed:         seed,
	}
}

// Data is where the broker keeps topics and offsets. pid.lock belongs to the
// process that wrote it.
func (e Engine) Data() backing.Data {
	return backing.Data{Path: dataDir, Exclude: []string{"pid.lock"}}
}

// brokerCommand starts a single-node Redpanda sized for a test machine and
// advertising the loopback endpoint clients reach it on.
func brokerCommand(hostPort int) []string {
	return []string{
		"redpanda", "start",
		"--mode", "dev-container",
		"--smp", "1",
		"--memory", "1G",
		"--default-log-level", "warn",
		"--kafka-addr", fmt.Sprintf("PLAINTEXT://0.0.0.0:%d", brokerPort),
		"--advertise-kafka-addr", fmt.Sprintf("PLAINTEXT://%s:%d", loopback, hostPort),
	}
}

const loopback = "127.0.0.1"
