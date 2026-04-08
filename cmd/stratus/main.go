package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/stratus/internal/app"
	"github.com/stratus/internal/config"
)

func main() {
	cfg := config.RegisterFlags(flag.CommandLine)
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "stratus: %v\n", err)
		os.Exit(1)
	}

	logger := app.NewLogger(cfg.LogLevel, cfg.LogFormat)
	application, err := app.New(*cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := application.Run(ctx); err != nil {
		if err != context.Canceled {
			logger.Error("server exited with error", "error", err)
			os.Exit(1)
		}
	}

	logger.Info("shutdown complete")
}
