package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/control"
	"github.com/stratus/internal/operator"
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
)

// Services is the set of AWS service handlers for one account and region.
type Services struct {
	STS             *sts.Service
	S3              *s3.Service
	Lambda          *lambdasvc.Service
	APIGateway      *apigateway.Service
	DynamoDB        *dynamodb.Service
	CloudFormation  *cloudformation.Service
	IAM             *iam.Service
	SSM             *ssm.Service
	Logs            *logs.Service
	SNS             *sns.Service
	SQS             *sqs.Service
	Monitoring      *monitoring.Service
	KMS             *kms.Service
	Events          *eventssvc.Service
	SecretsManager  *secretsmanager.Service
	Kinesis         *kinesis.Service
	CognitoIDP      *cognitoidp.Service
	StepFunctions   *stepfunctions.Service
	APIGatewayV2    *apigatewayv2.Service
	ECR             *ecr.Service
	ECS             *ecs.Service
	ELBV2           *elbv2.Service
	EC2             *ec2.Service
	ACM             *acm.Service
	RDS             *rds.Service
	ElastiCache     *elasticache.Service
	DynamoDBStreams *dynamodbstreams.Service
	FIS             *fis.Service
	SES             *ses.Service
	Bedrock         *bedrock.Service
	Kafka           *kafka.Service
	Route53         *route53.Service
	CloudMap        *servicediscovery.Directory
	Scheduler       *scheduler.Service
}

// StackResolver returns the services for a scope.
type StackResolver interface {
	For(sc scope.Scope) *Services
}

type Options struct {
	Logger *slog.Logger
	// Services serves requests in the default account and region, and every
	// request when Stacks is nil.
	Services Services
	// Stacks, when set, picks the services for each request's account and
	// region.
	Stacks StackResolver
	// Accounts maps an access key to its account. Optional.
	Accounts AccountResolver
	// Authorizer, when set, decides whether the caller may make each AWS
	// API request, after faults and before dispatch.
	Authorizer Authorizer
	Operator   *operator.Service
	// Faults, when set, injects chaos faults into AWS API requests.
	Faults FaultDecider
	// ColdStart discards a Lambda function's warm container. Used by the
	// chaos cold_start action.
	ColdStart func(ctx context.Context, functionName string) error
	// Control serves /_stratus/control/v1. Optional.
	Control http.Handler
	// Gate, when set, admits each AWS API request so control operations
	// such as reset and restore never interleave with in-flight requests.
	Gate *control.Gate
}

type Server struct {
	logger     *slog.Logger
	services   *Services
	stacks     StackResolver
	accounts   AccountResolver
	operator   *operator.Service
	control    http.Handler
	gate       *control.Gate
	faults     FaultDecider
	coldStart  func(ctx context.Context, functionName string) error
	authorizer Authorizer
}

func NewServer(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	services := opts.Services
	return &Server{
		logger:     logger,
		services:   &services,
		stacks:     opts.Stacks,
		accounts:   opts.Accounts,
		authorizer: opts.Authorizer,
		operator:   opts.Operator,
		control:    opts.Control,
		gate:       opts.Gate,
		faults:     opts.Faults,
		coldStart:  opts.ColdStart,
	}
}

