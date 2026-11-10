package elasticache

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stratus/internal/backing"
)

// Engine describes how one cache engine runs locally.
type Engine struct {
	// Name is the engine name clients send, for example "redis".
	Name string
	// Image is the container image that provides the engine.
	Image string
	// Port is the engine's port inside the container.
	Port int
	// DefaultVersion is reported as EngineVersion when the caller sets none.
	DefaultVersion string
	// probe decides when the engine is ready.
	probe backing.Probe
	// data is where the engine keeps state worth snapshotting. Nil means it
	// keeps none: Memcached is memory only, as it is on AWS.
	data *backing.Data
}

// readyTimeout bounds how long CreateCacheCluster waits for an engine. Cache
// engines start in well under a second; the margin covers slow CI disks.
const readyTimeout = 30 * time.Second

// Spec builds the engine's container spec for a cluster. A non-empty seed is
// an archive of the cluster's data to start from; engines that keep no data
// ignore it.
func (e Engine) Spec(clusterID string, hostPort int, seed string) backing.Spec {
	spec := backing.Spec{
		Kind:         kind,
		Name:         clusterID,
		Image:        e.Image,
		Port:         e.Port,
		HostPort:     hostPort,
		Probe:        e.probe,
		ReadyTimeout: readyTimeout,
		Data:         e.data,
	}
	if e.data != nil {
		spec.Seed = seed
	}
	return spec
}

// Data reports where the engine keeps state worth snapshotting, and false
// for engines that keep none.
func (e Engine) Data() (backing.Data, bool) {
	if e.data == nil {
		return backing.Data{}, false
	}
	return *e.data, true
}

// redisData snapshots a Valkey server: SAVE writes dump.rdb into the working
// directory, /data in the image, and the server loads it on start.
var redisData = &backing.Data{Path: "/data", Flush: backing.RedisSave}

// Engines is the set of engines an ElastiCache service can run.
type Engines struct {
	byName map[string]Engine
}

// Environment variables that override the default image per engine.
const (
	EnvRedisImage     = "STRATUS_ELASTICACHE_REDIS_IMAGE"
	EnvMemcachedImage = "STRATUS_ELASTICACHE_MEMCACHED_IMAGE"

	// Valkey speaks the Redis protocol and is what ElastiCache runs today
	// for new clusters, so it backs both the "redis" and "valkey" engines.
	defaultRedisImage     = "valkey/valkey:8"
	defaultMemcachedImage = "memcached:1.6-alpine"
)

// DefaultEngines returns the engines Stratus ships with. getenv resolves image
// overrides; pass os.Getenv in production.
func DefaultEngines(getenv func(string) string) Engines {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	redis := imageOr(getenv(EnvRedisImage), defaultRedisImage)
	memcached := imageOr(getenv(EnvMemcachedImage), defaultMemcachedImage)
	return NewEngines(
		Engine{Name: "redis", Image: redis, Port: 6379, DefaultVersion: "7.1", probe: backing.RedisPing, data: redisData},
		Engine{Name: "valkey", Image: redis, Port: 6379, DefaultVersion: "8.0", probe: backing.RedisPing, data: redisData},
		Engine{Name: "memcached", Image: memcached, Port: 11211, DefaultVersion: "1.6.22", probe: backing.MemcachedVersion},
	)
}

// NewEngines indexes engines by name.
func NewEngines(engines ...Engine) Engines {
	byName := make(map[string]Engine, len(engines))
	for _, e := range engines {
		byName[e.Name] = e
	}
	return Engines{byName: byName}
}

// Lookup finds an engine by the name a client sent.
func (e Engines) Lookup(name string) (Engine, bool) {
	engine, ok := e.byName[strings.ToLower(name)]
	return engine, ok
}

// Names lists the supported engine names, sorted.
func (e Engines) Names() []string {
	names := make([]string, 0, len(e.byName))
	for name := range e.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (e Engines) unsupported(name string) string {
	return fmt.Sprintf("engine %q is not supported; use one of %s", name, strings.Join(e.Names(), ", "))
}

func imageOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
