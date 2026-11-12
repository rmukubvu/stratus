package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/chaos"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/httpapi"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/services/acm"
	"github.com/stratus/internal/services/apigateway"
	"github.com/stratus/internal/services/apigatewayv2"
	"github.com/stratus/internal/services/bedrock"
	"github.com/stratus/internal/services/cloudformation"
	"github.com/stratus/internal/services/cognitoidp"
	"github.com/stratus/internal/services/dynamodb"
	"github.com/stratus/internal/services/dynamodbstreams"
	"github.com/stratus/internal/services/ec2"
	"github.com/stratus/internal/services/ecr"
	"github.com/stratus/internal/services/ecs"
	"github.com/stratus/internal/services/elasticache"
	"github.com/stratus/internal/services/elbv2"
	eventssvc "github.com/stratus/internal/services/events"
	"github.com/stratus/internal/services/fis"
	"github.com/stratus/internal/services/iam"
	"github.com/stratus/internal/services/kafka"
	"github.com/stratus/internal/services/kinesis"
	"github.com/stratus/internal/services/kms"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/services/logs"
	"github.com/stratus/internal/services/monitoring"
	"github.com/stratus/internal/services/rds"
	"github.com/stratus/internal/services/route53"
	"github.com/stratus/internal/services/s3"
	"github.com/stratus/internal/services/scheduler"
	"github.com/stratus/internal/services/secretsmanager"
	"github.com/stratus/internal/services/servicediscovery"
	"github.com/stratus/internal/services/ses"
	"github.com/stratus/internal/services/sns"
	"github.com/stratus/internal/services/sqs"
	"github.com/stratus/internal/services/ssm"
	"github.com/stratus/internal/services/stepfunctions"
	"github.com/stratus/internal/services/sts"
	"github.com/stratus/internal/store"
	"github.com/stratus/internal/store/bbolt"
	"github.com/stratus/internal/store/fsblob"
)

// platform is what every account and region shares: the database, blob
// root, container runtimes and chaos engine.
type platform struct {
	meta    *bbolt.Store
	blobs   *fsblob.Store
	invoker lambdasvc.Invoker
	engines backing.Runner
	faults  *chaos.Injector
	chaos   *chaos.Engine
	bedrock *bedrock.Service
	sts     *sts.Service
	iamKeys *iam.KeyDirectory
	getenv  func(string) string
	logger  *slog.Logger
}

// accountServices are global AWS services: one instance per account, shared
// by every region of it.
type accountServices struct {
	iam     *iam.Service
	s3      *s3.Service
	route53 *route53.Service
}

// stack is the full set of services for one account and region.
type stack struct {
	scope     scope.Scope
	services  httpapi.Services
	scheduler *scheduler.Service
	logs      *logs.Service
	ses       *ses.Service
	kafka     *kafka.Service
	route53   *route53.Service
	backends  map[string]control.Backend
}

// Stacks builds and keeps one stack per account and region, on first use.
// The default scope's stack keeps the storage layout Stratus has always
// used; every other scope's metadata buckets and blobs are prefixed with
// scope.Prefix inside the same database and blob root, so snapshots, seeds
// and reset cover every account and region at once.
type Stacks struct {
	platform platform

	mu       sync.Mutex
	accounts map[string]*accountServices
	stacks   map[scope.Scope]*stack
}

func newStacks(p platform) *Stacks {
	return &Stacks{
		platform: p,
		accounts: map[string]*accountServices{},
		stacks:   map[scope.Scope]*stack{},
	}
}

// For returns the services for sc, building them on first use.
func (s *Stacks) For(sc scope.Scope) *httpapi.Services {
	return &s.get(sc).services
}

// Default is the stack of the default account and region.
func (s *Stacks) Default() *stack {
	return s.get(scope.Default())
}

func (s *Stacks) get(sc scope.Scope) *stack {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.stacks[sc]; ok {
		return st
	}
	st := s.build(sc, s.account(sc.Account))
	s.stacks[sc] = st
	if !sc.IsDefault() {
		s.platform.logger.Info("serving a new account and region", "account", sc.Account, "region", sc.Region)
	}
	return st
}

// Restore builds the stacks of every account and region that has data, so
// their schedules run and their engines come back after a restart or seed.
func (s *Stacks) Restore() error {
	names, err := s.platform.meta.BucketNames()
	if err != nil {
		return err
	}
	for _, sc := range scopesIn(names) {
		s.get(sc)
	}
	return nil
}

// scopesIn lists the non-default scopes that own any of the buckets, sorted.
func scopesIn(buckets []string) []scope.Scope {
	seen := map[scope.Scope]bool{}
	var out []scope.Scope
	for _, name := range buckets {
		sc, _ := scope.SplitPrefixed(name)
		if sc.IsDefault() || seen[sc] {
			continue
		}
		seen[sc] = true
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// all returns every stack built so far, default first.
func (s *Stacks) all() []*stack {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*stack, 0, len(s.stacks))
	for _, st := range s.stacks {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].scope.IsDefault() != out[j].scope.IsDefault() {
			return out[i].scope.IsDefault()
		}
		return out[i].scope.String() < out[j].scope.String()
	})
	return out
}

