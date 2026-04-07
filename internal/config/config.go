package config

import (
	"flag"
	"fmt"
	"path/filepath"
)

const (
	defaultPort     = 4566
	defaultDataDir  = "./data"
	defaultLogLevel = "info"
)

type Config struct {
	Port     int
	DataDir  string
	LogLevel string
}

func RegisterFlags(fs *flag.FlagSet) *Config {
	cfg := &Config{}
	fs.IntVar(&cfg.Port, "port", defaultPort, "Port to listen on")
	fs.StringVar(&cfg.DataDir, "data-dir", defaultDataDir, "Root directory for persistent data")
	fs.StringVar(&cfg.LogLevel, "log-level", defaultLogLevel, "Log level: debug, info, warn, error")
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
