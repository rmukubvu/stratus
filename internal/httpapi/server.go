package httpapi

import (
	"bytes"
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
	"github.com/stratus/internal/operator"
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
)

type Options struct {
	Logger          *slog.Logger
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
	ACM             *acm.Service
	RDS             *rds.Service
	ElastiCache     *elasticache.Service
	DynamoDBStreams *dynamodbstreams.Service
	Operator        *operator.Service
}

type Server struct {
	logger          *slog.Logger
	sts             *sts.Service
	s3              *s3.Service
	lambda          *lambdasvc.Service
	apiGateway      *apigateway.Service
	dynamodb        *dynamodb.Service
	cloudformation  *cloudformation.Service
	iam             *iam.Service
	ssm             *ssm.Service
	logs            *logs.Service
	sns             *sns.Service
	sqs             *sqs.Service
	monitoring      *monitoring.Service
	kms             *kms.Service
	events          *eventssvc.Service
	secretsManager  *secretsmanager.Service
	kinesis         *kinesis.Service
	cognitoIDP      *cognitoidp.Service
	stepFunctions   *stepfunctions.Service
	apiGatewayV2    *apigatewayv2.Service
	ecr             *ecr.Service
	ecs             *ecs.Service
	elbv2           *elbv2.Service
	acm             *acm.Service
	rds             *rds.Service
	elastiCache     *elasticache.Service
	dynamodbStreams *dynamodbstreams.Service
	operator        *operator.Service
}

func NewServer(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{
		logger:          logger,
		sts:             opts.STS,
		s3:              opts.S3,
		lambda:          opts.Lambda,
		apiGateway:      opts.APIGateway,
		dynamodb:        opts.DynamoDB,
		cloudformation:  opts.CloudFormation,
		iam:             opts.IAM,
		ssm:             opts.SSM,
		logs:            opts.Logs,
		sns:             opts.SNS,
		sqs:             opts.SQS,
		monitoring:      opts.Monitoring,
		kms:             opts.KMS,
		events:          opts.Events,
		secretsManager:  opts.SecretsManager,
		kinesis:         opts.Kinesis,
		cognitoIDP:      opts.CognitoIDP,
		stepFunctions:   opts.StepFunctions,
		apiGatewayV2:    opts.APIGatewayV2,
		ecr:             opts.ECR,
		ecs:             opts.ECS,
		elbv2:           opts.ELBV2,
		acm:             opts.ACM,
		rds:             opts.RDS,
		elastiCache:     opts.ElastiCache,
		dynamodbStreams: opts.DynamoDBStreams,
		operator:        opts.Operator,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.handleOperator(w, r) {
		return
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

	recorder.Header().Set("x-amzn-requestid", metadata.RequestID)
	r = r.WithContext(WithRequestMetadata(r.Context(), metadata))
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

	if err == nil {
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
	switch metadata.Classification.Service {
	case "stratus":
		WriteJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"service": "stratus",
		})
		return nil
	case "sts":
		return s.handleSTS(w, metadata)
	case "s3":
		return s.handleS3(w, r, metadata)
	case "lambda":
		return s.handleLambda(w, r, metadata)
	case "apigateway":
		return s.handleAPIGateway(w, r, metadata)
	case "dynamodb":
		return s.handleDynamoDB(w, r, metadata)
	case "cloudformation":
		return s.handleCloudFormation(w, r, metadata)
	case "iam":
		return s.handleIAM(w, r, metadata)
	case "ssm":
		return s.handleSSM(w, r, metadata)
	case "logs":
		return s.handleLogs(w, r, metadata)
	case "sns":
		return s.handleSNS(w, r, metadata)
	case "sqs":
		return s.handleSQS(w, r, metadata)
	case "monitoring":
		return s.handleMonitoring(w, r, metadata)
	case "kms":
		return s.handleKMS(w, r, metadata)
	case "events":
		return s.handleEvents(w, r, metadata)
	case "secretsmanager":
		return s.handleSecretsManager(w, r, metadata)
	case "kinesis":
		return s.handleKinesis(w, r, metadata)
	case "cognitoidp":
		return s.handleCognitoIDP(w, r, metadata)
	case "stepfunctions":
		return s.handleStepFunctions(w, r, metadata)
	case "apigatewayv2":
		return s.handleAPIGatewayV2(w, r, metadata)
	case "ecr":
		return s.handleECR(w, r, metadata)
	case "ecs":
		return s.handleECS(w, r, metadata)
	case "elbv2":
		return s.handleELBV2(w, r, metadata)
	case "acm":
		return s.handleACM(w, r, metadata)
	case "rds":
		return s.handleRDS(w, r, metadata)
	case "elasticache":
		return s.handleElastiCache(w, r, metadata)
	case "dynamodbstreams":
		return s.handleDynamoDBStreams(w, r, metadata)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    fmt.Sprintf("service %q is not implemented", metadata.Classification.Service),
		}
	}
}