// Close stops every stack's background work.
func (s *Stacks) Close() error {
	var first error
	for _, st := range s.all() {
		if err := st.scheduler.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

var _ io.Closer = (*Stacks)(nil)

// UserPolicies returns an IAM user's identity-based policy documents.
func (s *Stacks) UserPolicies(account, userName string) ([]string, error) {
	return s.iam(account).UserPolicies(userName)
}

// RolePolicies returns an IAM role's identity-based policy documents.
func (s *Stacks) RolePolicies(account, roleName string) ([]string, error) {
	return s.iam(account).RolePolicies(roleName)
}

func (s *Stacks) iam(account string) *iam.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account(account).iam
}

// account returns the global services of an account. Callers hold s.mu.
func (s *Stacks) account(id string) *accountServices {
	if acct, ok := s.accounts[id]; ok {
		return acct
	}
	sc := scope.Scope{Account: id, Region: scope.DefaultRegion}
	meta := store.Scoped(s.platform.meta, scope.Prefix(sc))
	acct := &accountServices{
		iam:     iam.NewService(meta, s.platform.iamKeys),
		s3:      s3.NewService(s3.Options{Metadata: meta, Blobs: s.platform.blobs.Scoped(scope.Prefix(sc))}),
		route53: route53.NewService(route53.Options{Metadata: meta}),
	}
	acct.iam.SetScope(sc)
	acct.s3.SetScope(sc)
	s.accounts[id] = acct
	return acct
}

// build wires one account and region's services together.
func (s *Stacks) build(sc scope.Scope, acct *accountServices) *stack {
	p := s.platform
	prefix := scope.Prefix(sc)
	meta := store.Scoped(p.meta, prefix)
	blobs := p.blobs.Scoped(prefix)
	engines := backing.Scoped(p.engines, engineTag(sc))

	lambdaService := lambdasvc.NewService(lambdasvc.Options{Metadata: meta, Blobs: blobs, Invoker: p.invoker})
	snsService := sns.NewService(meta)
	sqsService := sqs.NewService(meta)
	streamsService := dynamodbstreams.NewService(meta)
	dynamoDBService := dynamodb.NewService(meta)
	apiGatewayV2Service := apigatewayv2.NewService(apigatewayv2.Options{Metadata: meta, Lambda: lambdaService})
	apiGatewayService := apigateway.NewService(apigateway.Options{Metadata: meta, Lambda: lambdaService})
	eventsService := eventssvc.NewService(meta, lambdaService)
	kinesisService := kinesis.NewService(meta)
	cognitoIDPService := cognitoidp.NewService(meta)
	stepFunctionsService := stepfunctions.NewService(meta, lambdaService)
	sesService := ses.NewService(ses.Options{Metadata: meta})
	schedulerService := scheduler.NewService(scheduler.Options{Metadata: meta, Logger: p.logger})
	ecrService := ecr.NewService(meta)
	ecsService := ecs.NewService(meta)
	elbv2Service := elbv2.NewService(meta)
	ec2Service := ec2.NewService(meta)
	monitoringService := monitoring.NewService(meta)
	acmService := acm.NewService(meta)
	ssmService := ssm.NewService(meta)
	kmsService := kms.NewService(meta)
	secretsService := secretsmanager.NewService(meta)
	logsService := logs.NewService(meta)
	rdsService := rds.NewService(rds.Options{Metadata: meta, Engines: engines, Catalog: rds.DefaultEngines(p.getenv)})
	elastiCacheService := elasticache.NewService(elasticache.Options{Metadata: meta, Engines: engines, Catalog: elasticache.DefaultEngines(p.getenv)})
	kafkaService := kafka.NewService(kafka.Options{Metadata: meta, Engines: engines, Engine: kafka.DefaultEngine(p.getenv)})
	cloudMapService := servicediscovery.NewDirectory(servicediscovery.Options{Metadata: meta, Zones: acct.route53})
	fisService := fis.NewService(meta, p.chaos)

	for _, svc := range []interface{ SetScope(scope.Scope) }{
		lambdaService, snsService, sqsService, streamsService, dynamoDBService, eventsService,
		kinesisService, cognitoIDPService, stepFunctionsService, sesService, schedulerService,
		ecrService, ecsService, elbv2Service, ec2Service, monitoringService, acmService, ssmService, kmsService, secretsService,
		logsService, rdsService, elastiCacheService, kafkaService, cloudMapService, fisService,
	} {
		svc.SetScope(sc)
	}

	sqsService.SetLambda(lambdaService)
	snsService.SetQueuePublisher(sqsService)
	snsService.SetLambdaInvoker(lambdaService)
	monitoringService.SetSNS(snsService)
	lambdaService.SetQueuePublisher(sqsService)
	lambdaService.SetTopicPublisher(snsService)
	streamsService.SetLambda(lambdaService)
	dynamoDBService.SetStreams(streamsService)
	eventsService.SetSNS(snsService)
	eventsService.SetSQS(sqsService)
	eventsService.SetStepFunctions(stepFunctionsService)
	kinesisService.SetLambda(lambdaService)
	schedulerService.SetLambda(lambdaService)
	schedulerService.SetSQS(sqsService)
	schedulerService.SetSNS(snsService)
	schedulerService.SetStepFunctions(stepFunctionsService)

	lambdaService.SetFaults(p.faults)
	snsService.SetFaults(p.faults)
	eventsService.SetFaults(p.faults)
	stepFunctionsService.SetFaults(p.faults)
	schedulerService.SetFaults(p.faults)
	schedulerService.Start()

	cloudFormationService := cloudformation.NewService(cloudformation.Options{
		Metadata:       meta,
		Lambda:         lambdaService,
		APIGateway:     apiGatewayService,
		APIGatewayV2:   apiGatewayV2Service,
		S3:             acct.s3,
		SNS:            snsService,
		Events:         eventsService,
		SecretsManager: secretsService,
		Kinesis:        kinesisService,
		CognitoIDP:     cognitoIDPService,
		StepFunctions:  stepFunctionsService,
		SSM:            ssmService,
		KMS:            kmsService,
	})
	cloudFormationService.SetScope(sc)

	return &stack{
		scope: sc,
		services: httpapi.Services{
			STS:             p.sts,
			S3:              acct.s3,
			Lambda:          lambdaService,
			APIGateway:      apiGatewayService,
			DynamoDB:        dynamoDBService,
			CloudFormation:  cloudFormationService,
			IAM:             acct.iam,
			SSM:             ssmService,
			Logs:            logsService,
			SNS:             snsService,
			SQS:             sqsService,
			Monitoring:      monitoringService,
			KMS:             kmsService,
			Events:          eventsService,
			SecretsManager:  secretsService,
			Kinesis:         kinesisService,
			CognitoIDP:      cognitoIDPService,
			StepFunctions:   stepFunctionsService,
			APIGatewayV2:    apiGatewayV2Service,
			ECR:             ecrService,
			ECS:             ecsService,
			ELBV2:           elbv2Service,
			EC2:             ec2Service,
			ACM:             acmService,
			RDS:             rdsService,
			ElastiCache:     elastiCacheService,
			DynamoDBStreams: streamsService,
			FIS:             fisService,
			SES:             sesService,
			Bedrock:         p.bedrock,
			Kafka:           kafkaService,
			Route53:         acct.route53,
			CloudMap:        cloudMapService,
			Scheduler:       schedulerService,
		},
		scheduler: schedulerService,
		logs:      logsService,
		ses:       sesService,
		kafka:     kafkaService,
		route53:   acct.route53,
		backends: map[string]control.Backend{
			rdsService.Service():         rdsService,
			elastiCacheService.Service(): elastiCacheService,
			kafkaService.Service():       kafkaService,
		},
	}
}

// engineTag keeps engine containers of different scopes apart.
func engineTag(sc scope.Scope) string {
	if sc.IsDefault() {
		return ""
	}
	return sc.Account + "." + sc.Region
}

// scopedBackend presents one service's engine backends, across every account
// and region, to the control plane as a single backend. Each scope's engine
// data lives in a subdirectory named by scope.Prefix.
type scopedBackend struct {
	service string
	stacks  *Stacks
}

// scopedBackends returns one control backend per engine-backed service.
func (s *Stacks) scopedBackends() []control.Backend {
	names := make([]string, 0, 3)
	for name := range s.Default().backends {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]control.Backend, 0, len(names))
	for _, name := range names {
		out = append(out, scopedBackend{service: name, stacks: s})
	}
	return out
}

