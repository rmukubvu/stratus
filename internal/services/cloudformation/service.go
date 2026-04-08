package cloudformation

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/services/apigateway"
	"github.com/stratus/internal/services/apigatewayv2"
	"github.com/stratus/internal/services/cognitoidp"
	eventssvc "github.com/stratus/internal/services/events"
	"github.com/stratus/internal/services/kinesis"
	"github.com/stratus/internal/services/kms"
	lambdasvc "github.com/stratus/internal/services/lambda"
	"github.com/stratus/internal/services/s3"
	"github.com/stratus/internal/services/secretsmanager"
	"github.com/stratus/internal/services/sns"
	"github.com/stratus/internal/services/ssm"
	"github.com/stratus/internal/services/stepfunctions"
	"github.com/stratus/internal/store"
)

const (
	namespace             = "http://cloudformation.amazonaws.com/doc/2010-05-15/"
	accountID             = "000000000000"
	stacksBucket          = "cloudformation-stacks"
	changeSetsBucket      = "cloudformation-change-sets"
	logGroupsBucket       = "logs-groups"
	logStreamsBucket      = "logs-streams"
	logEventsBucket       = "logs-events"
	sqsQueuesBucket       = "sqs-queues"
	sqsMessagesBucket     = "sqs-messages"
	iamRolesBucket        = "iam-roles"
	iamRolePoliciesBucket = "iam-role-policies"
	dynamoTablesBucket    = "dynamodb-tables"
	dynamoItemsBucket     = "dynamodb-items"
)

var stackNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,127}$`)

type Service struct {
	metadata       store.Store
	lambda         *lambdasvc.Service
	apiGateway     *apigateway.Service
	apiGatewayV2   *apigatewayv2.Service
	s3             *s3.Service
	sns            *sns.Service
	events         *eventssvc.Service
	secretsManager *secretsmanager.Service
	kinesis        *kinesis.Service
	cognitoIDP     *cognitoidp.Service
	stepFunctions  *stepfunctions.Service
	ssm            *ssm.Service
	kms            *kms.Service
	now            func() time.Time
	mu             sync.Mutex
}

type Options struct {
	Metadata       store.Store
	Lambda         *lambdasvc.Service
	APIGateway     *apigateway.Service
	APIGatewayV2   *apigatewayv2.Service
	S3             *s3.Service
	SNS            *sns.Service
	Events         *eventssvc.Service
	SecretsManager *secretsmanager.Service
	Kinesis        *kinesis.Service
	CognitoIDP     *cognitoidp.Service
	StepFunctions  *stepfunctions.Service
	SSM            *ssm.Service
	KMS            *kms.Service
}

type stackRecord struct {
	Capabilities        []string          `json:"capabilities,omitempty"`
	ClientRequestToken  string            `json:"client_request_token,omitempty"`
	CreationTime        time.Time         `json:"creation_time"`
	Description         string            `json:"description,omitempty"`
	DisableRollback     bool              `json:"disable_rollback"`
	ManagedResources    []managedResource `json:"managed_resources,omitempty"`
	Parameters          []stackParameter  `json:"parameters,omitempty"`
	StackID             string            `json:"stack_id"`
	StackName           string            `json:"stack_name"`
	StackStatus         string            `json:"stack_status"`
	TemplateBody        string            `json:"template_body"`
	TemplateDescription string            `json:"template_description,omitempty"`
	TimeoutInMinutes    int               `json:"timeout_in_minutes,omitempty"`
}

type stackParameter struct {
	DefaultValue   string `json:"default_value,omitempty"`
	Description    string `json:"description,omitempty"`
	NoEcho         bool   `json:"no_echo,omitempty"`
	ParameterKey   string `json:"parameter_key"`
	ParameterValue string `json:"parameter_value,omitempty"`
}

type changeSetRecord struct {
	Capabilities        []string         `json:"capabilities,omitempty"`
	ChangeSetID         string           `json:"change_set_id"`
	ChangeSetName       string           `json:"change_set_name"`
	ChangeSetType       string           `json:"change_set_type"`
	ClientToken         string           `json:"client_token,omitempty"`
	CreatedAt           time.Time        `json:"created_at"`
	Description         string           `json:"description,omitempty"`
	Executed            bool             `json:"executed"`
	Parameters          []stackParameter `json:"parameters,omitempty"`
	StackName           string           `json:"stack_name"`
	Status              string           `json:"status"`
	ExecutionStatus     string           `json:"execution_status"`
	TemplateBody        string           `json:"template_body"`
	TemplateDescription string           `json:"template_description,omitempty"`
}

type templateDocument struct {
	AWSTemplateFormatVersion string                           `json:"AWSTemplateFormatVersion"`
	Description              string                           `json:"Description"`
	Parameters               map[string]templateParameterSpec `json:"Parameters"`
	Resources                map[string]templateResource      `json:"Resources"`
}

type templateParameterSpec struct {
	Type        string `json:"Type"`
	Default     string `json:"Default"`
	Description string `json:"Description"`
	NoEcho      bool   `json:"NoEcho"`
}

type templateResource struct {
	Type       string         `json:"Type"`
	Properties map[string]any `json:"Properties"`
}

type managedResource struct {
	LogicalID string `json:"logical_id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
}

type resourceState struct {
	RefValue string
	Attrs    map[string]string
}

type templateContext struct {
	Parameters map[string]string
	Resources  map[string]resourceState
}

type dependencyError struct {
	Resource string
}

func (e *dependencyError) Error() string {
	return "resource dependency not ready: " + e.Resource
}

type logGroupRecord struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type sqsQueueRecord struct {
	Name       string            `json:"name"`
	CreatedAt  time.Time         `json:"created_at"`
	Attributes map[string]string `json:"attributes"`
}

type iamRoleRecord struct {
	AssumeRolePolicyDocument string    `json:"assume_role_policy_document"`
	CreateDate               time.Time `json:"create_date"`
	Description              string    `json:"description,omitempty"`
	MaxSessionDuration       int       `json:"max_session_duration"`
	Path                     string    `json:"path"`
	RoleID                   string    `json:"role_id"`
	RoleName                 string    `json:"role_name"`
}

type iamRolePolicyRecord struct {
	PolicyDocument string `json:"policy_document"`
	PolicyName     string `json:"policy_name"`
	RoleName       string `json:"role_name"`
}

type dynamoAttributeDefinition struct {
	AttributeName string `json:"AttributeName"`
	AttributeType string `json:"AttributeType"`
}

type dynamoKeySchemaElement struct {
	AttributeName string `json:"AttributeName"`
	KeyType       string `json:"KeyType"`
}

