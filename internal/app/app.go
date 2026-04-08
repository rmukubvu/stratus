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
	"github.com/stratus/internal/services/acm"
	"github.com/stratus/internal/services/apigateway"
	"github.com/stratus/internal/services/apigatewayv2"
	"github.com/stratus/internal/services/cloudformation"
	"github.com/stratus/internal/services/cognitoidp"
	"github.com/stratus/internal/services/dynamodb"
	"github.com/stratus/internal/services/dynamodbstreams"
	"github.com/stratus/internal/services/ecr"
	"github.com/stratus/internal/services/ecs"
	"github.com/stratus/internal/services/elasticache"
	"github.com/stratus/internal/services/elbv2"
	eventssvc "github.com/stratus/internal/services/events"
	"github.com/stratus/internal/services/iam"
	"github.com/stratus/internal/services/kinesis"
	"github.com/stratus/internal/services/kms"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/services/logs"
	"github.com/stratus/internal/services/monitoring"
	"github.com/stratus/internal/services/rds"
	"github.com/stratus/internal/services/s3"
	"github.com/stratus/internal/services/secretsmanager"
	"github.com/stratus/internal/services/sns"
	"github.com/stratus/internal/services/sqs"
	"github.com/stratus/internal/services/ssm"
	"github.com/stratus/internal/services/stepfunctions"
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

	blobStore := fsblob.New(blobRoot)
	lambdaService := lambdasvc.NewService(lambdasvc.Options{
		Metadata: metaStore,
		Blobs:    blobStore,
		Invoker:  lambdaRuntime,
	})
	snsService := sns.NewService(metaStore)
	sqsService := sqs.NewService(metaStore)
	streamsService := dynamodbstreams.NewService(metaStore)
	dynamoDBService := dynamodb.NewService(metaStore)
	s3Service := s3.NewService(s3.Options{
		Metadata: metaStore,
		Blobs:    blobStore,
	})
	sqsService.SetLambda(lambdaService)
	snsService.SetQueuePublisher(sqsService)
	snsService.SetLambdaInvoker(lambdaService)
	lambdaService.SetQueuePublisher(sqsService)
	lambdaService.SetTopicPublisher(snsService)
	streamsService.SetLambda(lambdaService)
	dynamoDBService.SetStreams(streamsService)
	apiGatewayV2Service := apigatewayv2.NewService(apigatewayv2.Options{
		Metadata: metaStore,
		Lambda:   lambdaService,
	})
	apiGatewayService := apigateway.NewService(apigateway.Options{
		Metadata: metaStore,
		Lambda:   lambdaService,
	})
	eventsService := eventssvc.NewService(metaStore, lambdaService)
	eventsService.SetSNS(snsService)
	eventsService.SetSQS(sqsService)
	kinesisService := kinesis.NewService(metaStore)
	kinesisService.SetLambda(lambdaService)
	cognitoIDPService := cognitoidp.NewService(metaStore)
	stepFunctionsService := stepfunctions.NewService(metaStore, lambdaService)
	eventsService.SetStepFunctions(stepFunctionsService)
	ecrService := ecr.NewService(metaStore)
	ecsService := ecs.NewService(metaStore)
	elbv2Service := elbv2.NewService(metaStore)
	acmService := acm.NewService(metaStore)
	rdsService := rds.NewService(metaStore)
	elastiCacheService := elasticache.NewService(metaStore)

	handler := httpapi.NewServer(httpapi.Options{
		Logger:     logger,
		STS:        sts.NewService(),
		S3:         s3Service,
		Lambda:     lambdaService,
		APIGateway: apiGatewayService,
		DynamoDB:   dynamoDBService,
		CloudFormation: cloudformation.NewService(cloudformation.Options{
			Metadata:       metaStore,
			Lambda:         lambdaService,
			APIGateway:     apiGatewayService,
			APIGatewayV2:   apiGatewayV2Service,
			S3:             s3Service,
			SNS:            snsService,
			Events:         eventsService,
			SecretsManager: secretsmanager.NewService(metaStore),
			Kinesis:        kinesisService,
			CognitoIDP:     cognitoIDPService,
			StepFunctions:  stepFunctionsService,
			SSM:            ssm.NewService(metaStore),
			KMS:            kms.NewService(metaStore),
		}),
		IAM:             iam.NewService(metaStore),
		SSM:             ssm.NewService(metaStore),
		Logs:            logs.NewService(metaStore),
		SNS:             snsService,
		SQS:             sqsService,
		Monitoring:      monitoring.NewService(metaStore),
		KMS:             kms.NewService(metaStore),
		Events:          eventsService,
		SecretsManager:  secretsmanager.NewService(metaStore),
		Kinesis:         kinesisService,
		CognitoIDP:      cognitoIDPService,
		StepFunctions:   stepFunctionsService,
		APIGatewayV2:    apiGatewayV2Service,
		ECR:             ecrService,
		ECS:             ecsService,
		ELBV2:           elbv2Service,
		ACM:             acmService,
		RDS:             rdsService,
		ElastiCache:     elastiCacheService,
		DynamoDBStreams: streamsService,
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

func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	a.logger.Info("starting stratus",
		"addr", a.server.Addr,
		"data_dir", a.cfg.DataDir,
		"log_level", a.cfg.LogLevel,
		"log_format", a.cfg.LogFormat,
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