// servicesFor returns the services that serve sc.
func (s *Server) servicesFor(sc scope.Scope) *Services {
	if s.stacks == nil || sc.IsDefault() {
		return s.services
	}
	return s.stacks.For(sc)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.handleControl(w, r) {
		return
	}
	if s.handleOperator(w, r) {
		return
	}
	if s.gate != nil {
		release := s.gate.Admit()
		defer release()
	}

	start := time.Now()
	recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

	classification, err := Classify(r)
	if err != nil {
		classification = Classification{Protocol: ProtocolQuery}
	}

	metadata := RequestMetadata{
		RequestID:      uuid.NewString(),
		Classification: classification,
		SigV4:          awscompat.ParseSigV4Authorization(r.Header.Get("Authorization")),
	}
	metadata.Scope = ResolveScope(r, metadata.SigV4, s.accounts)

	recorder.Header().Set("x-amzn-requestid", metadata.RequestID)
	r = r.WithContext(scope.WithScope(WithRequestMetadata(r.Context(), metadata), metadata.Scope))
	if metadata.Classification.Protocol == ProtocolQuery && metadata.Classification.Service != "" && metadata.Classification.Operation == "" {
		s.logger.Debug("query request missing action",
			"service", metadata.Classification.Service,
			"content_type", r.Header.Get("Content-Type"),
			"x_amz_target", r.Header.Get("X-Amz-Target"),
			"body_preview", previewRequestBody(r),
		)
	}
	if metadata.Classification.Service == "s3" && r.Method == http.MethodPut {
		s.logger.Debug("s3 put request",
			"path", r.URL.Path,
			"content_encoding", r.Header.Get("Content-Encoding"),
			"content_md5", r.Header.Get("Content-MD5"),
			"x_amz_sdk_checksum_algorithm", r.Header.Get("X-Amz-Sdk-Checksum-Algorithm"),
			"x_amz_checksum_crc32", r.Header.Get("X-Amz-Checksum-Crc32"),
			"x_amz_checksum_crc32c", r.Header.Get("X-Amz-Checksum-Crc32C"),
			"x_amz_checksum_sha1", r.Header.Get("X-Amz-Checksum-Sha1"),
			"x_amz_checksum_sha256", r.Header.Get("X-Amz-Checksum-Sha256"),
			"x_amz_trailer", r.Header.Get("X-Amz-Trailer"),
			"x_amz_decoded_content_length", r.Header.Get("X-Amz-Decoded-Content-Length"),
		)
	}

	outcome := faultProceed
	if err == nil {
		outcome, err = s.injectFaults(recorder, r, metadata)
		if outcome == faultDrop {
			s.recordDropped(start, r, metadata)
			// Abort without a response: the client sees a reset connection.
			panic(http.ErrAbortHandler)
		}
	}
	if err == nil && outcome == faultProceed && s.authorizer != nil {
		err = s.authorizer.Authorize(r, metadata)
	}
	if err == nil && outcome == faultProceed {
		err = s.dispatch(recorder, r, metadata)
	}
	var apiErr *apierror.Error
	if err != nil {
		WriteError(recorder, r, err)
		var typed *apierror.Error
		if errors.As(err, &typed) {
			apiErr = typed
		} else {
			apiErr = &apierror.Error{
				StatusCode: http.StatusInternalServerError,
				Code:       "InternalFailure",
				Message:    "internal server error",
			}
		}
	}

	durationMS := time.Since(start).Milliseconds()
	s.logger.Info("request complete",
		"request_id", metadata.RequestID,
		"service", metadata.Classification.Service,
		"operation", metadata.Classification.Operation,
		"method", r.Method,
		"path", r.URL.Path,
		"status", recorder.statusCode,
		"duration_ms", durationMS,
	)
	if s.operator != nil {
		s.operator.Record(operator.RequestRecord{
			Time:         start.UTC(),
			RequestID:    metadata.RequestID,
			Service:      metadata.Classification.Service,
			Operation:    metadata.Classification.Operation,
			Method:       r.Method,
			Path:         r.URL.Path,
			Status:       recorder.statusCode,
			DurationMS:   durationMS,
			ErrorCode:    errorCode(apiErr),
			ErrorMessage: errorMessage(apiErr),
		})
	}
}

// handleControl serves the control API. It runs outside the gate because
// control operations take the gate exclusively themselves.
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) bool {
	if !control.Matches(r.URL.Path) {
		return false
	}
	if s.control == nil {
		WriteJSON(w, http.StatusNotImplemented, map[string]any{"error": map[string]string{
			"code":    "ControlAPIDisabled",
			"message": "the control API is not configured on this server",
		}})
		return true
	}
	s.control.ServeHTTP(w, r)
	return true
}

func (s *Server) handleOperator(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/_stratus" {
		http.Redirect(w, r, "/_stratus/", http.StatusTemporaryRedirect)
		return true
	}
	if r.URL.Path == "/_stratus/" {
		if s.operator == nil {
			WriteJSON(w, http.StatusNotImplemented, map[string]string{"error": "operator API is not configured"})
			return true
		}
		page, err := s.operator.PortalHTML()
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return true
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/_stratus/operator") {
		return false
	}
	if s.operator == nil {
		WriteJSON(w, http.StatusNotImplemented, map[string]string{"error": "operator API is not configured"})
		return true
	}
	status, payload := s.operator.Handle(w, r)
	WriteJSON(w, status, payload)
	return true
}