type dynamoTableRecord struct {
	AttributeDefinitions []dynamoAttributeDefinition `json:"attribute_definitions"`
	BillingMode          string                      `json:"billing_mode"`
	CreatedAt            time.Time                   `json:"created_at"`
	HashKey              string                      `json:"hash_key"`
	HashKeyType          string                      `json:"hash_key_type"`
	TableName            string                      `json:"table_name"`
	TableStatus          string                      `json:"table_status"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

type validateTemplateResponse struct {
	XMLName          xml.Name               `xml:"ValidateTemplateResponse"`
	XMLNS            string                 `xml:"xmlns,attr"`
	Result           validateTemplateResult `xml:"ValidateTemplateResult"`
	ResponseMetadata responseMetadata       `xml:"ResponseMetadata"`
}

type validateTemplateResult struct {
	Capabilities       []string               `xml:"Capabilities>member,omitempty"`
	CapabilitiesReason string                 `xml:"CapabilitiesReason,omitempty"`
	DeclaredTransforms []string               `xml:"DeclaredTransforms>member,omitempty"`
	Description        string                 `xml:"Description,omitempty"`
	Parameters         []templateParameterXML `xml:"Parameters>member,omitempty"`
}

type createStackResponse struct {
	XMLName          xml.Name          `xml:"CreateStackResponse"`
	XMLNS            string            `xml:"xmlns,attr"`
	Result           createStackResult `xml:"CreateStackResult"`
	ResponseMetadata responseMetadata  `xml:"ResponseMetadata"`
}

type createStackResult struct {
	StackID string `xml:"StackId"`
}

type createChangeSetResponse struct {
	XMLName          xml.Name              `xml:"CreateChangeSetResponse"`
	XMLNS            string                `xml:"xmlns,attr"`
	Result           createChangeSetResult `xml:"CreateChangeSetResult"`
	ResponseMetadata responseMetadata      `xml:"ResponseMetadata"`
}

type createChangeSetResult struct {
	ID      string `xml:"Id"`
	StackID string `xml:"StackId"`
}

type describeStacksResponse struct {
	XMLName          xml.Name             `xml:"DescribeStacksResponse"`
	XMLNS            string               `xml:"xmlns,attr"`
	Result           describeStacksResult `xml:"DescribeStacksResult"`
	ResponseMetadata responseMetadata     `xml:"ResponseMetadata"`
}

type describeStacksResult struct {
	Stacks []stackXML `xml:"Stacks>member"`
}

type describeStackEventsResponse struct {
	XMLName          xml.Name                  `xml:"DescribeStackEventsResponse"`
	XMLNS            string                    `xml:"xmlns,attr"`
	Result           describeStackEventsResult `xml:"DescribeStackEventsResult"`
	ResponseMetadata responseMetadata          `xml:"ResponseMetadata"`
}

type describeStackEventsResult struct {
	StackEvents []stackEventXML `xml:"StackEvents>member"`
}

type getTemplateResponse struct {
	XMLName          xml.Name          `xml:"GetTemplateResponse"`
	XMLNS            string            `xml:"xmlns,attr"`
	Result           getTemplateResult `xml:"GetTemplateResult"`
	ResponseMetadata responseMetadata  `xml:"ResponseMetadata"`
}

type getTemplateResult struct {
	StagesAvailable []string `xml:"StagesAvailable>member"`
	TemplateBody    string   `xml:"TemplateBody"`
}

type describeChangeSetResponse struct {
	XMLName          xml.Name                `xml:"DescribeChangeSetResponse"`
	XMLNS            string                  `xml:"xmlns,attr"`
	Result           describeChangeSetResult `xml:"DescribeChangeSetResult"`
	ResponseMetadata responseMetadata        `xml:"ResponseMetadata"`
}

type describeChangeSetResult struct {
	Capabilities    []string               `xml:"Capabilities>member,omitempty"`
	ChangeSetID     string                 `xml:"ChangeSetId"`
	ChangeSetName   string                 `xml:"ChangeSetName"`
	Changes         []describeChangeXML    `xml:"Changes>member,omitempty"`
	CreationTime    string                 `xml:"CreationTime"`
	Description     string                 `xml:"Description,omitempty"`
	ExecutionStatus string                 `xml:"ExecutionStatus"`
	Parameters      []templateParameterXML `xml:"Parameters>member,omitempty"`
	StackID         string                 `xml:"StackId"`
	StackName       string                 `xml:"StackName"`
	Status          string                 `xml:"Status"`
	TemplateBody    string                 `xml:"TemplateBody,omitempty"`
}

type describeChangeXML struct {
	ResourceChange describeResourceChangeXML `xml:"ResourceChange"`
}

type describeResourceChangeXML struct {
	Action             string `xml:"Action"`
	LogicalResourceID  string `xml:"LogicalResourceId"`
	PhysicalResourceID string `xml:"PhysicalResourceId,omitempty"`
	ResourceType       string `xml:"ResourceType"`
	Replacement        string `xml:"Replacement,omitempty"`
}

type listStackResourcesResponse struct {
	XMLName          xml.Name                 `xml:"ListStackResourcesResponse"`
	XMLNS            string                   `xml:"xmlns,attr"`
	Result           listStackResourcesResult `xml:"ListStackResourcesResult"`
	ResponseMetadata responseMetadata         `xml:"ResponseMetadata"`
}

type listStackResourcesResult struct {
	StackResourceSummaries []stackResourceSummaryXML `xml:"StackResourceSummaries>member"`
}

type listStacksResponse struct {
	XMLName          xml.Name         `xml:"ListStacksResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           listStacksResult `xml:"ListStacksResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type listStacksResult struct {
	StackSummaries []stackSummaryXML `xml:"StackSummaries>member"`
}

type deleteStackResponse struct {
	XMLName          xml.Name         `xml:"DeleteStackResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type executeChangeSetResponse struct {
	XMLName          xml.Name         `xml:"ExecuteChangeSetResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type deleteChangeSetResponse struct {
	XMLName          xml.Name         `xml:"DeleteChangeSetResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type stackXML struct {
	Capabilities        []string               `xml:"Capabilities>member,omitempty"`
	ClientRequestToken  string                 `xml:"ClientRequestToken,omitempty"`
	CreationTime        string                 `xml:"CreationTime"`
	Description         string                 `xml:"Description,omitempty"`
	DisableRollback     bool                   `xml:"DisableRollback"`
	Outputs             []outputXML            `xml:"Outputs>member,omitempty"`
	Parameters          []templateParameterXML `xml:"Parameters>member,omitempty"`
	StackID             string                 `xml:"StackId"`
	StackName           string                 `xml:"StackName"`
	StackStatus         string                 `xml:"StackStatus"`
	TemplateDescription string                 `xml:"TemplateDescription,omitempty"`
	TimeoutInMinutes    int                    `xml:"TimeoutInMinutes,omitempty"`
}

type stackSummaryXML struct {
	CreationTime        string `xml:"CreationTime"`
	LastUpdatedTime     string `xml:"LastUpdatedTime,omitempty"`
	StackID             string `xml:"StackId"`
	StackName           string `xml:"StackName"`
	StackStatus         string `xml:"StackStatus"`
	TemplateDescription string `xml:"TemplateDescription,omitempty"`
}

type stackResourceSummaryXML struct {
	LastUpdatedTimestamp string `xml:"LastUpdatedTimestamp,omitempty"`
	LogicalResourceID    string `xml:"LogicalResourceId"`
	PhysicalResourceID   string `xml:"PhysicalResourceId"`
	ResourceStatus       string `xml:"ResourceStatus"`
	ResourceType         string `xml:"ResourceType"`
}

type stackEventXML struct {
	EventID            string `xml:"EventId"`
	LogicalResourceID  string `xml:"LogicalResourceId"`
	PhysicalResourceID string `xml:"PhysicalResourceId"`
	ResourceStatus     string `xml:"ResourceStatus"`
	ResourceType       string `xml:"ResourceType"`
	StackID            string `xml:"StackId"`
	StackName          string `xml:"StackName"`
	Timestamp          string `xml:"Timestamp"`
}

type templateParameterXML struct {
	DefaultValue   string `xml:"DefaultValue,omitempty"`
	Description    string `xml:"Description,omitempty"`
	NoEcho         bool   `xml:"NoEcho,omitempty"`
	ParameterKey   string `xml:"ParameterKey"`
	ParameterValue string `xml:"ParameterValue,omitempty"`
}

type outputXML struct {
	Description string `xml:"Description,omitempty"`
	OutputKey   string `xml:"OutputKey"`
	OutputValue string `xml:"OutputValue"`
}

func NewService(opts Options) *Service {
	return &Service{
		metadata:       opts.Metadata,
		lambda:         opts.Lambda,
		apiGateway:     opts.APIGateway,
		apiGatewayV2:   opts.APIGatewayV2,
		s3:             opts.S3,
		sns:            opts.SNS,
		events:         opts.Events,
		secretsManager: opts.SecretsManager,
		kinesis:        opts.Kinesis,
		cognitoIDP:     opts.CognitoIDP,
		stepFunctions:  opts.StepFunctions,
		ssm:            opts.SSM,
		kms:            opts.KMS,
		now:            time.Now,
	}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "ValidateTemplate":
		return s.validateTemplate(w, r, requestID)
	case "CreateStack":
		return s.createStack(w, r, requestID)
	case "CreateChangeSet":
		return s.createChangeSet(w, r, requestID)
	case "DescribeStacks":
		return s.describeStacks(w, r, requestID)
	case "DescribeStackEvents":
		return s.describeStackEvents(w, r, requestID)
	case "DescribeChangeSet":
		return s.describeChangeSet(w, r, requestID)
	case "ListStackResources":
		return s.listStackResources(w, r, requestID)
	case "GetTemplate":
		return s.getTemplate(w, r, requestID)
	case "ListStacks":
		return s.listStacks(w, r, requestID)
	case "ExecuteChangeSet":
		return s.executeChangeSet(w, r, requestID)
	case "DeleteChangeSet":
		return s.deleteChangeSet(w, r, requestID)
	case "DeleteStack":
		return s.deleteStack(w, r, requestID)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "cloudformation operation is not implemented",
		}
	}
}

func (s *Service) validateTemplate(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	doc, normalized, err := parseTemplate(form)
	if err != nil {
		return err
	}
	_ = normalized

	writeXML(w, http.StatusOK, validateTemplateResponse{
		XMLNS: namespace,
		Result: validateTemplateResult{
			Capabilities:       []string{},
			DeclaredTransforms: []string{},
			Description:        doc.Description,
			Parameters:         parametersFromTemplate(doc, nil),
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) createStack(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	stackName := form.Get("StackName")
	if err := validateStackName(stackName); err != nil {
		return err
	}
	if raw := form.Get("TemplateURL"); raw != "" {
		return notImplemented("TemplateURL is not implemented")
	}
	if form.Get("RoleARN") != "" {
		return notImplemented("RoleARN is not implemented")
	}
	if form.Get("StackPolicyBody") != "" || form.Get("StackPolicyURL") != "" {
		return notImplemented("stack policies are not implemented")
	}
	if form.Get("ResourceTypes.member.1") != "" || hasIndexedPrefix(form, "ResourceTypes.member.") {
		return notImplemented("resource type filters are not implemented")
	}
	if form.Get("NotificationARNs.member.1") != "" || hasIndexedPrefix(form, "NotificationARNs.member.") {
		return notImplemented("notification arns are not implemented")
	}
	if form.Get("RollbackConfiguration.MonitoringTimeInMinutes") != "" || hasIndexedPrefix(form, "RollbackConfiguration.") {
		return notImplemented("rollback configuration is not implemented")
	}
	if form.Get("EnableTerminationProtection") != "" || form.Get("RetainExceptOnCreate") != "" {
		return notImplemented("termination protection controls are not implemented")
	}
	if hasIndexedPrefix(form, "Tags.member.") {
		return notImplemented("stack tags are not implemented")
	}
	doc, normalized, err := parseTemplate(form)
	if err != nil {
		return err
	}

	if _, err := s.loadStack(stackName); err == nil {
		return &apierror.Error{
			StatusCode: http.StatusBadRequest,
			Code:       "AlreadyExistsException",
			Message:    "Stack " + stackName + " already exists",
		}
	}

	disableRollback := false
	if raw := form.Get("DisableRollback"); raw != "" {
		disableRollback, err = strconv.ParseBool(raw)
		if err != nil {
			return validationError("DisableRollback must be true or false")
		}
	}

	timeoutInMinutes := 0
	if raw := form.Get("TimeoutInMinutes"); raw != "" {
		timeoutInMinutes, err = strconv.Atoi(raw)
		if err != nil || timeoutInMinutes < 1 {
			return validationError("TimeoutInMinutes must be at least 1")
		}
	}

	parameters, err := parametersFromForm(doc, form)
	if err != nil {
		return err
	}
	capabilities, err := capabilitiesFromForm(form)
	if err != nil {
		return err
	}
	ctx := &templateContext{
		Parameters: parameterValueMap(parameters),
		Resources:  map[string]resourceState{},
	}
	managedResources, err := s.applyResources(stackName, doc.Resources, ctx)
	if err != nil {
		return err
	}

	record := stackRecord{
		Capabilities:        capabilities,
		ClientRequestToken:  form.Get("ClientRequestToken"),
		CreationTime:        s.now().UTC(),
		Description:         doc.Description,
		DisableRollback:     disableRollback,
		ManagedResources:    managedResources,
		Parameters:          parameters,
		StackID:             stackID(stackName),
		StackName:           stackName,
		StackStatus:         "CREATE_COMPLETE",
		TemplateBody:        normalized,
		TemplateDescription: doc.Description,
		TimeoutInMinutes:    timeoutInMinutes,
	}
	if err := s.putStack(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, createStackResponse{
		XMLNS: namespace,
		Result: createStackResult{
			StackID: record.StackID,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) createChangeSet(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	stackName := form.Get("StackName")
	if err := validateStackName(stackName); err != nil {
		return err
	}

	doc, normalized, err := parseTemplate(form)
	if err != nil {
		return err
	}
	parameters, err := parametersFromForm(doc, form)
	if err != nil {
		return err
	}
	capabilities, err := capabilitiesFromForm(form)
	if err != nil {
		return err
	}

	changeSetName := form.Get("ChangeSetName")
	if changeSetName == "" {
		return validationError("ChangeSetName is required")
	}
	changeSetType := strings.ToUpper(strings.TrimSpace(form.Get("ChangeSetType")))
	existingStack, loadErr := s.loadStack(stackName)
	stackExists := loadErr == nil
	switch changeSetType {
	case "":
		if stackExists {
			changeSetType = "UPDATE"
		} else {
			changeSetType = "CREATE"
		}
	case "CREATE":
		if stackExists {
			return validationError("Stack " + stackName + " already exists")
		}
	case "UPDATE":
		if !stackExists {
			return validationError("Stack " + stackName + " does not exist")
		}
	default:
		return validationError("ChangeSetType must be CREATE or UPDATE")
	}

	record := changeSetRecord{
		Capabilities:        capabilities,
		ChangeSetID:         changeSetID(stackName, changeSetName),
		ChangeSetName:       changeSetName,
		ChangeSetType:       changeSetType,
		ClientToken:         form.Get("ClientToken"),
		CreatedAt:           s.now().UTC(),
		Description:         form.Get("Description"),
		Executed:            false,
		Parameters:          parameters,
		StackName:           stackName,
		Status:              "CREATE_COMPLETE",
		ExecutionStatus:     "AVAILABLE",
		TemplateBody:        normalized,
		TemplateDescription: doc.Description,
	}
	if err := s.putChangeSet(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, createChangeSetResponse{
		XMLNS: namespace,
		Result: createChangeSetResult{
			ID:      record.ChangeSetID,
			StackID: firstNonEmpty(existingStack.StackID, stackID(stackName)),
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) describeChangeSet(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadChangeSetByForm(form)
	if err != nil {
		return err
	}
	stackIDValue := stackID(record.StackName)
	if stack, err := s.loadStack(record.StackName); err == nil {
		stackIDValue = stack.StackID
	}

	var changes []describeChangeXML
	var doc templateDocument
	if err := json.Unmarshal([]byte(record.TemplateBody), &doc); err == nil {
		keys := make([]string, 0, len(doc.Resources))
		for key := range doc.Resources {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		action := "Add"
		if record.ChangeSetType == "UPDATE" {
			action = "Modify"
		}
		for _, logicalID := range keys {
			resource := doc.Resources[logicalID]
			changes = append(changes, describeChangeXML{
				ResourceChange: describeResourceChangeXML{
					Action:            action,
					LogicalResourceID: logicalID,
					ResourceType:      resource.Type,
					Replacement:       "False",
				},
			})
		}
	}

	writeXML(w, http.StatusOK, describeChangeSetResponse{
		XMLNS: namespace,
		Result: describeChangeSetResult{
			Capabilities:    record.Capabilities,
			ChangeSetID:     record.ChangeSetID,
			ChangeSetName:   record.ChangeSetName,
			Changes:         changes,
			CreationTime:    record.CreatedAt.UTC().Format(time.RFC3339),
			Description:     record.Description,
			ExecutionStatus: record.ExecutionStatus,
			Parameters:      stackParametersToXML(record.Parameters),
			StackID:         stackIDValue,
			StackName:       record.StackName,
			Status:          record.Status,
			TemplateBody:    record.TemplateBody,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) executeChangeSet(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadChangeSetByForm(form)
	if err != nil {
		return err
	}
	if record.Executed {
		writeXML(w, http.StatusOK, executeChangeSetResponse{
			XMLNS:            namespace,
			ResponseMetadata: responseMetadata{RequestID: requestID},
		})
		return nil
	}

	var doc templateDocument
	if err := json.Unmarshal([]byte(record.TemplateBody), &doc); err != nil {
		return validationError("template body is not a valid CloudFormation object")
	}
	ctx := &templateContext{
		Parameters: parameterValueMap(record.Parameters),
		Resources:  map[string]resourceState{},
	}

	stackIDValue := stackID(record.StackName)
	creationTime := s.now().UTC()
	disableRollback := false
	timeoutInMinutes := 0
	if existing, err := s.loadStack(record.StackName); err == nil {
		if record.ChangeSetType == "UPDATE" {
			if err := s.deleteManagedResources(existing.ManagedResources); err != nil {
				return err
			}
		}
		stackIDValue = existing.StackID
		creationTime = existing.CreationTime
		disableRollback = existing.DisableRollback
		timeoutInMinutes = existing.TimeoutInMinutes
	}

	managedResources, err := s.applyResources(record.StackName, doc.Resources, ctx)
	if err != nil {
		return err
	}

	stack := stackRecord{
		Capabilities:        record.Capabilities,
		ClientRequestToken:  record.ClientToken,
		CreationTime:        creationTime,
		Description:         doc.Description,
		DisableRollback:     disableRollback,
		ManagedResources:    managedResources,
		Parameters:          record.Parameters,
		StackID:             stackIDValue,
		StackName:           record.StackName,
		StackStatus:         "CREATE_COMPLETE",
		TemplateBody:        record.TemplateBody,
		TemplateDescription: record.TemplateDescription,
		TimeoutInMinutes:    timeoutInMinutes,
	}
	if err := s.putStack(stack); err != nil {
		_ = s.deleteManagedResources(managedResources)
		return internal(err)
	}

	record.Executed = true
	record.ExecutionStatus = "EXECUTE_COMPLETE"
	if err := s.putChangeSet(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, executeChangeSetResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) deleteChangeSet(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadChangeSetByForm(form)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(changeSetsBucket, record.ChangeSetID); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, deleteChangeSetResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) describeStacks(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	if stackName := form.Get("StackName"); stackName != "" {
		stack, err := s.loadStack(stackName)
		if err != nil {
			return err
		}
		writeXML(w, http.StatusOK, describeStacksResponse{
			XMLNS: namespace,
			Result: describeStacksResult{
				Stacks: []stackXML{stackToXML(stack)},
			},
			ResponseMetadata: responseMetadata{RequestID: requestID},
		})
		return nil
	}

	var stacks []stackXML
	if err := s.metadata.Scan(stacksBucket, "", func(_, v []byte) error {
		var record stackRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		stacks = append(stacks, stackToXML(record))
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(stacks, func(i, j int) bool {
		return stacks[i].StackName < stacks[j].StackName
	})
	writeXML(w, http.StatusOK, describeStacksResponse{
		XMLNS: namespace,
		Result: describeStacksResult{
			Stacks: stacks,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) describeStackEvents(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	stack, err := s.loadStackByForm(form)
	if err != nil {
		return err
	}

	events := make([]stackEventXML, 0, len(stack.ManagedResources)+1)
	events = append(events, stackEventXML{
		EventID:            stack.StackName + "-stack",
		LogicalResourceID:  stack.StackName,
		PhysicalResourceID: stack.StackID,
		ResourceStatus:     stack.StackStatus,
		ResourceType:       "AWS::CloudFormation::Stack",
		StackID:            stack.StackID,
		StackName:          stack.StackName,
		Timestamp:          stack.CreationTime.UTC().Format(time.RFC3339),
	})
	for idx := len(stack.ManagedResources) - 1; idx >= 0; idx-- {
		resource := stack.ManagedResources[idx]
		events = append(events, stackEventXML{
			EventID:            fmt.Sprintf("%s-%d", resource.LogicalID, idx),
			LogicalResourceID:  resource.LogicalID,
			PhysicalResourceID: resource.Name,
			ResourceStatus:     "CREATE_COMPLETE",
			ResourceType:       resource.Type,
			StackID:            stack.StackID,
			StackName:          stack.StackName,
			Timestamp:          stack.CreationTime.UTC().Format(time.RFC3339),
		})
	}

	writeXML(w, http.StatusOK, describeStackEventsResponse{
		XMLNS: namespace,
		Result: describeStackEventsResult{
			StackEvents: events,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) getTemplate(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	stack, err := s.loadStackByForm(form)
	if err != nil {
		return err
	}
	writeXML(w, http.StatusOK, getTemplateResponse{
		XMLNS: namespace,
		Result: getTemplateResult{
			StagesAvailable: []string{"Original"},
			TemplateBody:    stack.TemplateBody,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) listStackResources(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	stack, err := s.loadStackByForm(form)
	if err != nil {
		return err
	}

	summaries := make([]stackResourceSummaryXML, 0, len(stack.ManagedResources))
	for _, resource := range stack.ManagedResources {
		summaries = append(summaries, stackResourceSummaryXML{
			LastUpdatedTimestamp: stack.CreationTime.UTC().Format(time.RFC3339),
			LogicalResourceID:    resource.LogicalID,
			PhysicalResourceID:   resource.Name,
			ResourceStatus:       "CREATE_COMPLETE",
			ResourceType:         resource.Type,
		})
	}

	writeXML(w, http.StatusOK, listStackResourcesResponse{
		XMLNS: namespace,
		Result: listStackResourcesResult{
			StackResourceSummaries: summaries,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) listStacks(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	var filters map[string]struct{}
	if hasIndexedPrefix(form, "StackStatusFilter.member.") {
		filters = map[string]struct{}{}
		for idx := 1; ; idx++ {
			value := form.Get(fmt.Sprintf("StackStatusFilter.member.%d", idx))
			if value == "" {
				break
			}
			filters[value] = struct{}{}
		}
	}

	var summaries []stackSummaryXML
	if err := s.metadata.Scan(stacksBucket, "", func(_, v []byte) error {
		var record stackRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if len(filters) > 0 {
			if _, ok := filters[record.StackStatus]; !ok {
				return nil
			}
		}
		summaries = append(summaries, stackSummaryXML{
			CreationTime:        record.CreationTime.UTC().Format(time.RFC3339),
			LastUpdatedTime:     record.CreationTime.UTC().Format(time.RFC3339),
			StackID:             record.StackID,
			StackName:           record.StackName,
			StackStatus:         record.StackStatus,
			TemplateDescription: record.TemplateDescription,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].StackName < summaries[j].StackName
	})
	writeXML(w, http.StatusOK, listStacksResponse{
		XMLNS: namespace,
		Result: listStacksResult{
			StackSummaries: summaries,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) deleteStack(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	stack, err := s.loadStackByForm(form)
	if err != nil {
		return err
	}
	if err := s.deleteManagedResources(stack.ManagedResources); err != nil {
		return err
	}
	if err := s.metadata.Delete(stacksBucket, stack.StackName); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, deleteStackResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func parseForm(r *http.Request) (url.Values, error) {
	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return nil, validationError("request body is not valid form data")
	}
	return form, nil
}

func parseTemplate(form url.Values) (templateDocument, string, error) {
	if raw := form.Get("TemplateURL"); raw != "" {
		return templateDocument{}, "", notImplemented("TemplateURL is not implemented")
	}
	body := form.Get("TemplateBody")
	if body == "" {
		return templateDocument{}, "", validationError("TemplateBody is required")
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(body), &generic); err != nil {
		if yamlErr := yaml.Unmarshal([]byte(body), &generic); yamlErr != nil {
			return templateDocument{}, "", validationError("template body must be valid JSON or YAML")
		}
	}
	normalizedBytes, err := json.Marshal(generic)
	if err != nil {
		return templateDocument{}, "", internal(err)
	}
	var doc templateDocument
	if err := json.Unmarshal(normalizedBytes, &doc); err != nil {
		return templateDocument{}, "", validationError("template body is not a valid CloudFormation object")
	}
	if doc.Parameters == nil {
		doc.Parameters = map[string]templateParameterSpec{}
	}
	if doc.Resources == nil {
		doc.Resources = map[string]templateResource{}
	}
	return doc, string(normalizedBytes), nil
}

func parametersFromTemplate(doc templateDocument, values map[string]string) []templateParameterXML {
	keys := make([]string, 0, len(doc.Parameters))
	for key := range doc.Parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]templateParameterXML, 0, len(keys))
	for _, key := range keys {
		spec := doc.Parameters[key]
		item := templateParameterXML{
			DefaultValue: spec.Default,
			Description:  spec.Description,
			NoEcho:       spec.NoEcho,
			ParameterKey: key,
		}
		if values != nil {
			item.ParameterValue = values[key]
		}
		out = append(out, item)
	}
	return out
}

func parametersFromForm(doc templateDocument, form url.Values) ([]stackParameter, error) {
	values := map[string]string{}
	for idx := 1; ; idx++ {
		key := form.Get(fmt.Sprintf("Parameters.member.%d.ParameterKey", idx))
		if key == "" {
			break
		}
		if _, ok := doc.Parameters[key]; !ok {
			return nil, validationError("parameter " + key + " is not defined in the template")
		}
		values[key] = form.Get(fmt.Sprintf("Parameters.member.%d.ParameterValue", idx))
	}

	keys := make([]string, 0, len(doc.Parameters))
	for key := range doc.Parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var out []stackParameter
	for _, key := range keys {
		spec := doc.Parameters[key]
		if spec.Type != "" && spec.Type != "String" {
			return nil, notImplemented("parameter type " + spec.Type + " is not implemented")
		}
		value, ok := values[key]
		if !ok {
			value = spec.Default
		}
		out = append(out, stackParameter{
			DefaultValue:   spec.Default,
			Description:    spec.Description,
			NoEcho:         spec.NoEcho,
			ParameterKey:   key,
			ParameterValue: value,
		})
	}
	return out, nil
}

func stackID(name string) string {
	return fmt.Sprintf("arn:aws:cloudformation:us-east-1:%s:stack/%s/%s", accountID, name, uuid.NewString())
}

func parameterValueMap(parameters []stackParameter) map[string]string {
	values := make(map[string]string, len(parameters))
	for _, parameter := range parameters {
		values[parameter.ParameterKey] = parameter.ParameterValue
	}
	return values
}

func capabilitiesFromForm(form url.Values) ([]string, error) {
	var capabilities []string
	for idx := 1; ; idx++ {
		value := form.Get(fmt.Sprintf("Capabilities.member.%d", idx))
		if value == "" {
			break
		}
		switch value {
		case "CAPABILITY_IAM", "CAPABILITY_NAMED_IAM", "CAPABILITY_AUTO_EXPAND":
			capabilities = append(capabilities, value)
		default:
			return nil, validationError("unsupported capability " + value)
		}
	}
	return capabilities, nil
}

func (s *Service) applyResources(stackName string, resources map[string]templateResource, ctx *templateContext) ([]managedResource, error) {
	pending := make([]string, 0, len(resources))
	for logicalID := range resources {
		pending = append(pending, logicalID)
	}
	sort.Strings(pending)

	var created []managedResource
	for len(pending) > 0 {
		nextPending := make([]string, 0, len(pending))
		progressed := false
		for _, logicalID := range pending {
			resource, state, err := s.applyResource(stackName, logicalID, resources[logicalID], ctx)
			if err != nil {
				var depErr *dependencyError
				if errors.As(err, &depErr) {
					nextPending = append(nextPending, logicalID)
					continue
				}
				_ = s.deleteManagedResources(created)
				return nil, err
			}
			created = append(created, resource...)
			ctx.Resources[logicalID] = state
			progressed = true
		}
		if !progressed {
			_ = s.deleteManagedResources(created)
			return nil, validationError("resource dependency graph could not be resolved")
		}
		pending = nextPending
	}
	return created, nil
}

func (s *Service) applyResource(stackName, logicalID string, resource templateResource, ctx *templateContext) ([]managedResource, resourceState, error) {
	switch resource.Type {
	case "AWS::Logs::LogGroup":
		name, err := resolveOptionalString(resource.Properties["LogGroupName"], ctx, defaultPhysicalName(stackName, logicalID))
		if err != nil {
			return nil, resourceState{}, err
		}
		if _, ok := resource.Properties["RetentionInDays"]; ok {
			return nil, resourceState{}, notImplemented("AWS::Logs::LogGroup RetentionInDays is not implemented")
		}
		if err := s.createLogGroup(name); err != nil {
			return nil, resourceState{}, err
		}
		return []managedResource{{LogicalID: logicalID, Type: resource.Type, Name: name}}, resourceState{RefValue: name}, nil
	case "AWS::SQS::Queue":
		name, err := resolveOptionalString(resource.Properties["QueueName"], ctx, defaultPhysicalName(stackName, logicalID))
		if err != nil {
			return nil, resourceState{}, err
		}
		attrs, err := s.sqsAttributes(resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		if err := s.createQueue(name, attrs); err != nil {
			return nil, resourceState{}, err
		}
		return []managedResource{{LogicalID: logicalID, Type: resource.Type, Name: name}}, resourceState{
			RefValue: name,
			Attrs: map[string]string{
				"Arn":       fmt.Sprintf("arn:aws:sqs:us-east-1:%s:%s", accountID, name),
				"QueueName": name,
			},
		}, nil
	case "AWS::IAM::Role":
		managed, state, err := s.createIAMRole(stackName, logicalID, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		return managed, state, nil
	case "AWS::IAM::Policy":
		return s.createIAMPolicy(logicalID, resource.Properties, ctx)
	case "AWS::DynamoDB::Table":
		name, err := resolveOptionalString(resource.Properties["TableName"], ctx, defaultPhysicalName(stackName, logicalID))
		if err != nil {
			return nil, resourceState{}, err
		}
		record, err := s.dynamoTableRecord(name, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		if err := s.createDynamoTable(record); err != nil {
			return nil, resourceState{}, err
		}
		return []managedResource{{LogicalID: logicalID, Type: resource.Type, Name: name}}, resourceState{
			RefValue: name,
			Attrs: map[string]string{
				"Arn": fmt.Sprintf("arn:aws:dynamodb:us-east-1:%s:table/%s", accountID, name),
			},
		}, nil
	case "AWS::Lambda::Function":
		managed, state, err := s.createLambdaFunction(stackName, logicalID, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		return managed, state, nil
	case "AWS::Lambda::Permission":
		managed, state, err := s.createLambdaPermission(logicalID, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		return managed, state, nil
	case "AWS::Lambda::EventSourceMapping":
		return s.createLambdaEventSourceMapping(logicalID, resource.Properties, ctx)
	case "AWS::ApiGatewayV2::Api":
		managed, state, err := s.createAPIGatewayV2API(logicalID, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		return managed, state, nil
	case "AWS::ApiGatewayV2::Integration":
		managed, state, err := s.createAPIGatewayV2Integration(logicalID, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		return managed, state, nil
	case "AWS::ApiGatewayV2::Route":
		managed, state, err := s.createAPIGatewayV2Route(logicalID, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		return managed, state, nil
	case "AWS::ApiGatewayV2::Stage":
		managed, state, err := s.createAPIGatewayV2Stage(logicalID, resource.Properties, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		return managed, state, nil
	case "AWS::SNS::Topic":
		return s.createSNSTopic(stackName, logicalID, resource.Properties, ctx)
	case "AWS::Events::EventBus":
		return s.createEventBus(stackName, logicalID, resource.Properties, ctx)
	case "AWS::Events::Rule":
		return s.createEventRule(stackName, logicalID, resource.Properties, ctx)
	case "AWS::SecretsManager::Secret":
		return s.createSecret(stackName, logicalID, resource.Properties, ctx)
	case "AWS::Kinesis::Stream":
		return s.createKinesisStream(stackName, logicalID, resource.Properties, ctx)
	case "AWS::Cognito::UserPool":
		return s.createCognitoUserPool(stackName, logicalID, resource.Properties, ctx)
	case "AWS::Cognito::UserPoolClient":
		return s.createCognitoUserPoolClient(stackName, logicalID, resource.Properties, ctx)
	case "AWS::StepFunctions::StateMachine":
		return s.createStateMachine(stackName, logicalID, resource.Properties, ctx)
	case "AWS::ApiGateway::RestApi":
		return s.createAPIGatewayRestAPI(logicalID, resource.Properties, ctx)
	case "AWS::ApiGateway::Resource":
		return s.createAPIGatewayResource(logicalID, resource.Properties, ctx)
	case "AWS::ApiGateway::Method":
		return s.createAPIGatewayMethod(logicalID, resource.Properties, ctx)
	case "AWS::ApiGateway::Deployment":
		return s.createAPIGatewayDeployment(logicalID, resource.Properties, ctx)
	case "AWS::ApiGateway::Stage":
		return s.createAPIGatewayStage(logicalID, resource.Properties, ctx)
	case "AWS::SSM::Parameter":
		return s.createSSMParameter(stackName, logicalID, resource.Properties, ctx)
	case "AWS::KMS::Key":
		return s.createKMSKey(logicalID, resource.Properties, ctx)
	case "AWS::KMS::Alias":
		return s.createKMSAlias(logicalID, resource.Properties, ctx)
	case "AWS::CDK::Metadata":
		return nil, resourceState{RefValue: logicalID}, nil
	default:
		return nil, resourceState{}, notImplemented("resource type " + resource.Type + " is not implemented")
	}
}

func (s *Service) deleteManagedResources(resources []managedResource) error {
	for idx := len(resources) - 1; idx >= 0; idx-- {
		resource := resources[idx]
		switch resource.Type {
		case "AWS::Logs::LogGroup":
			if err := s.metadata.Delete(logGroupsBucket, resource.Name); err != nil {
				return internal(err)
			}
			if err := s.metadata.DeletePrefix(logStreamsBucket, resource.Name+"|"); err != nil {
				return internal(err)
			}
			if err := s.metadata.DeletePrefix(logEventsBucket, resource.Name+"|"); err != nil {
				return internal(err)
			}
		case "AWS::SQS::Queue":
			if err := s.metadata.Delete(sqsQueuesBucket, resource.Name); err != nil {
				return internal(err)
			}
			if err := s.metadata.DeletePrefix(sqsMessagesBucket, resource.Name+"|"); err != nil {
				return internal(err)
			}
		case "AWS::IAM::Role":
			if err := s.metadata.DeletePrefix(iamRolePoliciesBucket, resource.Name+"|"); err != nil {
				return internal(err)
			}
			if err := s.metadata.Delete(iamRolesBucket, resource.Name); err != nil {
				return internal(err)
			}
		case "AWS::IAM::Policy":
			if err := s.metadata.Delete(iamRolePoliciesBucket, resource.Name); err != nil {
				return internal(err)
			}
		case "AWS::DynamoDB::Table":
			if err := s.metadata.Delete(dynamoTablesBucket, resource.Name); err != nil {
				return internal(err)
			}
			if err := s.metadata.DeletePrefix(dynamoItemsBucket, resource.Name+"|"); err != nil {
				return internal(err)
			}
		case "AWS::Lambda::Function":
			if s.lambda == nil {
				return notImplemented("lambda service is not configured")
			}
			if err := s.lambda.Delete(context.Background(), resource.Name); err != nil {
				return err
			}
		case "AWS::Lambda::EventSourceMapping":
			if s.lambda == nil {
				return notImplemented("lambda service is not configured")
			}
			if err := s.lambda.DeleteEventSourceMappingByID(resource.Name); err != nil {
				return err
			}
		case "AWS::Lambda::Permission":
		case "AWS::CDK::Metadata":
		case "AWS::ApiGatewayV2::Stage":
			if s.apiGatewayV2 == nil {
				return notImplemented("apigatewayv2 service is not configured")
			}
			parts := strings.SplitN(resource.Name, "|", 2)
			if len(parts) != 2 {
				return validationError("invalid apigatewayv2 stage state")
			}
			if err := s.apiGatewayV2.DeleteStage(parts[0], parts[1]); err != nil {
				return err
			}
		case "AWS::ApiGatewayV2::Route":
			if s.apiGatewayV2 == nil {
				return notImplemented("apigatewayv2 service is not configured")
			}
			parts := strings.SplitN(resource.Name, "|", 2)
			if len(parts) != 2 {
				return validationError("invalid apigatewayv2 route state")
			}
			if err := s.apiGatewayV2.DeleteRoute(parts[0], parts[1]); err != nil {
				return err
			}
		case "AWS::ApiGatewayV2::Integration":
			if s.apiGatewayV2 == nil {
				return notImplemented("apigatewayv2 service is not configured")
			}
			parts := strings.SplitN(resource.Name, "|", 2)
			if len(parts) != 2 {
				return validationError("invalid apigatewayv2 integration state")
			}
			if err := s.apiGatewayV2.DeleteIntegration(parts[0], parts[1]); err != nil {
				return err
			}
		case "AWS::ApiGatewayV2::Api":
			if s.apiGatewayV2 == nil {
				return notImplemented("apigatewayv2 service is not configured")
			}
			if err := s.apiGatewayV2.DeleteAPI(resource.Name); err != nil {
				return err
			}
		case "AWS::SNS::Topic":
			if s.sns == nil {
				return notImplemented("sns service is not configured")
			}
			if err := s.sns.DeleteTopic(resource.Name); err != nil {
				return err
			}
		case "AWS::Events::Rule":
			if s.events == nil {
				return notImplemented("events service is not configured")
			}
			parts := strings.SplitN(resource.Name, "|", 2)
			if len(parts) != 2 {
				return validationError("invalid events rule state")
			}
			if err := s.events.DeleteRule(parts[0], parts[1]); err != nil {
				return err
			}
		case "AWS::Events::EventBus":
			if s.events == nil {
				return notImplemented("events service is not configured")
			}
			if err := s.events.DeleteEventBus(resource.Name); err != nil {
				return err
			}
		case "AWS::SecretsManager::Secret":
			if s.secretsManager == nil {
				return notImplemented("secretsmanager service is not configured")
			}
			if err := s.secretsManager.DeleteSecret(resource.Name); err != nil {
				return err
			}
		case "AWS::Kinesis::Stream":
			if s.kinesis == nil {
				return notImplemented("kinesis service is not configured")
			}
			if err := s.kinesis.DeleteStreamByName(resource.Name); err != nil {
				return err
			}
		case "AWS::Cognito::UserPoolClient":
			if s.cognitoIDP == nil {
				return notImplemented("cognitoidp service is not configured")
			}
			if err := s.cognitoIDP.DeleteUserPoolClient(resource.Name); err != nil {
				return err
			}
		case "AWS::Cognito::UserPool":
			if s.cognitoIDP == nil {
				return notImplemented("cognitoidp service is not configured")
			}
			if err := s.cognitoIDP.DeleteUserPool(resource.Name); err != nil {
				return err
			}
		case "AWS::StepFunctions::StateMachine":
			if s.stepFunctions == nil {
				return notImplemented("stepfunctions service is not configured")
			}
			if err := s.stepFunctions.DeleteStateMachineByARN(resource.Name); err != nil {
				return err
			}
		case "AWS::ApiGateway::RestApi":
			if s.apiGateway == nil {
				return notImplemented("apigateway service is not configured")
			}
			if err := s.apiGateway.DeleteAPI(resource.Name); err != nil {
				return err
			}
		case "AWS::ApiGateway::Resource":
		case "AWS::ApiGateway::Method":
		case "AWS::ApiGateway::Deployment":
		case "AWS::ApiGateway::Stage":
			// API-scoped subresources are cleaned up when the RestApi is deleted.
		case "AWS::SSM::Parameter":
			if s.ssm == nil {
				return notImplemented("ssm service is not configured")
			}
			if err := s.ssm.DeleteParameterByName(resource.Name); err != nil {
				return err
			}
		case "AWS::KMS::Alias":
			if s.kms == nil {
				return notImplemented("kms service is not configured")
			}
			if err := s.kms.DeleteAlias(resource.Name); err != nil {
				return err
			}
		case "AWS::KMS::Key":
			if s.kms == nil {
				return notImplemented("kms service is not configured")
			}
			if err := s.kms.DeleteKey(resource.Name); err != nil {
				return err
			}
		default:
			return notImplemented("resource type " + resource.Type + " cannot be deleted")
		}
	}
	return nil
}

func defaultPhysicalName(stackName, logicalID string) string {
	return strings.ToLower(stackName + "-" + logicalID)
}

func resolveOptionalString(value any, ctx *templateContext, fallback string) (string, error) {
	if value == nil {
		return fallback, nil
	}
	return resolveString(value, ctx)
}

func resolveString(value any, ctx *templateContext) (string, error) {
	resolved, err := resolveValue(value, ctx)
	if err != nil {
		return "", err
	}
	out, ok := resolved.(string)
	if !ok {
		return "", validationError("property must resolve to a string")
	}
	return out, nil
}

func resolveInt(value any, ctx *templateContext, fallback int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	resolved, err := resolveValue(value, ctx)
	if err != nil {
		return 0, err
	}
	switch typed := resolved.(type) {
	case float64:
		return int(typed), nil
	case int:
		return typed, nil
	case string:
		i, err := strconv.Atoi(typed)
		if err != nil {
			return 0, validationError("property must resolve to an integer")
		}
		return i, nil
	default:
		return 0, validationError("property must resolve to an integer")
	}
}

func resolveBool(value any, ctx *templateContext, fallback bool) (bool, error) {
	if value == nil {
		return fallback, nil
	}
	resolved, err := resolveValue(value, ctx)
	if err != nil {
		return false, err
	}
	switch typed := resolved.(type) {
	case bool:
		return typed, nil
	case string:
		b, err := strconv.ParseBool(typed)
		if err != nil {
			return false, validationError("property must resolve to a boolean")
		}
		return b, nil
	default:
		return false, validationError("property must resolve to a boolean")
	}
}

func resolveValue(value any, ctx *templateContext) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["Ref"]; ok && len(typed) == 1 {
			name, ok := ref.(string)
			if !ok {
				return nil, validationError("Ref must target a parameter name")
			}
			if resolved, ok := ctx.Parameters[name]; ok {
				return resolved, nil
			}
			if resource, ok := ctx.Resources[name]; ok {
				return resource.RefValue, nil
			}
			switch name {
			case "AWS::AccountId":
				return accountID, nil
			case "AWS::Region":
				return "us-east-1", nil
			case "AWS::Partition":
				return "aws", nil
			default:
				return nil, &dependencyError{Resource: name}
			}
		}
		if raw, ok := typed["Fn::GetAtt"]; ok && len(typed) == 1 {
			logicalID, attribute, err := parseGetAtt(raw)
			if err != nil {
				return nil, err
			}
			resource, ok := ctx.Resources[logicalID]
			if !ok {
				return nil, &dependencyError{Resource: logicalID}
			}
			value, ok := resource.Attrs[attribute]
			if !ok {
				return nil, notImplemented("Fn::GetAtt attribute " + logicalID + "." + attribute + " is not implemented")
			}
			return value, nil
		}
		if raw, ok := typed["Fn::Join"]; ok && len(typed) == 1 {
			return resolveJoin(raw, ctx)
		}
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved, err := resolveValue(item, ctx)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			resolved, err := resolveValue(item, ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	default:
		return value, nil
	}
}

func parseGetAtt(raw any) (string, string, error) {
	switch typed := raw.(type) {
	case string:
		parts := strings.SplitN(typed, ".", 2)
		if len(parts) != 2 {
			return "", "", validationError("Fn::GetAtt must use LogicalId.Attribute syntax")
		}
		return parts[0], parts[1], nil
	case []any:
		if len(typed) != 2 {
			return "", "", validationError("Fn::GetAtt must contain two items")
		}
		logicalID, ok := typed[0].(string)
		if !ok {
			return "", "", validationError("Fn::GetAtt logical id must be a string")
		}
		attribute, ok := typed[1].(string)
		if !ok {
			return "", "", validationError("Fn::GetAtt attribute must be a string")
		}
		return logicalID, attribute, nil
	default:
		return "", "", validationError("Fn::GetAtt is not valid")
	}
}

func resolveJoin(raw any, ctx *templateContext) (string, error) {
	parts, ok := raw.([]any)
	if !ok || len(parts) != 2 {
		return "", validationError("Fn::Join must contain a delimiter and a list")
	}
	delimiter, ok := parts[0].(string)
	if !ok {
		return "", validationError("Fn::Join delimiter must be a string")
	}
	items, ok := parts[1].([]any)
	if !ok {
		return "", validationError("Fn::Join values must be a list")
	}
	resolved := make([]string, 0, len(items))
	for _, item := range items {
		value, err := resolveString(item, ctx)
		if err != nil {
			return "", err
		}
		resolved = append(resolved, value)
	}
	return strings.Join(resolved, delimiter), nil
}

func (s *Service) createLogGroup(name string) error {
	raw, err := s.metadata.Get(logGroupsBucket, name)
	if err != nil {
		return internal(err)
	}
	if raw != nil {
		return validationError("log group " + name + " already exists")
	}
	record := logGroupRecord{Name: name, CreatedAt: s.now().UTC()}
	payload, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(logGroupsBucket, name, payload); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) sqsAttributes(properties map[string]any, ctx *templateContext) (map[string]string, error) {
	attrs := map[string]string{
		"DelaySeconds":                  "0",
		"MessageRetentionPeriod":        "345600",
		"ReceiveMessageWaitTimeSeconds": "0",
		"VisibilityTimeout":             "30",
	}
	for key, value := range properties {
		switch key {
		case "QueueName":
		case "VisibilityTimeout", "DelaySeconds", "MessageRetentionPeriod", "ReceiveMessageWaitTimeSeconds":
			i, err := resolveInt(value, ctx, 0)
			if err != nil {
				return nil, err
			}
			attrs[key] = strconv.Itoa(i)
		case "FifoQueue":
			enabled, err := resolveBool(value, ctx, false)
			if err != nil {
				return nil, err
			}
			if enabled {
				return nil, notImplemented("FIFO queues are not implemented")
			}
		default:
			return nil, notImplemented("AWS::SQS::Queue property " + key + " is not implemented")
		}
	}
	return attrs, nil
}

func (s *Service) createQueue(name string, attrs map[string]string) error {
	if strings.HasSuffix(name, ".fifo") {
		return notImplemented("FIFO queues are not implemented")
	}
	raw, err := s.metadata.Get(sqsQueuesBucket, name)
	if err != nil {
		return internal(err)
	}
	if raw != nil {
		return validationError("queue " + name + " already exists")
	}
	record := sqsQueueRecord{Name: name, CreatedAt: s.now().UTC(), Attributes: attrs}
	payload, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(sqsQueuesBucket, name, payload); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) createIAMRole(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	roleName, err := resolveOptionalString(properties["RoleName"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	pathValue := "/"
	if raw, ok := properties["Path"]; ok {
		pathValue, err = resolveString(raw, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		if !strings.HasPrefix(pathValue, "/") || !strings.HasSuffix(pathValue, "/") {
			return nil, resourceState{}, validationError("AWS::IAM::Role Path must begin and end with /")
		}
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	maxSessionDuration, err := resolveInt(properties["MaxSessionDuration"], ctx, 3600)
	if err != nil {
		return nil, resourceState{}, err
	}
	if _, ok := properties["PermissionsBoundary"]; ok {
		return nil, resourceState{}, notImplemented("AWS::IAM::Role PermissionsBoundary is not implemented")
	}
	if _, ok := properties["Tags"]; ok {
		return nil, resourceState{}, notImplemented("AWS::IAM::Role Tags is not implemented")
	}
	if managedPolicyArns, ok := properties["ManagedPolicyArns"]; ok {
		list, ok := managedPolicyArns.([]any)
		if !ok {
			return nil, resourceState{}, validationError("AWS::IAM::Role ManagedPolicyArns must be a list")
		}
		for _, item := range list {
			if _, err := resolveString(item, ctx); err != nil {
				return nil, resourceState{}, err
			}
		}
	}

	assumeDoc, err := resolveValue(properties["AssumeRolePolicyDocument"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	if assumeDoc == nil {
		return nil, resourceState{}, validationError("AWS::IAM::Role AssumeRolePolicyDocument is required")
	}
	assumeJSON, err := json.Marshal(assumeDoc)
	if err != nil {
		return nil, resourceState{}, internal(err)
	}
	raw, err := s.metadata.Get(iamRolesBucket, roleName)
	if err != nil {
		return nil, resourceState{}, internal(err)
	}
	if raw != nil {
		return nil, resourceState{}, validationError("role " + roleName + " already exists")
	}
	role := iamRoleRecord{
		AssumeRolePolicyDocument: string(assumeJSON),
		CreateDate:               s.now().UTC(),
		Description:              description,
		MaxSessionDuration:       maxSessionDuration,
		Path:                     pathValue,
		RoleID:                   "AROA" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:16],
		RoleName:                 roleName,
	}
	rolePayload, err := json.Marshal(role)
	if err != nil {
		return nil, resourceState{}, internal(err)
	}
	if err := s.metadata.Put(iamRolesBucket, roleName, rolePayload); err != nil {
		return nil, resourceState{}, internal(err)
	}

	if policies, ok := properties["Policies"]; ok {
		list, ok := policies.([]any)
		if !ok {
			return nil, resourceState{}, validationError("AWS::IAM::Role Policies must be a list")
		}
		for _, item := range list {
			policyMap, ok := item.(map[string]any)
			if !ok {
				return nil, resourceState{}, validationError("AWS::IAM::Role Policies entries must be objects")
			}
			policyName, err := resolveString(policyMap["PolicyName"], ctx)
			if err != nil {
				return nil, resourceState{}, err
			}
			policyDoc, err := resolveValue(policyMap["PolicyDocument"], ctx)
			if err != nil {
				return nil, resourceState{}, err
			}
			policyJSON, err := json.Marshal(policyDoc)
			if err != nil {
				return nil, resourceState{}, internal(err)
			}
			record := iamRolePolicyRecord{
				PolicyDocument: string(policyJSON),
				PolicyName:     policyName,
				RoleName:       roleName,
			}
			payload, err := json.Marshal(record)
			if err != nil {
				return nil, resourceState{}, internal(err)
			}
			if err := s.metadata.Put(iamRolePoliciesBucket, roleName+"|"+policyName, payload); err != nil {
				return nil, resourceState{}, internal(err)
			}
		}
	}

	return []managedResource{{LogicalID: logicalID, Type: "AWS::IAM::Role", Name: roleName}}, resourceState{
		RefValue: roleName,
		Attrs: map[string]string{
			"Arn":      fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, roleName),
			"RoleName": roleName,
		},
	}, nil
}

func (s *Service) createIAMPolicy(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	policyName, err := resolveString(properties["PolicyName"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	policyDoc, err := resolveValue(properties["PolicyDocument"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	policyJSON, err := json.Marshal(policyDoc)
	if err != nil {
		return nil, resourceState{}, internal(err)
	}
	if _, ok := properties["Users"]; ok {
		return nil, resourceState{}, notImplemented("AWS::IAM::Policy Users is not implemented")
	}
	if _, ok := properties["Groups"]; ok {
		return nil, resourceState{}, notImplemented("AWS::IAM::Policy Groups is not implemented")
	}

	roleRefs, ok := properties["Roles"].([]any)
	if !ok || len(roleRefs) == 0 {
		return nil, resourceState{}, validationError("AWS::IAM::Policy Roles must contain at least one role")
	}

	managed := make([]managedResource, 0, len(roleRefs))
	for _, roleRef := range roleRefs {
		roleName, err := resolveString(roleRef, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		record := iamRolePolicyRecord{
			PolicyDocument: string(policyJSON),
			PolicyName:     policyName,
			RoleName:       roleName,
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return nil, resourceState{}, internal(err)
		}
		key := roleName + "|" + policyName
		if err := s.metadata.Put(iamRolePoliciesBucket, key, payload); err != nil {
			return nil, resourceState{}, internal(err)
		}
		managed = append(managed, managedResource{LogicalID: logicalID, Type: "AWS::IAM::Policy", Name: key})
	}

	return managed, resourceState{RefValue: policyName}, nil
}

func (s *Service) dynamoTableRecord(tableName string, properties map[string]any, ctx *templateContext) (dynamoTableRecord, error) {
	if _, ok := properties["ProvisionedThroughput"]; ok {
		return dynamoTableRecord{}, notImplemented("AWS::DynamoDB::Table ProvisionedThroughput is not implemented")
	}
	if _, ok := properties["GlobalSecondaryIndexes"]; ok {
		return dynamoTableRecord{}, notImplemented("AWS::DynamoDB::Table secondary indexes are not implemented")
	}
	if _, ok := properties["StreamSpecification"]; ok {
		return dynamoTableRecord{}, notImplemented("AWS::DynamoDB::Table streams are not implemented")
	}

	attrDefsRaw, ok := properties["AttributeDefinitions"].([]any)
	if !ok || len(attrDefsRaw) == 0 {
		return dynamoTableRecord{}, validationError("AWS::DynamoDB::Table AttributeDefinitions are required")
	}
	keySchemaRaw, ok := properties["KeySchema"].([]any)
	if !ok || len(keySchemaRaw) != 1 {
		return dynamoTableRecord{}, notImplemented("AWS::DynamoDB::Table requires a single HASH key")
	}

	attrDefs := make([]dynamoAttributeDefinition, 0, len(attrDefsRaw))
	for _, item := range attrDefsRaw {
		entry, ok := item.(map[string]any)
		if !ok {
			return dynamoTableRecord{}, validationError("AttributeDefinitions entries must be objects")
		}
		name, err := resolveString(entry["AttributeName"], ctx)
		if err != nil {
			return dynamoTableRecord{}, err
		}
		attrType, err := resolveString(entry["AttributeType"], ctx)
		if err != nil {
			return dynamoTableRecord{}, err
		}
		attrDefs = append(attrDefs, dynamoAttributeDefinition{AttributeName: name, AttributeType: attrType})
	}

	keyEntry, ok := keySchemaRaw[0].(map[string]any)
	if !ok {
		return dynamoTableRecord{}, validationError("KeySchema entries must be objects")
	}
	hashKey, err := resolveString(keyEntry["AttributeName"], ctx)
	if err != nil {
		return dynamoTableRecord{}, err
	}
	keyType, err := resolveString(keyEntry["KeyType"], ctx)
	if err != nil {
		return dynamoTableRecord{}, err
	}
	if keyType != "HASH" {
		return dynamoTableRecord{}, notImplemented("AWS::DynamoDB::Table requires a single HASH key")
	}

	hashType := ""
	for _, attr := range attrDefs {
		if attr.AttributeName == hashKey {
			hashType = attr.AttributeType
			break
		}
	}
	if hashType == "" {
		return dynamoTableRecord{}, validationError("HASH key attribute definition is required")
	}

	billingMode, err := resolveOptionalString(properties["BillingMode"], ctx, "PAY_PER_REQUEST")
	if err != nil {
		return dynamoTableRecord{}, err
	}
	return dynamoTableRecord{
		AttributeDefinitions: attrDefs,
		BillingMode:          billingMode,
		CreatedAt:            s.now().UTC(),
		HashKey:              hashKey,
		HashKeyType:          hashType,
		TableName:            tableName,
		TableStatus:          "ACTIVE",
	}, nil
}

func (s *Service) createDynamoTable(record dynamoTableRecord) error {
	raw, err := s.metadata.Get(dynamoTablesBucket, record.TableName)
	if err != nil {
		return internal(err)
	}
	if raw != nil {
		return validationError("table " + record.TableName + " already exists")
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(dynamoTablesBucket, record.TableName, payload); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) createLambdaFunction(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.lambda == nil {
		return nil, resourceState{}, notImplemented("lambda service is not configured")
	}
	name, err := resolveOptionalString(properties["FunctionName"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	handler, err := resolveString(properties["Handler"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	role, err := resolveString(properties["Role"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	runtime, err := resolveString(properties["Runtime"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	timeout, err := resolveInt(properties["Timeout"], ctx, 3)
	if err != nil {
		return nil, resourceState{}, err
	}
	memorySize, err := resolveInt(properties["MemorySize"], ctx, 128)
	if err != nil {
		return nil, resourceState{}, err
	}
	environment, err := resolveEnvironmentVariables(properties["Environment"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	codeMap, ok := properties["Code"].(map[string]any)
	if !ok {
		return nil, resourceState{}, validationError("AWS::Lambda::Function Code is required")
	}
	var zipBytes []byte
	if raw := codeMap["ZipFile"]; raw != nil {
		inlineCode, err := resolveString(raw, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		zipBytes, err = buildInlineLambdaZip(runtime, handler, inlineCode)
		if err != nil {
			return nil, resourceState{}, err
		}
	} else if codeMap["S3Bucket"] != nil || codeMap["S3Key"] != nil {
		if s.s3 == nil {
			return nil, resourceState{}, notImplemented("s3 service is not configured")
		}
		bucket, err := resolveString(codeMap["S3Bucket"], ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		key, err := resolveString(codeMap["S3Key"], ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		if codeMap["S3ObjectVersion"] != nil {
			return nil, resourceState{}, notImplemented("AWS::Lambda::Function S3ObjectVersion is not implemented")
		}
		zipBytes, err = s.s3.ReadObjectBytes(bucket, key)
		if err != nil {
			return nil, resourceState{}, err
		}
	} else {
		return nil, resourceState{}, validationError("AWS::Lambda::Function Code must include ZipFile or S3Bucket/S3Key")
	}
	for _, unsupported := range []string{"Layers", "VpcConfig", "KmsKeyArn", "TracingConfig", "DeadLetterConfig", "ReservedConcurrentExecutions"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::Lambda::Function property " + unsupported + " is not implemented")
		}
	}
	arn, err := s.lambda.Provision(lambdasvc.ProvisionInput{
		CodeZip:      zipBytes,
		Description:  description,
		Environment:  environment,
		FunctionName: name,
		Handler:      handler,
		MemorySize:   memorySize,
		Role:         role,
		Runtime:      runtime,
		Timeout:      timeout,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Lambda::Function", Name: name}}, resourceState{
		RefValue: name,
		Attrs: map[string]string{
			"Arn": arn,
		},
	}, nil
}

func (s *Service) createLambdaEventSourceMapping(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.lambda == nil {
		return nil, resourceState{}, notImplemented("lambda service is not configured")
	}
	functionName, err := resolveString(properties["FunctionName"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	eventSourceArn, err := resolveString(properties["EventSourceArn"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	batchSize, err := resolveInt(properties["BatchSize"], ctx, 1)
	if err != nil {
		return nil, resourceState{}, err
	}
	enabled, err := resolveBool(properties["Enabled"], ctx, true)
	if err != nil {
		return nil, resourceState{}, err
	}
	startingPosition, err := resolveOptionalString(properties["StartingPosition"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	for _, unsupported := range []string{"BisectBatchOnFunctionError", "DestinationConfig", "FunctionResponseTypes", "MaximumBatchingWindowInSeconds", "MaximumRecordAgeInSeconds", "MaximumRetryAttempts", "ParallelizationFactor", "Queues", "Topics", "TumblingWindowInSeconds"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::Lambda::EventSourceMapping property " + unsupported + " is not implemented")
		}
	}

	mappingID, err := s.lambda.CreateEventSourceMappingRecord(functionName, eventSourceArn, batchSize, startingPosition, &enabled)
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Lambda::EventSourceMapping", Name: mappingID}}, resourceState{
		RefValue: mappingID,
		Attrs: map[string]string{
			"Id": mappingID,
		},
	}, nil
}

func (s *Service) createLambdaPermission(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if _, err := resolveString(properties["Action"], ctx); err != nil {
		return nil, resourceState{}, err
	}
	if _, err := resolveString(properties["FunctionName"], ctx); err != nil {
		return nil, resourceState{}, err
	}
	if _, err := resolveString(properties["Principal"], ctx); err != nil {
		return nil, resourceState{}, err
	}
	if raw := properties["SourceArn"]; raw != nil {
		if _, err := resolveString(raw, ctx); err != nil {
			return nil, resourceState{}, err
		}
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Lambda::Permission", Name: logicalID}}, resourceState{RefValue: logicalID}, nil
}

func (s *Service) createAPIGatewayV2API(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGatewayV2 == nil {
		return nil, resourceState{}, notImplemented("apigatewayv2 service is not configured")
	}
	name, err := resolveString(properties["Name"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	protocolType, err := resolveString(properties["ProtocolType"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	disableExecuteAPIEndpoint, err := resolveBool(properties["DisableExecuteApiEndpoint"], ctx, false)
	if err != nil {
		return nil, resourceState{}, err
	}
	routeSelectionExpression, err := resolveOptionalString(properties["RouteSelectionExpression"], ctx, "$request.method $request.path")
	if err != nil {
		return nil, resourceState{}, err
	}
	record, err := s.apiGatewayV2.CreateAPI(apigatewayv2.CreateAPIInput{
		Description:               description,
		DisableExecuteAPIEndpoint: disableExecuteAPIEndpoint,
		Name:                      name,
		ProtocolType:              protocolType,
		RouteSelectionExpression:  routeSelectionExpression,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGatewayV2::Api", Name: record.APIID}}, resourceState{
		RefValue: record.APIID,
		Attrs: map[string]string{
			"ApiEndpoint": fmt.Sprintf("http://127.0.0.1:4566/_aws/execute-api/%s", record.APIID),
		},
	}, nil
}

func (s *Service) createAPIGatewayV2Integration(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGatewayV2 == nil {
		return nil, resourceState{}, notImplemented("apigatewayv2 service is not configured")
	}
	apiID, err := resolveString(properties["ApiId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	integrationType, err := resolveString(properties["IntegrationType"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	integrationURI, err := resolveString(properties["IntegrationUri"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	payloadFormatVersion, err := resolveOptionalString(properties["PayloadFormatVersion"], ctx, "2.0")
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	integrationMethod, err := resolveOptionalString(properties["IntegrationMethod"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	timeoutInMillis, err := resolveInt(properties["TimeoutInMillis"], ctx, 30000)
	if err != nil {
		return nil, resourceState{}, err
	}
	record, err := s.apiGatewayV2.CreateIntegration(apiID, apigatewayv2.CreateIntegrationInput{
		Description:          description,
		IntegrationMethod:    integrationMethod,
		IntegrationType:      integrationType,
		IntegrationURI:       integrationURI,
		PayloadFormatVersion: payloadFormatVersion,
		TimeoutInMillis:      timeoutInMillis,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGatewayV2::Integration", Name: apiID + "|" + record.IntegrationID}}, resourceState{
		RefValue: record.IntegrationID,
	}, nil
}

func (s *Service) createAPIGatewayV2Route(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGatewayV2 == nil {
		return nil, resourceState{}, notImplemented("apigatewayv2 service is not configured")
	}
	apiID, err := resolveString(properties["ApiId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	routeKey, err := resolveString(properties["RouteKey"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	target, err := resolveString(properties["Target"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	authorizationType, err := resolveOptionalString(properties["AuthorizationType"], ctx, "NONE")
	if err != nil {
		return nil, resourceState{}, err
	}
	operationName, err := resolveOptionalString(properties["OperationName"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	record, err := s.apiGatewayV2.CreateRoute(apiID, apigatewayv2.CreateRouteInput{
		AuthorizationType: authorizationType,
		OperationName:     operationName,
		RouteKey:          routeKey,
		Target:            target,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGatewayV2::Route", Name: apiID + "|" + record.RouteID}}, resourceState{
		RefValue: record.RouteID,
	}, nil
}

func (s *Service) createAPIGatewayV2Stage(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGatewayV2 == nil {
		return nil, resourceState{}, notImplemented("apigatewayv2 service is not configured")
	}
	apiID, err := resolveString(properties["ApiId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	stageName, err := resolveString(properties["StageName"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	autoDeploy, err := resolveBool(properties["AutoDeploy"], ctx, false)
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	variables, err := resolveStringMap(properties["StageVariables"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	record, err := s.apiGatewayV2.CreateStage(apiID, apigatewayv2.CreateStageInput{
		AutoDeploy:     autoDeploy,
		Description:    description,
		StageName:      stageName,
		StageVariables: variables,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGatewayV2::Stage", Name: apiID + "|" + record.StageName}}, resourceState{
		RefValue: record.StageName,
	}, nil
}

func (s *Service) createSNSTopic(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.sns == nil {
		return nil, resourceState{}, notImplemented("sns service is not configured")
	}
	name, err := resolveOptionalString(properties["TopicName"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	attrs := map[string]string{}
	if raw := properties["DisplayName"]; raw != nil {
		value, err := resolveString(raw, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		attrs["DisplayName"] = value
	}
	for _, unsupported := range []string{"Subscription", "Tags", "FifoTopic", "ContentBasedDeduplication"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::SNS::Topic property " + unsupported + " is not implemented")
		}
	}
	arn, err := s.sns.CreateTopic(sns.CreateTopicInput{Name: name, Attributes: attrs})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::SNS::Topic", Name: arn}}, resourceState{
		RefValue: arn,
		Attrs:    map[string]string{"TopicArn": arn},
	}, nil
}

func (s *Service) createEventBus(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.events == nil {
		return nil, resourceState{}, notImplemented("events service is not configured")
	}
	name, err := resolveOptionalString(properties["Name"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	for _, unsupported := range []string{"Policy", "DeadLetterConfig", "KmsKeyIdentifier", "Tags"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::Events::EventBus property " + unsupported + " is not implemented")
		}
	}
	arn, err := s.events.CreateEventBus(name)
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Events::EventBus", Name: name}}, resourceState{
		RefValue: name,
		Attrs:    map[string]string{"Arn": arn, "Name": name},
	}, nil
}

func (s *Service) createEventRule(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.events == nil {
		return nil, resourceState{}, notImplemented("events service is not configured")
	}
	name, err := resolveOptionalString(properties["Name"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	eventBusName, err := resolveOptionalString(properties["EventBusName"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	state, err := resolveOptionalString(properties["State"], ctx, "ENABLED")
	if err != nil {
		return nil, resourceState{}, err
	}
	if _, ok := properties["ScheduleExpression"]; ok {
		return nil, resourceState{}, notImplemented("AWS::Events::Rule ScheduleExpression is not implemented")
	}
	patternValue, err := resolveValue(properties["EventPattern"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	if patternValue == nil {
		return nil, resourceState{}, validationError("AWS::Events::Rule EventPattern is required")
	}
	patternJSON, err := json.Marshal(patternValue)
	if err != nil {
		return nil, resourceState{}, internal(err)
	}
	arn, err := s.events.PutRule(eventssvc.PutRuleInput{
		Description:  description,
		EventBusName: eventBusName,
		EventPattern: string(patternJSON),
		Name:         name,
		State:        state,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	if rawTargets, ok := properties["Targets"]; ok {
		list, ok := rawTargets.([]any)
		if !ok {
			return nil, resourceState{}, validationError("AWS::Events::Rule Targets must be a list")
		}
		targets := make([]eventssvc.TargetInput, 0, len(list))
		for idx, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				return nil, resourceState{}, validationError("AWS::Events::Rule Targets entries must be objects")
			}
			targetArn, err := resolveString(entry["Arn"], ctx)
			if err != nil {
				return nil, resourceState{}, err
			}
			targetID, err := resolveOptionalString(entry["Id"], ctx, fmt.Sprintf("%sTarget%d", logicalID, idx+1))
			if err != nil {
				return nil, resourceState{}, err
			}
			inputValue, err := resolveOptionalString(entry["Input"], ctx, "")
			if err != nil {
				return nil, resourceState{}, err
			}
			for _, unsupported := range []string{"InputPath", "InputTransformer", "RoleArn"} {
				if _, ok := entry[unsupported]; ok {
					return nil, resourceState{}, notImplemented("AWS::Events::Rule target property " + unsupported + " is not implemented")
				}
			}
			targets = append(targets, eventssvc.TargetInput{Arn: targetArn, ID: targetID, Input: inputValue})
		}
		if err := s.events.PutTargets(eventssvc.PutTargetsInput{EventBusName: eventBusName, Rule: name, Targets: targets}); err != nil {
			return nil, resourceState{}, err
		}
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Events::Rule", Name: eventBusName + "|" + name}}, resourceState{
		RefValue: name,
		Attrs:    map[string]string{"Arn": arn},
	}, nil
}

func (s *Service) createSecret(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.secretsManager == nil {
		return nil, resourceState{}, notImplemented("secretsmanager service is not configured")
	}
	name, err := resolveOptionalString(properties["Name"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	kmsKeyID, err := resolveOptionalString(properties["KmsKeyId"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	secretString, err := resolveOptionalString(properties["SecretString"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	for _, unsupported := range []string{"GenerateSecretString", "ReplicaRegions", "Tags"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::SecretsManager::Secret property " + unsupported + " is not implemented")
		}
	}
	arn, err := s.secretsManager.CreateSecret(secretsmanager.CreateSecretInput{
		Description:  description,
		KMSKeyID:     kmsKeyID,
		Name:         name,
		SecretString: secretString,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::SecretsManager::Secret", Name: arn}}, resourceState{
		RefValue: arn,
		Attrs:    map[string]string{"Arn": arn},
	}, nil
}

func (s *Service) createKinesisStream(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.kinesis == nil {
		return nil, resourceState{}, notImplemented("kinesis service is not configured")
	}
	name, err := resolveOptionalString(properties["Name"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	shardCount, err := resolveInt(properties["ShardCount"], ctx, 1)
	if err != nil {
		return nil, resourceState{}, err
	}
	mode := "PROVISIONED"
	if raw := properties["StreamModeDetails"]; raw != nil {
		modeMap, ok := raw.(map[string]any)
		if !ok {
			return nil, resourceState{}, validationError("AWS::Kinesis::Stream StreamModeDetails must be an object")
		}
		mode, err = resolveOptionalString(modeMap["StreamMode"], ctx, mode)
		if err != nil {
			return nil, resourceState{}, err
		}
	}
	for _, unsupported := range []string{"RetentionPeriodHours", "ShardLevelMetrics", "Tags", "StreamEncryption"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::Kinesis::Stream property " + unsupported + " is not implemented")
		}
	}
	arn, err := s.kinesis.CreateStream(kinesis.CreateStreamInput{Mode: mode, ShardCount: shardCount, StreamName: name})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Kinesis::Stream", Name: name}}, resourceState{
		RefValue: name,
		Attrs:    map[string]string{"Arn": arn},
	}, nil
}

func (s *Service) createCognitoUserPool(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.cognitoIDP == nil {
		return nil, resourceState{}, notImplemented("cognitoidp service is not configured")
	}
	name, err := resolveOptionalString(properties["UserPoolName"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	usernameAttrs := make([]string, 0)
	if raw := properties["UsernameAttributes"]; raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, resourceState{}, validationError("AWS::Cognito::UserPool UsernameAttributes must be a list")
		}
		for _, item := range list {
			value, err := resolveString(item, ctx)
			if err != nil {
				return nil, resourceState{}, err
			}
			usernameAttrs = append(usernameAttrs, value)
		}
	}
	for _, unsupported := range []string{"AliasAttributes", "Policies", "Schema", "AutoVerifiedAttributes", "LambdaConfig", "MfaConfiguration", "UserPoolTags"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::Cognito::UserPool property " + unsupported + " is not implemented")
		}
	}
	poolID, arn, err := s.cognitoIDP.CreateUserPool(cognitoidp.CreateUserPoolInput{PoolName: name, UsernameAttributes: usernameAttrs})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Cognito::UserPool", Name: poolID}}, resourceState{
		RefValue: poolID,
		Attrs: map[string]string{
			"Arn":          arn,
			"ProviderName": "cognito-idp.us-east-1.amazonaws.com/" + poolID,
			"ProviderURL":  "cognito-idp.us-east-1.amazonaws.com/" + poolID,
		},
	}, nil
}

func (s *Service) createCognitoUserPoolClient(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.cognitoIDP == nil {
		return nil, resourceState{}, notImplemented("cognitoidp service is not configured")
	}
	poolID, err := resolveString(properties["UserPoolId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	clientName, err := resolveOptionalString(properties["ClientName"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	flows := []string{}
	if raw := properties["ExplicitAuthFlows"]; raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, resourceState{}, validationError("AWS::Cognito::UserPoolClient ExplicitAuthFlows must be a list")
		}
		for _, item := range list {
			value, err := resolveString(item, ctx)
			if err != nil {
				return nil, resourceState{}, err
			}
			flows = append(flows, value)
		}
	}
	for _, unsupported := range []string{"GenerateSecret", "AllowedOAuthFlows", "AllowedOAuthScopes", "AllowedOAuthFlowsUserPoolClient", "CallbackURLs", "LogoutURLs", "SupportedIdentityProviders"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::Cognito::UserPoolClient property " + unsupported + " is not implemented")
		}
	}
	clientID, err := s.cognitoIDP.CreateUserPoolClient(cognitoidp.CreateUserPoolClientInput{
		ClientName:        clientName,
		ExplicitAuthFlows: flows,
		UserPoolID:        poolID,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::Cognito::UserPoolClient", Name: clientID}}, resourceState{
		RefValue: clientID,
		Attrs:    map[string]string{"ClientId": clientID},
	}, nil
}

func (s *Service) createStateMachine(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.stepFunctions == nil {
		return nil, resourceState{}, notImplemented("stepfunctions service is not configured")
	}
	name, err := resolveOptionalString(properties["StateMachineName"], ctx, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	roleArn, err := resolveString(properties["RoleArn"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	stateMachineType, err := resolveOptionalString(properties["StateMachineType"], ctx, "STANDARD")
	if err != nil {
		return nil, resourceState{}, err
	}
	definition := ""
	switch {
	case properties["DefinitionString"] != nil:
		definition, err = resolveString(properties["DefinitionString"], ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
	case properties["Definition"] != nil:
		value, err := resolveValue(properties["Definition"], ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, resourceState{}, internal(err)
		}
		definition = string(raw)
	default:
		return nil, resourceState{}, validationError("AWS::StepFunctions::StateMachine requires Definition or DefinitionString")
	}
	for _, unsupported := range []string{"DefinitionSubstitutions", "LoggingConfiguration", "TracingConfiguration", "Tags"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::StepFunctions::StateMachine property " + unsupported + " is not implemented")
		}
	}
	arn, err := s.stepFunctions.CreateStateMachine(stepfunctions.CreateStateMachineInput{
		Definition: definition,
		Name:       name,
		RoleArn:    roleArn,
		Type:       stateMachineType,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::StepFunctions::StateMachine", Name: arn}}, resourceState{
		RefValue: arn,
		Attrs:    map[string]string{"Arn": arn, "Name": name},
	}, nil
}

func (s *Service) createAPIGatewayRestAPI(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGateway == nil {
		return nil, resourceState{}, notImplemented("apigateway service is not configured")
	}
	name, err := resolveString(properties["Name"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	for _, unsupported := range []string{"Body", "BodyS3Location", "BinaryMediaTypes", "CloneFrom", "EndpointConfiguration", "FailOnWarnings", "Policy"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::ApiGateway::RestApi property " + unsupported + " is not implemented")
		}
	}
	apiID, rootResourceID, err := s.apiGateway.CreateAPI(apigateway.CreateAPIInput{Name: name, Description: description})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGateway::RestApi", Name: apiID}}, resourceState{
		RefValue: apiID,
		Attrs:    map[string]string{"RootResourceId": rootResourceID},
	}, nil
}

func (s *Service) createAPIGatewayResource(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGateway == nil {
		return nil, resourceState{}, notImplemented("apigateway service is not configured")
	}
	apiID, err := resolveString(properties["RestApiId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	parentID, err := resolveString(properties["ParentId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	pathPart, err := resolveString(properties["PathPart"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	resourceID, path, err := s.apiGateway.CreateResource(apigateway.CreateResourceInput{APIID: apiID, ParentID: parentID, PathPart: pathPart})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGateway::Resource", Name: apiID + "|" + resourceID}}, resourceState{
		RefValue: resourceID,
		Attrs:    map[string]string{"Path": path},
	}, nil
}

func (s *Service) createAPIGatewayMethod(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGateway == nil {
		return nil, resourceState{}, notImplemented("apigateway service is not configured")
	}
	apiID, err := resolveString(properties["RestApiId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	resourceID, err := resolveString(properties["ResourceId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	httpMethod, err := resolveString(properties["HttpMethod"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	authorizationType, err := resolveOptionalString(properties["AuthorizationType"], ctx, "NONE")
	if err != nil {
		return nil, resourceState{}, err
	}
	if err := s.apiGateway.PutMethod(apigateway.PutMethodInput{
		APIID:             apiID,
		AuthorizationType: authorizationType,
		HTTPMethod:        httpMethod,
		ResourceID:        resourceID,
	}); err != nil {
		return nil, resourceState{}, err
	}
	if raw := properties["Integration"]; raw != nil {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, resourceState{}, validationError("AWS::ApiGateway::Method Integration must be an object")
		}
		integrationType, err := resolveString(entry["Type"], ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		uri, err := resolveString(entry["Uri"], ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		integrationMethod, err := resolveOptionalString(entry["IntegrationHttpMethod"], ctx, "POST")
		if err != nil {
			return nil, resourceState{}, err
		}
		if err := s.apiGateway.PutIntegration(apigateway.PutIntegrationInput{
			APIID:                 apiID,
			HTTPMethod:            httpMethod,
			IntegrationHTTPMethod: integrationMethod,
			ResourceID:            resourceID,
			Type:                  integrationType,
			URI:                   uri,
		}); err != nil {
			return nil, resourceState{}, err
		}
	}
	for _, unsupported := range []string{"ApiKeyRequired", "AuthorizerId", "AuthorizationScopes", "RequestModels", "RequestParameters", "MethodResponses", "OperationName"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::ApiGateway::Method property " + unsupported + " is not implemented")
		}
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGateway::Method", Name: apiID + "|" + resourceID + "|" + strings.ToUpper(httpMethod)}}, resourceState{
		RefValue: strings.ToUpper(httpMethod),
	}, nil
}

func (s *Service) createAPIGatewayDeployment(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGateway == nil {
		return nil, resourceState{}, notImplemented("apigateway service is not configured")
	}
	apiID, err := resolveString(properties["RestApiId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	stageName, err := resolveOptionalString(properties["StageName"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	deploymentID, err := s.apiGateway.CreateDeployment(apigateway.CreateDeploymentInput{
		APIID:       apiID,
		Description: description,
		StageName:   stageName,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGateway::Deployment", Name: apiID + "|" + deploymentID}}, resourceState{
		RefValue: deploymentID,
	}, nil
}

func (s *Service) createAPIGatewayStage(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.apiGateway == nil {
		return nil, resourceState{}, notImplemented("apigateway service is not configured")
	}
	apiID, err := resolveString(properties["RestApiId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	deploymentID, err := resolveString(properties["DeploymentId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	stageName, err := resolveString(properties["StageName"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	if err := s.apiGateway.CreateStage(apigateway.CreateStageInput{APIID: apiID, DeploymentID: deploymentID, StageName: stageName}); err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::ApiGateway::Stage", Name: apiID + "|" + stageName}}, resourceState{
		RefValue: stageName,
	}, nil
}

func (s *Service) createSSMParameter(stackName, logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.ssm == nil {
		return nil, resourceState{}, notImplemented("ssm service is not configured")
	}
	name, err := resolveOptionalString(properties["Name"], ctx, "/"+defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, resourceState{}, err
	}
	value, err := resolveString(properties["Value"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	paramType, err := resolveOptionalString(properties["Type"], ctx, "String")
	if err != nil {
		return nil, resourceState{}, err
	}
	for _, unsupported := range []string{"DataType", "Description", "Policies", "Tier", "Tags"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::SSM::Parameter property " + unsupported + " is not implemented")
		}
	}
	if err := s.ssm.PutParameter(ssm.PutParameterInput{Name: name, Type: paramType, Value: value}); err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::SSM::Parameter", Name: name}}, resourceState{
		RefValue: name,
	}, nil
}

func (s *Service) createKMSKey(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.kms == nil {
		return nil, resourceState{}, notImplemented("kms service is not configured")
	}
	description, err := resolveOptionalString(properties["Description"], ctx, "")
	if err != nil {
		return nil, resourceState{}, err
	}
	keySpec, err := resolveOptionalString(properties["KeySpec"], ctx, "SYMMETRIC_DEFAULT")
	if err != nil {
		return nil, resourceState{}, err
	}
	keyUsage, err := resolveOptionalString(properties["KeyUsage"], ctx, "ENCRYPT_DECRYPT")
	if err != nil {
		return nil, resourceState{}, err
	}
	multiRegion, err := resolveBool(properties["MultiRegion"], ctx, false)
	if err != nil {
		return nil, resourceState{}, err
	}
	policy := ""
	if raw := properties["KeyPolicy"]; raw != nil {
		value, err := resolveValue(raw, ctx)
		if err != nil {
			return nil, resourceState{}, err
		}
		policyJSON, err := json.Marshal(value)
		if err != nil {
			return nil, resourceState{}, internal(err)
		}
		policy = string(policyJSON)
	}
	for _, unsupported := range []string{"EnableKeyRotation", "Enabled", "PendingWindowInDays", "Tags"} {
		if _, ok := properties[unsupported]; ok {
			return nil, resourceState{}, notImplemented("AWS::KMS::Key property " + unsupported + " is not implemented")
		}
	}
	keyID, arn, err := s.kms.CreateKey(kms.CreateKeyInput{
		Description: description,
		KeySpec:     keySpec,
		KeyUsage:    keyUsage,
		MultiRegion: multiRegion,
		Policy:      policy,
	})
	if err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::KMS::Key", Name: keyID}}, resourceState{
		RefValue: keyID,
		Attrs:    map[string]string{"Arn": arn},
	}, nil
}

func (s *Service) createKMSAlias(logicalID string, properties map[string]any, ctx *templateContext) ([]managedResource, resourceState, error) {
	if s.kms == nil {
		return nil, resourceState{}, notImplemented("kms service is not configured")
	}
	aliasName, err := resolveString(properties["AliasName"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	targetKeyID, err := resolveString(properties["TargetKeyId"], ctx)
	if err != nil {
		return nil, resourceState{}, err
	}
	if err := s.kms.CreateAlias(aliasName, targetKeyID); err != nil {
		return nil, resourceState{}, err
	}
	return []managedResource{{LogicalID: logicalID, Type: "AWS::KMS::Alias", Name: aliasName}}, resourceState{
		RefValue: aliasName,
		Attrs:    map[string]string{"AliasName": aliasName},
	}, nil
}

func resolveEnvironmentVariables(value any, ctx *templateContext) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	envMap, ok := value.(map[string]any)
	if !ok {
		return nil, validationError("Environment must be an object")
	}
	return resolveStringMap(envMap["Variables"], ctx)
}

func resolveStringMap(value any, ctx *templateContext) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	resolved, err := resolveValue(value, ctx)
	if err != nil {
		return nil, err
	}
	typed, ok := resolved.(map[string]any)
	if !ok {
		return nil, validationError("property must resolve to a map")
	}
	out := make(map[string]string, len(typed))
	for key, value := range typed {
		str, ok := value.(string)
		if !ok {
			return nil, validationError("map values must resolve to strings")
		}
		out[key] = str
	}
	return out, nil
}

func buildInlineLambdaZip(runtime, handler, source string) ([]byte, error) {
	module := "index"
	if prefix, _, found := strings.Cut(handler, "."); found && prefix != "" {
		module = prefix
	}
	extension := ".py"
	switch {
	case strings.HasPrefix(runtime, "python"):
		extension = ".py"
	case strings.HasPrefix(runtime, "nodejs"):
		extension = ".js"
	default:
		return nil, notImplemented("inline ZipFile is not implemented for runtime " + runtime)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(module + extension)
	if err != nil {
		return nil, internal(err)
	}
	if _, err := w.Write([]byte(source)); err != nil {
		return nil, internal(err)
	}
	if err := zw.Close(); err != nil {
		return nil, internal(err)
	}
	return buf.Bytes(), nil
}

func stackToXML(record stackRecord) stackXML {
	return stackXML{
		Capabilities:        record.Capabilities,
		ClientRequestToken:  record.ClientRequestToken,
		CreationTime:        record.CreationTime.UTC().Format(time.RFC3339),
		Description:         record.Description,
		DisableRollback:     record.DisableRollback,
		Outputs:             []outputXML{},
		Parameters:          stackParametersToXML(record.Parameters),
		StackID:             record.StackID,
		StackName:           record.StackName,
		StackStatus:         record.StackStatus,
		TemplateDescription: record.TemplateDescription,
		TimeoutInMinutes:    record.TimeoutInMinutes,
	}
}

func stackParametersToXML(params []stackParameter) []templateParameterXML {
	out := make([]templateParameterXML, 0, len(params))
	for _, param := range params {
		out = append(out, templateParameterXML{
			DefaultValue:   param.DefaultValue,
			Description:    param.Description,
			NoEcho:         param.NoEcho,
			ParameterKey:   param.ParameterKey,
			ParameterValue: param.ParameterValue,
		})
	}
	return out
}

func validateStackName(name string) error {
	if !stackNameRe.MatchString(name) {
		return validationError("StackName must start with a letter and contain only alphanumeric characters or hyphens")
	}
	return nil
}

func (s *Service) loadStackByForm(form url.Values) (stackRecord, error) {
	stackName := form.Get("StackName")
	if stackName == "" {
		return stackRecord{}, validationError("StackName is required")
	}
	return s.loadStack(stackName)
}

func (s *Service) loadChangeSetByForm(form url.Values) (changeSetRecord, error) {
	changeSetName := form.Get("ChangeSetName")
	if changeSetName == "" {
		return changeSetRecord{}, validationError("ChangeSetName is required")
	}
	stackName := form.Get("StackName")
	id := changeSetName
	if !strings.Contains(changeSetName, ":changeSet/") {
		if stackName == "" {
			return changeSetRecord{}, validationError("StackName is required")
		}
		id = changeSetID(stackName, changeSetName)
	}
	raw, err := s.metadata.Get(changeSetsBucket, id)
	if err != nil {
		return changeSetRecord{}, internal(err)
	}
	if raw == nil {
		return changeSetRecord{}, validationError("ChangeSet " + changeSetName + " does not exist")
	}
	var record changeSetRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return changeSetRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadStack(stackName string) (stackRecord, error) {
	raw, err := s.metadata.Get(stacksBucket, stackName)
	if err != nil {
		return stackRecord{}, internal(err)
	}
	if raw == nil {
		return stackRecord{}, validationError("Stack with id " + stackName + " does not exist")
	}
	var record stackRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return stackRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putStack(record stackRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(stacksBucket, record.StackName, raw)
}

func (s *Service) putChangeSet(record changeSetRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(changeSetsBucket, record.ChangeSetID, raw)
}

func changeSetID(stackName, changeSetName string) string {
	return fmt.Sprintf("arn:aws:cloudformation:us-east-1:%s:changeSet/%s/%s", accountID, changeSetName, stackName)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func hasIndexedPrefix(form url.Values, prefix string) bool {
	for key := range form {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func validationError(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationError", Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}
