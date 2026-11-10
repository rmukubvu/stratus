package rds

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stratus/internal/backing"
)

// Engine describes how one RDS engine runs locally: which image provides
// it, where it listens and how its credentials are passed in.
type Engine struct {
	// Name is the engine name clients send, for example "postgres".
	Name string
	// family selects the image's configuration conventions.
	family engineFamily
	// Image is the container image that provides the engine.
	Image string
	// Port is the engine's port inside the container.
	Port int
	// DefaultVersion is reported as EngineVersion when the caller sets none.
	DefaultVersion string
	// DefaultDBName is the database created when the caller names none.
	DefaultDBName string
}

type engineFamily string

const (
	familyPostgres engineFamily = "postgres"
	familyMySQL    engineFamily = "mysql"
	familyMariaDB  engineFamily = "mariadb"
)

// Credentials are the master user and initial database of an instance.
type Credentials struct {
	Username string
	Password string
	Database string
}

// readyTimeout bounds how long CreateDBInstance waits for an engine. MySQL's
// first start initialises its data directory, which can take a while on a
// cold CI runner.
const readyTimeout = 90 * time.Second

// Data directories inside the engine images. PostgreSQL's is set explicitly
// through PGDATA, because newer images moved the default.
const (
	postgresDataDir = "/var/lib/postgresql/data"
	mysqlDataDir    = "/var/lib/mysql"
)

// Spec builds the engine's container spec for an instance. A non-empty seed
// is an archive of the instance's data to start from.
func (e Engine) Spec(identifier string, creds Credentials, hostPort int, seed string) backing.Spec {
	data := e.Data()
	return backing.Spec{
		Kind:         kind,
		Name:         identifier,
		Image:        e.Image,
		Env:          e.env(creds),
		Port:         e.Port,
		HostPort:     hostPort,
		Probe:        e.probe(creds),
		ReadyTimeout: readyTimeout,
		Data:         &data,
		Seed:         seed,
	}
}

// Data is where the engine keeps its databases. Every RDS engine writes to
// disk, so a paused copy of the directory is a crash-consistent backup that
// the engine recovers on its next start.
func (e Engine) Data() backing.Data {
	switch e.family {
	case familyPostgres:
		// postmaster.pid belongs to the process that wrote it.
		return backing.Data{Path: postgresDataDir, Exclude: []string{"postmaster.pid"}}
	default:
		return backing.Data{Path: mysqlDataDir}
	}
}

// DatabaseName is the database the engine creates for creds.
func (e Engine) DatabaseName(creds Credentials) string {
	if creds.Database != "" {
		return creds.Database
	}
	return e.DefaultDBName
}

// probe speaks the engine's protocol, because a Docker-published port
// accepts connections before the server behind it is up.
func (e Engine) probe(creds Credentials) backing.Probe {
	switch e.family {
	case familyPostgres:
		return backing.PostgresStartup(creds.Username, e.DatabaseName(creds))
	case familyMySQL, familyMariaDB:
		return backing.MySQLHandshake
	default:
		return backing.TCP
	}
}

func (e Engine) env(creds Credentials) []string {
	database := e.DatabaseName(creds)
	switch e.family {
	case familyPostgres:
		return []string{
			"POSTGRES_USER=" + creds.Username,
			"POSTGRES_PASSWORD=" + creds.Password,
			"POSTGRES_DB=" + database,
			"PGDATA=" + postgresDataDir,
		}
	case familyMySQL:
		return sqlFamilyEnv("MYSQL", creds, database)
	case familyMariaDB:
		return sqlFamilyEnv("MARIADB", creds, database)
	default:
		return nil
	}
}

// sqlFamilyEnv configures the MySQL and MariaDB images, which share one
// convention: the root password is mandatory and an extra user is optional.
// Naming root as the master user must not create a second "root".
func sqlFamilyEnv(prefix string, creds Credentials, database string) []string {
	env := []string{prefix + "_ROOT_PASSWORD=" + creds.Password}
	if creds.Username != "root" {
		env = append(env, prefix+"_USER="+creds.Username, prefix+"_PASSWORD="+creds.Password)
	}
	if database != "" {
		env = append(env, prefix+"_DATABASE="+database)
	}
	return env
}

// Engines is the set of engines an RDS service can run.
type Engines struct {
	byName map[string]Engine
}

// Environment variables that override the default image per engine family,
// for teams that mirror images or need another version.
const (
	EnvPostgresImage = "STRATUS_RDS_POSTGRES_IMAGE"
	EnvMySQLImage    = "STRATUS_RDS_MYSQL_IMAGE"
	EnvMariaDBImage  = "STRATUS_RDS_MARIADB_IMAGE"

	defaultPostgresImage = "postgres:16-alpine"
	defaultMySQLImage    = "mysql:8.0"
	defaultMariaDBImage  = "mariadb:11"
)

// DefaultEngines returns the engines Stratus ships with. getenv resolves image
// overrides; pass os.Getenv in production.
func DefaultEngines(getenv func(string) string) Engines {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	postgres := imageOr(getenv(EnvPostgresImage), defaultPostgresImage)
	mysql := imageOr(getenv(EnvMySQLImage), defaultMySQLImage)
	mariadb := imageOr(getenv(EnvMariaDBImage), defaultMariaDBImage)
	return NewEngines(
		Engine{Name: "postgres", family: familyPostgres, Image: postgres, Port: 5432, DefaultVersion: "16.4", DefaultDBName: "postgres"},
		Engine{Name: "aurora-postgresql", family: familyPostgres, Image: postgres, Port: 5432, DefaultVersion: "16.4", DefaultDBName: "postgres"},
		Engine{Name: "mysql", family: familyMySQL, Image: mysql, Port: 3306, DefaultVersion: "8.0.39"},
		Engine{Name: "aurora-mysql", family: familyMySQL, Image: mysql, Port: 3306, DefaultVersion: "8.0.mysql_aurora.3.07.1"},
		Engine{Name: "mariadb", family: familyMariaDB, Image: mariadb, Port: 3306, DefaultVersion: "11.4.3"},
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
