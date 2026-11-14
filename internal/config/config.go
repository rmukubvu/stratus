package config

import (
	"flag"
	"fmt"
	"path/filepath"
)

const (
	defaultPort      = 4566
	defaultDataDir   = "./data"
	defaultLogLevel  = "info"
	defaultLogFormat = "auto"
)

type Config struct {
	Port      int
	DataDir   string
	LogLevel  string
	LogFormat string
	// Seed is an optional snapshot directory used to initialise an empty
	// data directory at startup.
	Seed string
	// Limits applies a limits profile at startup: a base profile name
	// (aws-default, strict) or the path of a JSON profile. Empty falls
	// back to STRATUS_LIMITS; both empty leaves limits off.
	Limits string
	// LambdaContainers caps the warm containers one Lambda function may
	// have; zero falls back to STRATUS_LAMBDA_CONTAINERS, then 4.
	LambdaContainers int
}

func RegisterFlags(fs *flag.FlagSet) *Config {
	cfg := &Config{}
	fs.IntVar(&cfg.Port, "port", defaultPort, "Port to listen on")
	fs.StringVar(&cfg.DataDir, "data-dir", defaultDataDir, "Root directory for persistent data")
	fs.StringVar(&cfg.LogLevel, "log-level", defaultLogLevel, "Log level: debug, info, warn, error")
	fs.StringVar(&cfg.LogFormat, "log-format", defaultLogFormat, "Log format: auto, json, pretty")
	fs.StringVar(&cfg.Seed, "seed", "", "Snapshot directory to initialise an empty data dir from")
	fs.StringVar(&cfg.Limits, "limits", "", "Limits profile to apply at startup: aws-default, strict or a JSON file (default $STRATUS_LIMITS)")
	fs.IntVar(&cfg.LambdaContainers, "lambda-containers", 0, "Most warm containers one Lambda function may run at once (default $STRATUS_LAMBDA_CONTAINERS, or 4)")
	return cfg
}

func (c *Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	if c.DataDir == "" {
		return fmt.Errorf("data dir must not be empty")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log level %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "auto", "json", "pretty":
	default:
		return fmt.Errorf("invalid log format %q", c.LogFormat)
	}
	if c.LambdaContainers < 0 {
		return fmt.Errorf("lambda containers must not be negative")
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	c.DataDir = abs
	return nil
}

func (c Config) Address() string {
	return fmt.Sprintf(":%d", c.Port)
}
