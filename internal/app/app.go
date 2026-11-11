package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/config"
	"github.com/stratus/internal/container"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/diagnose"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/operator"
	"github.com/stratus/internal/services/bedrock"
	"github.com/stratus/internal/services/iam"
	"github.com/stratus/internal/services/kafka"
	"github.com/stratus/internal/services/route53"
	"github.com/stratus/internal/services/scheduler"
	"github.com/stratus/internal/services/ses"
	"github.com/stratus/internal/services/sts"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

type App struct {
	cfg     config.Config
	logger  *slog.Logger
	server  *http.Server
	closers []io.Closer
}

func New(cfg config.Config, logger *slog.Logger) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	blobRoot := filepath.Join(cfg.DataDir, "blobs")
	if err := os.MkdirAll(blobRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create blob dir: %w", err)
	}

	if cfg.Seed != "" {
		snap, err := control.SeedDataDir(cfg.Seed, cfg.DataDir)
		if err != nil {
			return nil, err
		}
		logger.Info("seeded data dir from snapshot", "snapshot", snap.ID, "seed", cfg.Seed)
	}

	metaStore, err := bbolt.Open(filepath.Join(cfg.DataDir, "meta.db"))
	if err != nil {
		return nil, err
	}

	lambdaRuntime, err := container.NewManager(cfg.DataDir, blobRoot, logger)
	if err != nil {
		return nil, err
	}
	engines, err := container.NewBacking(cfg.DataDir, logger)
	if err != nil {
		return nil, err
	}

	bedrockConfig, err := bedrock.ConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	chaosEngine := chaos.NewEngine(control.DefaultCatalog().Names(), nil)
	blobStore := fsblob.New(blobRoot)
	iamKeys := iam.NewKeyDirectory(metaStore)
	stsService := sts.NewService(metaStore, iamKeys)
	stacks := newStacks(platform{
		meta:    metaStore,
		blobs:   blobStore,
		invoker: lambdaRuntime,
		engines: engines,
		faults:  chaos.NewInjector(chaosEngine, lambdaRuntime.CleanupFunction, logger),
		chaos:   chaosEngine,
		bedrock: bedrock.NewService(bedrock.Options{
			Metadata:   metaStore,
			FixtureDir: bedrockConfig.FixtureDir,
			Mode:       bedrockConfig.Mode,
			Upstream:   bedrock.UpstreamFromEnv(os.Getenv),
		}),
		sts:     stsService,
		iamKeys: iamKeys,
		getenv:  os.Getenv,
		logger:  logger,
	})
	defaults := stacks.Default()
	if err := stacks.Restore(); err != nil {
		return nil, err
	}
	backends := stacks.scopedBackends()
	// Records can outlive their containers: a restart reaps the old ones and
	// a seed brings records with no containers at all. Start what is missing.
	startEngines(backends, cfg.DataDir, cfg.Seed != "", logger)

	operatorStore := operator.NewStore("http://127.0.0.1"+cfg.Address(), cfg.DataDir, cfg.LogLevel, cfg.LogFormat)
	operatorService := operator.NewService(operatorStore, defaults.logs)

	snapshotRepo, err := control.NewFSSnapshotRepository(filepath.Join(cfg.DataDir, "snapshots"))
	if err != nil {
		return nil, err
	}
	controlService := control.NewService(control.Options{
		Catalog:   control.DefaultCatalog(),
		State:     metaStore,
		Blobs:     blobStore,
		Runtime:   lambdaRuntime,
		Backends:  backends,
		Snapshots: snapshotRepo,
	})

	// The control API's service views (SES inbox, scheduler clock, MSK
	// brokers, DNS) serve the default account and region.
	controlHandler := control.NewHandler(controlService)
	chaos.RegisterRoutes(controlHandler, chaosEngine)
	ses.RegisterRoutes(controlHandler, defaults.ses)
	scheduler.RegisterRoutes(controlHandler, defaults.scheduler)
	bedrock.RegisterRoutes(controlHandler, defaults.services.Bedrock)
	kafka.RegisterRoutes(controlHandler, defaults.kafka)
	route53.RegisterRoutes(controlHandler, defaults.route53)
	diagnose.RegisterRoutes(controlHandler, diagnose.NewService(
		failuresFrom(operatorStore),
		injectionsFrom(chaosEngine),
		nil,
	))

	handler := httpapi.NewServer(httpapi.Options{
		Logger:    logger,
		Services:  defaults.services,
		Stacks:    stacks,
		Accounts:  httpapi.AccountResolvers{stsService, iamKeys},
		Operator:  operatorService,
		Control:   controlHandler,
		Gate:      controlService.Gate(),
		Faults:    chaosEngine,
		ColdStart: lambdaRuntime.CleanupFunction,
	})

	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return &App{
		cfg:     cfg,
		logger:  logger,
		server:  server,
		closers: []io.Closer{stacks, metaStore, lambdaRuntime, engines},
	}, nil
}

