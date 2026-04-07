package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/services/cloudformation"
	"github.com/stratus/internal/services/dynamodb"
	"github.com/stratus/internal/services/iam"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/services/logs"
	"github.com/stratus/internal/services/s3"
	"github.com/stratus/internal/services/sqs"
	"github.com/stratus/internal/services/ssm"
	"github.com/stratus/internal/services/sts"
)

type Options struct {
	Logger         *slog.Logger
	STS            *sts.Service
	S3             *s3.Service
	Lambda         *lambdasvc.Service
	DynamoDB       *dynamodb.Service
	CloudFormation *cloudformation.Service
	IAM            *iam.Service
	SSM            *ssm.Service
	Logs           *logs.Service
	SQS            *sqs.Service
}

type Server struct {
	logger         *slog.Logger
	sts            *sts.Service
	s3             *s3.Service
	lambda         *lambdasvc.Service
	dynamodb       *dynamodb.Service
	cloudformation *cloudformation.Service
	iam            *iam.Service
	ssm            *ssm.Service
	logs           *logs.Service
	sqs            *sqs.Service
}

func NewServer(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{
		logger:         logger,
		sts:            opts.STS,
		s3:             opts.S3,
		lambda:         opts.Lambda,
		dynamodb:       opts.DynamoDB,
		cloudformation: opts.CloudFormation,
		iam:            opts.IAM,
		ssm:            opts.SSM,
		logs:           opts.Logs,
		sqs:            opts.SQS,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	if err == nil {
		err = s.dispatch(recorder, r, metadata)
	}
	if err != nil {
		WriteError(recorder, r, err)
	}

	s.logger.Info("request complete",
		"request_id", metadata.RequestID,
		"service", metadata.Classification.Service,
		"operation", metadata.Classification.Operation,
		"method", r.Method,
		"path", r.URL.Path,
		"status", recorder.statusCode,
		"duration_ms", time.Since(start).Milliseconds(),
	)
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
	case "sqs":
		return s.handleSQS(w, r, metadata)
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

func (s *Server) handleSQS(w http.ResponseWriter, r *http.Request, metadata RequestMetadata) error {
	if s.sqs == nil {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "service \"sqs\" is not implemented"}
	}
	return s.sqs.Handle(w, r, metadata.Classification.Operation, metadata.RequestID)
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