func errorCode(err *apierror.Error) string {
	if err == nil {
		return ""
	}
	return err.Code
}

func errorMessage(err *apierror.Error) string {
	if err == nil {
		return ""
	}
	return err.Message
}

func previewRequestBody(r *http.Request) string {
	if r == nil || r.Body == nil {
		return ""
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 512))
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
	return string(raw)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	svc := s.servicesFor(metadata.Scope)
	switch metadata.Classification.Service {
	case "stratus":
		WriteJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"service": "stratus",
		})
		return nil
	case "sts":
		return svc.handleSTS(w, r, metadata)
	case "s3":
		return svc.handleS3(w, r, metadata)
	case "lambda":
		return svc.handleLambda(w, r, metadata)
	case "apigateway":
		return svc.handleAPIGateway(w, r, metadata)
	case "dynamodb":
		return svc.handleDynamoDB(w, r, metadata)
	case "cloudformation":
		return svc.handleCloudFormation(w, r, metadata)
	case "iam":
		return svc.handleIAM(w, r, metadata)
	case "ssm":
		return svc.handleSSM(w, r, metadata)
	case "logs":
		return svc.handleLogs(w, r, metadata)
	case "sns":
		return svc.handleSNS(w, r, metadata)
	case "sqs":
		return svc.handleSQS(w, r, metadata)
	case "monitoring":
		return svc.handleMonitoring(w, r, metadata)
	case "kms":
		return svc.handleKMS(w, r, metadata)
	case "events":
		return svc.handleEvents(w, r, metadata)
	case "secretsmanager":
		return svc.handleSecretsManager(w, r, metadata)
	case "kinesis":
		return svc.handleKinesis(w, r, metadata)
	case "cognitoidp":
		return svc.handleCognitoIDP(w, r, metadata)
	case "stepfunctions":
		return svc.handleStepFunctions(w, r, metadata)
	case "apigatewayv2":
		return svc.handleAPIGatewayV2(w, r, metadata)
	case "ecr":
		return svc.handleECR(w, r, metadata)
	case "ecs":
		return svc.handleECS(w, r, metadata)
	case "elbv2":
		return svc.handleELBV2(w, r, metadata)
	case "ec2":
		return svc.handleEC2(w, r, metadata)
	case "acm":
		return svc.handleACM(w, r, metadata)
	case "rds":
		return svc.handleRDS(w, r, metadata)
	case "elasticache":
		return svc.handleElastiCache(w, r, metadata)
	case "dynamodbstreams":
		return svc.handleDynamoDBStreams(w, r, metadata)
	case "fis":
		if svc.FIS == nil {
			return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"fis\" is not implemented"}
		}
		return svc.FIS.Handle(w, r)
	case "ses":
		return svc.handleSES(w, r, metadata)
	case "scheduler":
		if svc.Scheduler == nil {
			return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "ResourceNotFoundException", Message: "service \"scheduler\" is not implemented"}
		}
		return svc.Scheduler.Handle(w, r)
	case "bedrock":
		if svc.Bedrock == nil {
			return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "UnknownOperationException", Message: "service \"bedrock\" is not implemented"}
		}
		return svc.Bedrock.Handle(w, r)
	case "kafka":
		if svc.Kafka == nil {
			return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotFoundException", Message: "service \"kafka\" is not implemented"}
		}
		return svc.Kafka.Handle(w, r)
	case "route53":
		if svc.Route53 == nil {
			return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"route53\" is not implemented"}
		}
		return svc.Route53.Handle(w, r)
	case "servicediscovery":
		if svc.CloudMap == nil {
			return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "UnknownOperationException", Message: "service \"servicediscovery\" is not implemented"}
		}
		return svc.CloudMap.Handle(w, r, metadata.Classification.Operation)
	case UnsupportedService:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "operation is not implemented: " + metadata.Classification.Operation,
		}
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    fmt.Sprintf("service %q is not implemented", metadata.Classification.Service),
		}
	}
}

