package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/stratus/internal/app"
	"github.com/stratus/internal/cli"
	"github.com/stratus/internal/config"
)

func main() {
	if wantsHelp(os.Args[1:]) {
		printUsage()
		return
	}

	mode, args := cli.ParseMode(os.Args[1:])
	var (
		cfg    *config.Config
		noOpen bool
	)

	switch mode {
	case cli.ModeDev:
		fs := flag.NewFlagSet("stratus dev", flag.ExitOnError)
		cfg = config.RegisterFlags(fs)
		fs.BoolVar(&noOpen, "no-open", false, "Do not open the operator portal in the browser")
		fs.Parse(args)
	case cli.ModeServe:
		fs := flag.NewFlagSet("stratus serve", flag.ExitOnError)
		cfg = config.RegisterFlags(fs)
		fs.Parse(args)
	default:
		fmt.Fprintf(os.Stderr, "stratus: unsupported mode %q\n", mode)
		os.Exit(1)
	}

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

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	portalURL := baseURL + "/_stratus/"

	if mode == cli.ModeDev {
		logger.Info("operator portal",
			"portal_url", portalURL,
			"endpoint", baseURL,
			"hint", "use `stratus serve` for headless mode",
		)
		if !noOpen {
			go waitForPortalAndOpen(portalURL, logger)
		}
	}

	if err := application.Run(ctx); err != nil {
		if err != context.Canceled {
			logger.Error("server exited with error", "error", err)
			os.Exit(1)
		}
	}

	logger.Info("shutdown complete")
}

func wantsHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "help":
		return true
	default:
		return false
	}
}

func printUsage() {
	fmt.Fprint(os.Stdout, `stratus

Usage:
  stratus                       Start the emulator in developer mode and open the built-in portal
  stratus dev [flags]           Start the emulator and serve the built-in portal
  stratus serve [flags]         Start the emulator in headless mode
  stratus --port 4566           Legacy headless form, equivalent to serve

Common flags:
  --port <port>                 Port to listen on (default: 4566)
  --data-dir <dir>              Persistent data directory (default: ./data)
  --log-level <level>           debug, info, warn, error
  --log-format <format>         auto, json, pretty

Dev-only flags:
  --no-open                     Do not open the operator portal in the browser

Key endpoints:
  /_stratus/                    Built-in operator portal
  /_stratus/health              Health check
  /_stratus/operator/bootstrap  Service inventory and quick-start data
`)
}

func waitForPortalAndOpen(portalURL string, logger interface {
	Info(string, ...any)
	Debug(string, ...any)
}) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(portalURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				if err := openBrowser(portalURL); err != nil {
					logger.Debug("browser open skipped", "error", err, "portal_url", portalURL)
				} else {
					logger.Info("opened operator portal", "portal_url", portalURL)
				}
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	logger.Debug("operator portal did not become ready before browser open timeout", "portal_url", portalURL)
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