func (s *Server) handleSTS(w http.ResponseWriter, metadata RequestMetadata) error {
	switch metadata.Classification.Operation {
	case "GetCallerIdentity":
		output := s.sts.GetCallerIdentity(sts.GetCallerIdentityInput{
			AccessKeyID: accessKeyFromMetadata(metadata),
		})
		WriteXML(w, http.StatusOK, sts.NewGetCallerIdentityResponse(output, metadata.RequestID))
		return nil
	case "":
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "InvalidAction",
			Message:    "missing Action for STS query request",
		}
	default:
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "InvalidAction",
			Message:    "The action " + metadata.Classification.Operation + " is not valid for this endpoint.",
		}
	}
}

func (s *Server) handleS3(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.s3 == nil {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "service \"s3\" is not implemented",
		}
	}
	return s.s3.Handle(w, r, metadata.Classification.Bucket, metadata.Classification.Key)
}

func (s *Server) handleLambda(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.lambda == nil {
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "service \"lambda\" is not implemented",
		}
	}
	return s.lambda.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleAPIGateway(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.apiGateway == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"apigateway\" is not implemented"}
	}
	return s.apiGateway.Handle(w, r)
}

func (s *Server) handleDynamoDB(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.dynamodb == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"dynamodb\" is not implemented"}
	}
	return s.dynamodb.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleCloudFormation(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.cloudformation == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"cloudformation\" is not implemented"}
	}
	return s.cloudformation.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleIAM(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.iam == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"iam\" is not implemented"}
	}
	return s.iam.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleSSM(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ssm == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"ssm\" is not implemented"}
	}
	return s.ssm.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.logs == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"logs\" is not implemented"}
	}
	return s.logs.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleSNS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.sns == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"sns\" is not implemented"}
	}
	return s.sns.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleSQS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.sqs == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"sqs\" is not implemented"}
	}
	return s.sqs.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleMonitoring(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.monitoring == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"monitoring\" is not implemented"}
	}
	return s.monitoring.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleKMS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.kms == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"kms\" is not implemented"}
	}
	return s.kms.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.events == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"events\" is not implemented"}
	}
	return s.events.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleSecretsManager(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.secretsManager == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"secretsmanager\" is not implemented"}
	}
	return s.secretsManager.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleKinesis(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.kinesis == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"kinesis\" is not implemented"}
	}
	return s.kinesis.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleCognitoIDP(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.cognitoIDP == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"cognitoidp\" is not implemented"}
	}
	return s.cognitoIDP.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleStepFunctions(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.stepFunctions == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"stepfunctions\" is not implemented"}
	}
	return s.stepFunctions.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleAPIGatewayV2(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.apiGatewayV2 == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"apigatewayv2\" is not implemented"}
	}
	return s.apiGatewayV2.Handle(w, r)
}

func (s *Server) handleECR(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ecr == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"ecr\" is not implemented"}
	}
	return s.ecr.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleECS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.ecs == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"ecs\" is not implemented"}
	}
	return s.ecs.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleELBV2(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.elbv2 == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"elbv2\" is not implemented"}
	}
	return s.elbv2.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleACM(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.acm == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"acm\" is not implemented"}
	}
	return s.acm.Handle(w, r, metadata.Classification.Operation)
}

func (s *Server) handleRDS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.rds == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"rds\" is not implemented"}
	}
	return s.rds.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleElastiCache(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.elastiCache == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"elasticache\" is not implemented"}
	}
	return s.elastiCache.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
}

func (s *Server) handleDynamoDBStreams(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.dynamodbStreams == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "service \"dynamodbstreams\" is not implemented"}
	}
	return s.dynamodbStreams.Handle(w, r, metadata.Classification.Operation)
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