func (b scopedBackend) Service() string { return b.service }

func (b scopedBackend) each(fn func(*stack, control.Backend) error) error {
	for _, st := range b.stacks.all() {
		if backend, ok := st.backends[b.service]; ok {
			if err := fn(st, backend); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b scopedBackend) Reset(ctx context.Context) error {
	return b.each(func(_ *stack, backend control.Backend) error { return backend.Reset(ctx) })
}

func (b scopedBackend) Reconcile(ctx context.Context, opts backing.ReconcileOptions) error {
	return b.each(func(st *stack, backend control.Backend) error {
		scoped := opts
		if opts.DataDir != "" {
			scoped.DataDir = filepath.Join(opts.DataDir, filepath.FromSlash(scope.Prefix(st.scope)))
		}
		return backend.Reconcile(ctx, scoped)
	})
}

func (b scopedBackend) Capture(ctx context.Context, dir string) ([]string, error) {
	var captured []string
	err := b.each(func(st *stack, backend control.Backend) error {
		prefix := scope.Prefix(st.scope)
		sub := filepath.Join(dir, filepath.FromSlash(prefix))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			return err
		}
		names, err := backend.Capture(ctx, sub)
		for _, name := range names {
			captured = append(captured, prefix+name)
		}
		return err
	})
	return captured, err
}
