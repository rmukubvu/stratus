package app

import (
	"context"
	"fmt"
	"github.com/stratus/internal/validation"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/stratus/internal/advise"
	"github.com/stratus/internal/authn"
	"github.com/stratus/internal/authz"
	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/clock"
	"github.com/stratus/internal/config"
	"github.com/stratus/internal/container"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/diagnose"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/idle"
	"github.com/stratus/internal/limits"
	"github.com/stratus/internal/loadrun"
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
	daemon  *container.Daemon
	// idle is nil unless an idle timeout is set.
	idle *idle.Watcher
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

	reachSettings, err := container.ParseReachSettings(os.Getenv)
	if err != nil {
		return nil, err
	}
	daemon, err := container.Connect(cfg.DataDir, reachSettings, logger)
	if err != nil {
		return nil, err
	}
	lambdaRuntime, err := container.NewManager(daemon, cfg.DataDir, blobRoot, logger)
	if err != nil {
		return nil, err
	}
	engines := container.NewBacking(daemon, logger)

	iamMode, err := authz.ParseMode(os.Getenv("STRATUS_IAM"))
	if err != nil {
		return nil, err
	}
	sigv4Settings, err := sigv4SettingsFrom(cfg, os.Getenv)
	if err != nil {
		return nil, err
	}
	validate, err := cfg.ValidationEnabled(os.Getenv)
	if err != nil {
		return nil, err
	}
	bedrockConfig, err := bedrock.ConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	chaosEngine := chaos.NewEngine(control.DefaultCatalog().Names(), nil)
	governor, err := newGovernor(cfg, logger)
	if err != nil {
		return nil, err
	}
	blobStore := fsblob.New(blobRoot)
	emulatorClock := clock.New(metaStore, nil)
	iamKeys := iam.NewKeyDirectory(metaStore)
	stsService := sts.NewService(metaStore, iamKeys)
	lambdaRuntime.SetCredentialIssuer(stsService)
	lambdaRuntime.SetEndpoint(containerEndpoint(daemon.CallbackHost(), cfg.Port))
	if n := lambdaContainers(cfg); n > 0 {
		lambdaRuntime.SetMaxContainers(n)
	}
	governor.SetEmulatorStats(func() any { return map[string]any{"lambda": lambdaRuntime.PoolStats()} })
	deliveries := diagnose.NewDeliveryLog(0)
	bedrockService := bedrock.NewService(bedrock.Options{
		Metadata:   metaStore,
		FixtureDir: bedrockConfig.FixtureDir,
		Mode:       bedrockConfig.Mode,
		Upstream:   bedrock.UpstreamFromEnv(os.Getenv),
	})
	bedrockService.SetClock(emulatorClock.Now)
	stacks := newStacks(platform{
		meta:        metaStore,
		blobs:       blobStore,
		invoker:     lambdaRuntime,
		engines:     engines,
		faults:      newRecordingGate(chaos.NewInjector(chaosEngine, lambdaRuntime.CleanupFunction, logger), deliveries),
		chaos:       chaosEngine,
		bedrock:     bedrockService,
		sts:         stsService,
		iamKeys:     iamKeys,
		getenv:      os.Getenv,
		logger:      logger,
		capacity:    capacityGate{governor: governor},
		concurrency: concurrencyGate{governor: governor},
		now:         emulatorClock.Now,
	})
	emulatorClock.OnAdvance(func(ctx context.Context) (clock.Effect, error) {
		fired, err := stacks.FireDueSchedules(ctx)
		return clock.Effect{Source: "scheduler", Count: len(fired), Detail: fired}, err
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

	authenticator := authn.New(sigv4Settings.mode, authn.Directory{
		Sessions:   stsService,
		Keys:       iamKeys,
		Static:     sigv4Settings.static,
		RootSecret: sigv4Settings.rootSecret,
	}, logger, nil)

	// The control API's service views (SES inbox, scheduler clock, MSK
	// brokers, DNS) serve the default account and region.
	controlHandler := control.NewHandler(controlService)
	chaos.RegisterRoutes(controlHandler, chaosEngine)
	limits.RegisterRoutes(controlHandler, governor)
	authn.RegisterRoutes(controlHandler, authenticator)
	runs := loadrun.NewRecorder(governor, chaosEngine, nil)
	loadrun.RegisterRoutes(controlHandler, runs)
	ses.RegisterRoutes(controlHandler, defaults.ses)
	scheduler.RegisterRoutes(controlHandler, defaults.scheduler)
	clock.RegisterRoutes(controlHandler, emulatorClock)
	bedrock.RegisterRoutes(controlHandler, defaults.services.Bedrock)
	kafka.RegisterRoutes(controlHandler, defaults.kafka)
	route53.RegisterRoutes(controlHandler, defaults.route53)
	advise.RegisterRoutes(controlHandler, newAdviseService(metaStore, emulatorClock.Now))
	diagnose.RegisterRoutes(controlHandler, diagnose.NewService(
		failuresFrom(operatorStore, deliveries, chaosEngine),
		attributionsFrom(chaosEngine, governor, authenticator),
		nil,
	))

	handler := httpapi.NewServer(httpapi.Options{
		Logger:        logger,
		Services:      defaults.services,
		Stacks:        stacks,
		Accounts:      httpapi.AccountResolvers{stsService, iamKeys, sigv4Settings.static},
		Authenticator: authenticator,
		Authorizer:    authz.New(iamMode, authz.Callers{Sessions: stsService, Keys: iamKeys}, stacks, stacks, logger),
		Validator:     requestValidator(validate),
		Operator:      operatorService,
		Control:       controlHandler,
		Gate:          controlService.Gate(),
		Faults:        chaosEngine,
		Limits:        governor,
		Observer:      runObserver{runs},
		ColdStart:     lambdaRuntime.CleanupFunction,
	})

	var (
		watcher *idle.Watcher
		served  http.Handler = handler
	)
	if cfg.IdleTimeout > 0 {
		watcher = idle.NewWatcher(cfg.IdleTimeout, nil, isProbe)
		served = watcher.Middleware(handler)
	}

	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           served,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return &App{
		cfg:     cfg,
		logger:  logger,
		server:  server,
		closers: []io.Closer{stacks, metaStore, lambdaRuntime, engines, daemon},
		daemon:  daemon,
		idle:    watcher,
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

// ContainerEndpoint is Stratus's base URL as the containers it starts see
// it, for Lambda functions that call back into Stratus.
func (a *App) ContainerEndpoint() string {
	return containerEndpoint(a.daemon.CallbackHost(), a.cfg.Port)
}

func containerEndpoint(host string, port int) string {
	return fmt.Sprintf("http://%s:%d", host, port)
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
	ctx, stopIdleWatch := a.stopWhenIdle(ctx)
	defer stopIdleWatch()
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

// stopWhenIdle returns a context that also ends once the idle watcher sees
// no requests for the idle timeout, so Serve shuts down and releases every
// container exactly as it does on Ctrl-C.
func (a *App) stopWhenIdle(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if a.idle == nil {
		return ctx, cancel
	}
	go func() {
		if a.idle.Wait(ctx, idle.CheckInterval(a.idle.Timeout())) {
			a.logger.Info("no requests for the idle timeout; stopping", "idle_timeout", a.idle.Timeout().String())
			cancel()
		}
	}()
	return ctx, cancel
}

// isProbe reports health and readiness checks (the control API's and the
// container image's), which don't count as use.
func isProbe(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case control.PathPrefix + "/health", control.PathPrefix + "/ready", "/_stratus/health":
		return true
	}
	return false
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

// failuresFrom adapts the operator's request log, the failed event
// deliveries and the deliveries chaos failed to the diagnosis port, newest
// first.
func failuresFrom(store *operator.Store, deliveries *diagnose.DeliveryLog, engine *chaos.Engine) diagnose.FailureSource {
	return func() []diagnose.Failure {
		records := store.Errors(200)
		out := make([]diagnose.Failure, 0, len(records))
		for _, r := range records {
			out = append(out, diagnose.Failure{
				Time: r.Time, RequestID: r.RequestID, Service: r.Service, Operation: r.Operation,
				Method: r.Method, Path: r.Path, Status: r.Status, Code: r.ErrorCode, Message: r.ErrorMessage,
			})
		}
		out = append(out, deliveries.Failures()...)
		out = append(out, injectedDeliveryFailures(engine.Hits())...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
		return out
	}
}

// attributionsFrom adapts chaos hits and limit breaches to the diagnosis
// port, so a failure Stratus caused on purpose is explained as such.
func attributionsFrom(engine *chaos.Engine, governor *limits.Governor, authenticator *authn.Authenticator) diagnose.InjectionSource {
	return func() map[string]diagnose.Injection {
		hits := engine.Hits()
		breaches := governor.Breaches()
		out := make(map[string]diagnose.Injection, len(hits)+len(breaches))
		for id, rec := range authenticator.Refusals() {
			out[id] = diagnose.Injection{RuleID: rec.Kind, Action: rec.Code, Source: diagnose.SourceSignature, Detail: rec.Reason}
		}
		for _, b := range breaches {
			if b.RequestID != "" {
				out[b.RequestID] = diagnose.Injection{RuleID: b.RuleID, Action: string(b.Kind), Source: diagnose.SourceLimit}
			}
		}
		for _, h := range hits {
			// Latency, cold starts and duplicates don't fail a request or
			// delivery by themselves, so a failure after one of them is not
			// attributed to chaos. A delivery's hit carries its delivery ID.
			if !failsDelivery(h.Action) || h.RequestID == "" {
				continue
			}
			out[h.RequestID] = diagnose.Injection{RuleID: h.RuleID, Action: string(h.Action)}
		}
		return out
	}
}

// lambdaContainers reads --lambda-containers, then
// STRATUS_LAMBDA_CONTAINERS; zero keeps the default.
func lambdaContainers(cfg config.Config) int {
	if cfg.LambdaContainers > 0 {
		return cfg.LambdaContainers
	}
	n, _ := strconv.Atoi(os.Getenv("STRATUS_LAMBDA_CONTAINERS"))
	return n
}

// newGovernor builds the limits governor and applies the startup profile
// from --limits or STRATUS_LIMITS, if any.
func newGovernor(cfg config.Config, logger *slog.Logger) (*limits.Governor, error) {
	governor := limits.NewGovernor(control.DefaultCatalog().Names(), nil)
	setting := cfg.Limits
	if setting == "" {
		setting = os.Getenv("STRATUS_LIMITS")
	}
	spec, ok, err := limits.ParseSetting(setting, os.ReadFile)
	if err != nil || !ok {
		return governor, err
	}
	status, err := governor.Apply(spec)
	if err != nil {
		return nil, fmt.Errorf("limits profile: %w", err)
	}
	logger.Info("limits profile applied", "profile", status.Profile, "scale", status.Scale, "rules", len(status.Rules))
	return governor, nil
}

// sigv4Settings is how strict signature mode starts.
type sigv4Settings struct {
	mode       authn.Mode
	static     authn.StaticCredentials
	rootSecret string
}

// sigv4SettingsFrom reads --sigv4 (or STRATUS_SIGV4),
// STRATUS_SIGV4_CREDENTIALS and STRATUS_SIGV4_ROOT_SECRET.
func sigv4SettingsFrom(cfg config.Config, getenv func(string) string) (sigv4Settings, error) {
	raw := cfg.SigV4
	if raw == "" {
		raw = getenv("STRATUS_SIGV4")
	}
	mode, err := authn.ParseMode(raw)
	if err != nil {
		return sigv4Settings{}, err
	}
	static, err := authn.ParseStaticCredentials(getenv("STRATUS_SIGV4_CREDENTIALS"))
	if err != nil {
		return sigv4Settings{}, err
	}
	return sigv4Settings{mode: mode, static: static, rootSecret: getenv("STRATUS_SIGV4_ROOT_SECRET")}, nil
}

// requestValidator is the validator for the server, or nil when validation
// is off (a typed nil would still be called).
func requestValidator(enabled bool) httpapi.RequestValidator {
	if !enabled {
		return nil
	}
	return validation.New()
}
