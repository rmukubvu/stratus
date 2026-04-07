package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stratus/internal/config"
	"github.com/stratus/internal/container"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/services/cloudformation"
	"github.com/stratus/internal/services/dynamodb"
	"github.com/stratus/internal/services/iam"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/services/logs"
	"github.com/stratus/internal/services/s3"
	"github.com/stratus/internal/services/sqs"
	"github.com/stratus/internal/services/ssm"
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

	metaStore, err := bbolt.Open(filepath.Join(cfg.DataDir, "meta.db"))
	if err != nil {
		return nil, err
	}

	lambdaRuntime, err := container.NewManager(cfg.DataDir, blobRoot, logger)
	if err != nil {
		return nil, err
	}

	handler := httpapi.NewServer(httpapi.Options{
		Logger: logger,
		STS:    sts.NewService(),
		S3: s3.NewService(s3.Options{
			Metadata: metaStore,
			Blobs:    fsblob.New(blobRoot),
		}),
		Lambda: lambdasvc.NewService(lambdasvc.Options{
			Metadata: metaStore,
			Blobs:    fsblob.New(blobRoot),
			Invoker:  lambdaRuntime,
		}),
		DynamoDB:       dynamodb.NewService(metaStore),
		CloudFormation: cloudformation.NewService(metaStore),
		IAM:            iam.NewService(metaStore),
		SSM:            ssm.NewService(metaStore),
		Logs:           logs.NewService(metaStore),
		SQS:            sqs.NewService(metaStore),
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
		closers: []io.Closer{metaStore, lambdaRuntime},
	}, nil
}

func NewLogger(level string) *slog.Logger {
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

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slogLevel,
	}))
}

func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	a.logger.Info("starting stratus",
		"addr", a.server.Addr,
		"data_dir", a.cfg.DataDir,
		"log_level", a.cfg.LogLevel,
	)

	go func() {
		err := a.server.ListenAndServe()
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
