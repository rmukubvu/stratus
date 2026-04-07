package cloudformation

import (
	"encoding/json"
	"encoding/xml"
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

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	namespace             = "http://cloudformation.amazonaws.com/doc/2010-05-15/"
	accountID             = "000000000000"
	stacksBucket          = "cloudformation-stacks"
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
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
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

type describeStacksResponse struct {
	XMLName          xml.Name             `xml:"DescribeStacksResponse"`
	XMLNS            string               `xml:"xmlns,attr"`
	Result           describeStacksResult `xml:"DescribeStacksResult"`
	ResponseMetadata responseMetadata     `xml:"ResponseMetadata"`
}

type describeStacksResult struct {
	Stacks []stackXML `xml:"Stacks>member"`
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

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "ValidateTemplate":
		return s.validateTemplate(w, r, requestID)
	case "CreateStack":
		return s.createStack(w, r, requestID)
	case "DescribeStacks":
		return s.describeStacks(w, r, requestID)
	case "GetTemplate":
		return s.getTemplate(w, r, requestID)
	case "ListStacks":
		return s.listStacks(w, r, requestID)
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
	if hasIndexedPrefix(form, "Capabilities.member.") {
		return notImplemented("capabilities are not implemented")
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
	parameterValues := parameterValueMap(parameters)
	managedResources, err := s.applyResources(stackName, doc.Resources, parameterValues)
	if err != nil {
		return err
	}

	record := stackRecord{
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
	if err := r.ParseForm(); err != nil {
		return nil, validationError("request body is not valid form data")
	}
	return r.Form, nil
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
		return templateDocument{}, "", validationError("template body must be valid JSON")
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

func (s *Service) applyResources(stackName string, resources map[string]templateResource, parameters map[string]string) ([]managedResource, error) {
	logicalIDs := make([]string, 0, len(resources))
	for logicalID := range resources {
		logicalIDs = append(logicalIDs, logicalID)
	}
	sort.Strings(logicalIDs)

	var created []managedResource
	for _, logicalID := range logicalIDs {
		resource, err := s.applyResource(stackName, logicalID, resources[logicalID], parameters)
		if err != nil {
			_ = s.deleteManagedResources(created)
			return nil, err
		}
		created = append(created, resource...)
	}
	return created, nil
}

func (s *Service) applyResource(stackName, logicalID string, resource templateResource, parameters map[string]string) ([]managedResource, error) {
	switch resource.Type {
	case "AWS::Logs::LogGroup":
		name, err := resolveOptionalString(resource.Properties["LogGroupName"], parameters, defaultPhysicalName(stackName, logicalID))
		if err != nil {
			return nil, err
		}
		if _, ok := resource.Properties["RetentionInDays"]; ok {
			return nil, notImplemented("AWS::Logs::LogGroup RetentionInDays is not implemented")
		}
		if err := s.createLogGroup(name); err != nil {
			return nil, err
		}
		return []managedResource{{LogicalID: logicalID, Type: resource.Type, Name: name}}, nil
	case "AWS::SQS::Queue":
		name, err := resolveOptionalString(resource.Properties["QueueName"], parameters, defaultPhysicalName(stackName, logicalID))
		if err != nil {
			return nil, err
		}
		attrs, err := s.sqsAttributes(resource.Properties, parameters)
		if err != nil {
			return nil, err
		}
		if err := s.createQueue(name, attrs); err != nil {
			return nil, err
		}
		return []managedResource{{LogicalID: logicalID, Type: resource.Type, Name: name}}, nil
	case "AWS::IAM::Role":
		managed, err := s.createIAMRole(stackName, logicalID, resource.Properties, parameters)
		if err != nil {
			return nil, err
		}
		return managed, nil
	case "AWS::DynamoDB::Table":
		name, err := resolveOptionalString(resource.Properties["TableName"], parameters, defaultPhysicalName(stackName, logicalID))
		if err != nil {
			return nil, err
		}
		record, err := s.dynamoTableRecord(name, resource.Properties, parameters)
		if err != nil {
			return nil, err
		}
		if err := s.createDynamoTable(record); err != nil {
			return nil, err
		}
		return []managedResource{{LogicalID: logicalID, Type: resource.Type, Name: name}}, nil
	default:
		return nil, notImplemented("resource type " + resource.Type + " is not implemented")
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
		case "AWS::DynamoDB::Table":
			if err := s.metadata.Delete(dynamoTablesBucket, resource.Name); err != nil {
				return internal(err)
			}
			if err := s.metadata.DeletePrefix(dynamoItemsBucket, resource.Name+"|"); err != nil {
				return internal(err)
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

func resolveOptionalString(value any, parameters map[string]string, fallback string) (string, error) {
	if value == nil {
		return fallback, nil
	}
	return resolveString(value, parameters)
}

func resolveString(value any, parameters map[string]string) (string, error) {
	resolved, err := resolveValue(value, parameters)
	if err != nil {
		return "", err
	}
	out, ok := resolved.(string)
	if !ok {
		return "", validationError("property must resolve to a string")
	}
	return out, nil
}

func resolveInt(value any, parameters map[string]string, fallback int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	resolved, err := resolveValue(value, parameters)
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

func resolveBool(value any, parameters map[string]string, fallback bool) (bool, error) {
	if value == nil {
		return fallback, nil
	}
	resolved, err := resolveValue(value, parameters)
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

func resolveValue(value any, parameters map[string]string) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["Ref"]; ok && len(typed) == 1 {
			name, ok := ref.(string)
			if !ok {
				return nil, validationError("Ref must target a parameter name")
			}
			if resolved, ok := parameters[name]; ok {
				return resolved, nil
			}
			return nil, notImplemented("Ref to non-parameter " + name + " is not implemented")
		}
		if _, ok := typed["Fn::GetAtt"]; ok {
			return nil, notImplemented("Fn::GetAtt is not implemented")
		}
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved, err := resolveValue(item, parameters)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			resolved, err := resolveValue(item, parameters)
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

func (s *Service) sqsAttributes(properties map[string]any, parameters map[string]string) (map[string]string, error) {
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
			i, err := resolveInt(value, parameters, 0)
			if err != nil {
				return nil, err
			}
			attrs[key] = strconv.Itoa(i)
		case "FifoQueue":
			enabled, err := resolveBool(value, parameters, false)
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

func (s *Service) createIAMRole(stackName, logicalID string, properties map[string]any, parameters map[string]string) ([]managedResource, error) {
	roleName, err := resolveOptionalString(properties["RoleName"], parameters, defaultPhysicalName(stackName, logicalID))
	if err != nil {
		return nil, err
	}
	pathValue := "/"
	if raw, ok := properties["Path"]; ok {
		pathValue, err = resolveString(raw, parameters)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(pathValue, "/") || !strings.HasSuffix(pathValue, "/") {
			return nil, validationError("AWS::IAM::Role Path must begin and end with /")
		}
	}
	description, err := resolveOptionalString(properties["Description"], parameters, "")
	if err != nil {
		return nil, err
	}
	maxSessionDuration, err := resolveInt(properties["MaxSessionDuration"], parameters, 3600)
	if err != nil {
		return nil, err
	}
	if _, ok := properties["ManagedPolicyArns"]; ok {
		return nil, notImplemented("AWS::IAM::Role ManagedPolicyArns is not implemented")
	}
	if _, ok := properties["PermissionsBoundary"]; ok {
		return nil, notImplemented("AWS::IAM::Role PermissionsBoundary is not implemented")
	}
	if _, ok := properties["Tags"]; ok {
		return nil, notImplemented("AWS::IAM::Role Tags is not implemented")
	}

	assumeDoc, err := resolveValue(properties["AssumeRolePolicyDocument"], parameters)
	if err != nil {
		return nil, err
	}
	if assumeDoc == nil {
		return nil, validationError("AWS::IAM::Role AssumeRolePolicyDocument is required")
	}
	assumeJSON, err := json.Marshal(assumeDoc)
	if err != nil {
		return nil, internal(err)
	}
	if _, err := s.metadata.Get(iamRolesBucket, roleName); err != nil {
		return nil, internal(err)
	} else if raw, _ := s.metadata.Get(iamRolesBucket, roleName); raw != nil {
		return nil, validationError("role " + roleName + " already exists")
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
		return nil, internal(err)
	}
	if err := s.metadata.Put(iamRolesBucket, roleName, rolePayload); err != nil {
		return nil, internal(err)
	}

	if policies, ok := properties["Policies"]; ok {
		list, ok := policies.([]any)
		if !ok {
			return nil, validationError("AWS::IAM::Role Policies must be a list")
		}
		for _, item := range list {
			policyMap, ok := item.(map[string]any)
			if !ok {
				return nil, validationError("AWS::IAM::Role Policies entries must be objects")
			}
			policyName, err := resolveString(policyMap["PolicyName"], parameters)
			if err != nil {
				return nil, err
			}
			policyDoc, err := resolveValue(policyMap["PolicyDocument"], parameters)
			if err != nil {
				return nil, err
			}
			policyJSON, err := json.Marshal(policyDoc)
			if err != nil {
				return nil, internal(err)
			}
			record := iamRolePolicyRecord{
				PolicyDocument: string(policyJSON),
				PolicyName:     policyName,
				RoleName:       roleName,
			}
			payload, err := json.Marshal(record)
			if err != nil {
				return nil, internal(err)
			}
			if err := s.metadata.Put(iamRolePoliciesBucket, roleName+"|"+policyName, payload); err != nil {
				return nil, internal(err)
			}
		}
	}

	return []managedResource{{LogicalID: logicalID, Type: "AWS::IAM::Role", Name: roleName}}, nil
}

func (s *Service) dynamoTableRecord(tableName string, properties map[string]any, parameters map[string]string) (dynamoTableRecord, error) {
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
		name, err := resolveString(entry["AttributeName"], parameters)
		if err != nil {
			return dynamoTableRecord{}, err
		}
		attrType, err := resolveString(entry["AttributeType"], parameters)
		if err != nil {
			return dynamoTableRecord{}, err
		}
		attrDefs = append(attrDefs, dynamoAttributeDefinition{AttributeName: name, AttributeType: attrType})
	}

	keyEntry, ok := keySchemaRaw[0].(map[string]any)
	if !ok {
		return dynamoTableRecord{}, validationError("KeySchema entries must be objects")
	}
	hashKey, err := resolveString(keyEntry["AttributeName"], parameters)
	if err != nil {
		return dynamoTableRecord{}, err
	}
	keyType, err := resolveString(keyEntry["KeyType"], parameters)
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

	billingMode, err := resolveOptionalString(properties["BillingMode"], parameters, "PAY_PER_REQUEST")
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

func stackToXML(record stackRecord) stackXML {
	params := make([]templateParameterXML, 0, len(record.Parameters))
	for _, param := range record.Parameters {
		params = append(params, templateParameterXML{
			DefaultValue:   param.DefaultValue,
			Description:    param.Description,
			NoEcho:         param.NoEcho,
			ParameterKey:   param.ParameterKey,
			ParameterValue: param.ParameterValue,
		})
	}

	return stackXML{
		Capabilities:        record.Capabilities,
		ClientRequestToken:  record.ClientRequestToken,
		CreationTime:        record.CreationTime.UTC().Format(time.RFC3339),
		Description:         record.Description,
		DisableRollback:     record.DisableRollback,
		Outputs:             []outputXML{},
		Parameters:          params,
		StackID:             record.StackID,
		StackName:           record.StackName,
		StackStatus:         record.StackStatus,
		TemplateDescription: record.TemplateDescription,
		TimeoutInMinutes:    record.TimeoutInMinutes,
	}
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