func (s *Services) handleSTS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	return s.STS.Handle(w, r, metadata.Classification.Operation, metadata.RequestID, sts.Caller{
		AccessKeyID: accessKeyFromMetadata(metadata),
		Account:     metadata.Scope.Account,
	})
}

func (s *Services) handleS3(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.S3 == nil {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "service \"s3\" is not implemented",
		}
	}
	return s.S3.Handle(w, r, metadata.Classification.Bucket, metadata.Classification.Key)
}

func (s *Services) handleLambda(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.Lambda == nil {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "service \"lambda\" is not implemented",
		}
	}
	return s.Lambda.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleAPIGateway(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.APIGateway == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"apigateway\" is not implemented"}
	}
	return s.APIGateway.Handle(w, r)
}

func (s *Services) handleDynamoDB(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.DynamoDB == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"dynamodb\" is not implemented"}
	}
	return s.DynamoDB.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleCloudFormation(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.CloudFormation == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"cloudformation\" is not implemented"}
	}
	return s.CloudFormation.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleIAM(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.IAM == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"iam\" is not implemented"}
	}
	return s.IAM.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleSSM(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.SSM == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"ssm\" is not implemented"}
	}
	return s.SSM.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleLogs(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.Logs == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"logs\" is not implemented"}
	}
	return s.Logs.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleSNS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.SNS == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"sns\" is not implemented"}
	}
	return s.SNS.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleSQS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.SQS == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"sqs\" is not implemented"}
	}
	return s.SQS.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleMonitoring(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.Monitoring == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"monitoring\" is not implemented"}
	}
	return s.Monitoring.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleKMS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.KMS == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"kms\" is not implemented"}
	}
	return s.KMS.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleEvents(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.Events == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"events\" is not implemented"}
	}
	return s.Events.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleSecretsManager(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.SecretsManager == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"secretsmanager\" is not implemented"}
	}
	return s.SecretsManager.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleKinesis(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.Kinesis == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"kinesis\" is not implemented"}
	}
	return s.Kinesis.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleCognitoIDP(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.CognitoIDP == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"cognitoidp\" is not implemented"}
	}
	return s.CognitoIDP.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleStepFunctions(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.StepFunctions == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"stepfunctions\" is not implemented"}
	}
	return s.StepFunctions.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleAPIGatewayV2(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.APIGatewayV2 == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"apigatewayv2\" is not implemented"}
	}
	return s.APIGatewayV2.Handle(w, r)
}

func (s *Services) handleECR(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ECR == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"ecr\" is not implemented"}
	}
	return s.ECR.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleECS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ECS == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"ecs\" is not implemented"}
	}
	return s.ECS.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleELBV2(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ELBV2 == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"elbv2\" is not implemented"}
	}
	return s.ELBV2.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleEC2(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.EC2 == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"ec2\" is not implemented"}
	}
	return s.EC2.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleACM(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ACM == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"acm\" is not implemented"}
	}
	return s.ACM.Handle(w, r, metadata.Classification.Operation)
}

func (s *Services) handleRDS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.RDS == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"rds\" is not implemented"}
	}
	return s.RDS.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

// handleSES serves both SES APIs: v1 is a query API on "/", v2 is REST
// under /v2/email/.
func (s *Services) handleSES(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.SES == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"ses\" is not implemented"}
	}
	if metadata.Classification.Protocol == ProtocolQuery {
		return s.SES.HandleQuery(w, r, metadata.Classification.Operation, metadata.RequestID)
	}
	return s.SES.HandleV2(w, r)
}

func (s *Services) handleElastiCache(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ElastiCache == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"elasticache\" is not implemented"}
	}
	return s.ElastiCache.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Services) handleDynamoDBStreams(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.DynamoDBStreams == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"dynamodbstreams\" is not implemented"}
	}
	return s.DynamoDBStreams.Handle(w, r, metadata.Classification.Operation)
}

func accessKeyFromMetadata(metadata RequestMetadata) string {
	if metadata.SigV4 == nil {
		return ""
	}
	return metadata.SigV4.AccessKeyID
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

// Unwrap lets http.ResponseController reach the underlying writer, so
// streaming responses can flush.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