func NewLogger(level, format string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: slogLevel}
	switch resolveLogFormat(format, os.Stderr) {
	case "pretty":
		return slog.New(NewPrettyHandler(os.Stderr, opts))
	default:
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
}

// Run listens on the configured address and serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.server.Addr)
	if err != nil {
		_ = a.closeResources()
		return fmt.Errorf("listen on %s: %w", a.server.Addr, err)
	}
	return a.Serve(ctx, ln)
}

// Serve serves on an existing listener until ctx is cancelled, then shuts
// down and releases all resources, including warm Lambda containers.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)

	a.logger.Info("starting stratus",
		"addr", a.server.Addr,
		"data_dir", a.cfg.DataDir,
		"log_level", a.cfg.LogLevel,
		"log_format", a.cfg.LogFormat,
	)

	go func() {
		err := a.server.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		a.logger.Info("shutting down")
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown server: %w", err)
		}
		if err := <-errCh; err != nil {
			_ = a.closeResources()
			return err
		}
		return a.closeResources()
	case err := <-errCh:
		if err != nil {
			_ = a.closeResources()
			return err
		}
		return a.closeResources()
	}
}

func (a *App) closeResources() error {
	var firstErr error
	for _, closer := range a.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// startEngines reconciles every backend at startup. Docker may be down; that
// is not fatal, the affected resources report a failed status instead.
//
// A data directory seeded from a snapshot starts its engines from the
// snapshot's engine data, on fresh ports because sibling seeds of the same
// snapshot may be running beside it. The seeded data is used once and then
// removed.
func startEngines(backends []control.Backend, dataDir string, seeded bool, logger *slog.Logger) {
	seedDir := control.SeedEngineDir(dataDir)
	for _, backend := range backends {
		var opts backing.ReconcileOptions
		if seeded {
			opts = backing.ReconcileOptions{DataDir: filepath.Join(seedDir, backend.Service()), FreshEndpoints: true}
		}
		if err := backend.Reconcile(context.Background(), opts); err != nil {
			logger.Warn("engines could not be started", "service", backend.Service(), "error", err)
		}
	}
	if err := os.RemoveAll(seedDir); err != nil {
		logger.Warn("could not remove seeded engine data", "dir", seedDir, "error", err)
	}
}

// failuresFrom adapts the operator's request log to the diagnosis port.
func failuresFrom(store *operator.Store) diagnose.FailureSource {
	return func() []diagnose.Failure {
		records := store.Errors(200)
		out := make([]diagnose.Failure, 0, len(records))
		for _, r := range records {
			out = append(out, diagnose.Failure{
				Time: r.Time, RequestID: r.RequestID, Service: r.Service, Operation: r.Operation,
				Method: r.Method, Path: r.Path, Status: r.Status, Code: r.ErrorCode, Message: r.ErrorMessage,
			})
		}
		return out
	}
}

// injectionsFrom adapts chaos hits to the diagnosis port.
func injectionsFrom(engine *chaos.Engine) diagnose.InjectionSource {
	return func() map[string]diagnose.Injection {
		hits := engine.Hits()
		out := make(map[string]diagnose.Injection, len(hits))
		for _, h := range hits {
			// Latency and cold starts don't fail a request by themselves, so
			// a failure after one of them is not attributed to chaos. Event
			// deliveries have no API request to attribute a failure to.
			if h.Scope != chaos.ScopeAPI || h.Action == chaos.ActionLatency || h.Action == chaos.ActionColdStart {
				continue
			}
			out[h.RequestID] = diagnose.Injection{RuleID: h.RuleID, Action: string(h.Action)}
		}
		return out
	}
}
