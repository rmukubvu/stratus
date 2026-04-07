package iam

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math/rand"
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
	namespace          = "https://iam.amazonaws.com/doc/2010-05-08/"
	accountID          = "000000000000"
	rolesBucket        = "iam-roles"
	rolePoliciesBucket = "iam-role-policies"
)

var (
	roleNameRe   = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)
	policyNameRe = regexp.MustCompile(`^[\w+=,.@-]{1,128}$`)
	pathRe       = regexp.MustCompile(`^/([\x21-\x7E]*/)?$`)
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	rand     *rand.Rand
	mu       sync.Mutex
}

type roleRecord struct {
	AssumeRolePolicyDocument string    `json:"assume_role_policy_document"`
	CreateDate               time.Time `json:"create_date"`
	Description              string    `json:"description,omitempty"`
	MaxSessionDuration       int       `json:"max_session_duration"`
	Path                     string    `json:"path"`
	RoleID                   string    `json:"role_id"`
	RoleName                 string    `json:"role_name"`
}

type rolePolicyRecord struct {
	PolicyDocument string `json:"policy_document"`
	PolicyName     string `json:"policy_name"`
	RoleName       string `json:"role_name"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

type emptyResponse struct {
	XMLName          xml.Name         `xml:"DeleteRoleResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type createRoleResponse struct {
	XMLName          xml.Name         `xml:"CreateRoleResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           roleResult       `xml:"CreateRoleResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type getRoleResponse struct {
	XMLName          xml.Name         `xml:"GetRoleResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           roleResult       `xml:"GetRoleResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type listRolesResponse struct {
	XMLName          xml.Name         `xml:"ListRolesResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	Result           listRolesResult  `xml:"ListRolesResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type putRolePolicyResponse struct {
	XMLName          xml.Name         `xml:"PutRolePolicyResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type getRolePolicyResponse struct {
	XMLName          xml.Name            `xml:"GetRolePolicyResponse"`
	XMLNS            string              `xml:"xmlns,attr"`
	Result           getRolePolicyResult `xml:"GetRolePolicyResult"`
	ResponseMetadata responseMetadata    `xml:"ResponseMetadata"`
}

type listRolePoliciesResponse struct {
	XMLName          xml.Name               `xml:"ListRolePoliciesResponse"`
	XMLNS            string                 `xml:"xmlns,attr"`
	Result           listRolePoliciesResult `xml:"ListRolePoliciesResult"`
	ResponseMetadata responseMetadata       `xml:"ResponseMetadata"`
}

type deleteRolePolicyResponse struct {
	XMLName          xml.Name         `xml:"DeleteRolePolicyResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type roleResult struct {
	Role roleXML `xml:"Role"`
}

type listRolesResult struct {
	IsTruncated bool      `xml:"IsTruncated"`
	Roles       []roleXML `xml:"Roles>member"`
}

type getRolePolicyResult struct {
	PolicyDocument string `xml:"PolicyDocument"`
	PolicyName     string `xml:"PolicyName"`
	RoleName       string `xml:"RoleName"`
}

type listRolePoliciesResult struct {
	IsTruncated bool     `xml:"IsTruncated"`
	PolicyNames []string `xml:"PolicyNames>member"`
}

type roleXML struct {
	Path                     string `xml:"Path"`
	Arn                      string `xml:"Arn"`
	RoleName                 string `xml:"RoleName"`
	AssumeRolePolicyDocument string `xml:"AssumeRolePolicyDocument"`
	CreateDate               string `xml:"CreateDate"`
	RoleID                   string `xml:"RoleId"`
	Description              string `xml:"Description,omitempty"`
	MaxSessionDuration       int    `xml:"MaxSessionDuration,omitempty"`
}

func NewService(metadata store.Store) *Service {
	return &Service{
		metadata: metadata,
		now:      time.Now,
		rand:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateRole":
		return s.createRole(w, r, requestID)
	case "GetRole":
		return s.getRole(w, r, requestID)
	case "ListRoles":
		return s.listRoles(w, r, requestID)
	case "DeleteRole":
		return s.deleteRole(w, r, requestID)
	case "PutRolePolicy":
		return s.putRolePolicy(w, r, requestID)
	case "GetRolePolicy":
		return s.getRolePolicy(w, r, requestID)
	case "ListRolePolicies":
		return s.listRolePolicies(w, r, requestID)
	case "DeleteRolePolicy":
		return s.deleteRolePolicy(w, r, requestID)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplemented",
			Message:    "iam operation is not implemented",
		}
	}
}

func (s *Service) createRole(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	roleName := form.Get("RoleName")
	if err := validateRoleName(roleName); err != nil {
		return err
	}
	if raw := form.Get("PermissionsBoundary"); raw != "" {
		return notImplemented("permissions boundaries are not implemented")
	}
	if hasIndexedPrefix(form, "Tags.member.") {
		return notImplemented("role tags are not implemented")
	}

	doc, err := normalizePolicyDocument(form.Get("AssumeRolePolicyDocument"))
	if err != nil {
		return err
	}
	path, err := normalizePath(form.Get("Path"))
	if err != nil {
		return err
	}
	maxSessionDuration, err := normalizeMaxSessionDuration(form.Get("MaxSessionDuration"))
	if err != nil {
		return err
	}

	if _, err := s.loadRole(roleName); err == nil {
		return &apierror.Error{
			StatusCode: http.StatusConflict,
			Code:       "EntityAlreadyExists",
			Message:    fmt.Sprintf("Role with name %s already exists.", roleName),
		}
	}

	record := roleRecord{
		AssumeRolePolicyDocument: doc,
		CreateDate:               s.now().UTC(),
		Description:              form.Get("Description"),
		MaxSessionDuration:       maxSessionDuration,
		Path:                     path,
		RoleID:                   s.newRoleID(),
		RoleName:                 roleName,
	}
	if err := s.putRole(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, createRoleResponse{
		XMLNS: namespace,
		Result: roleResult{
			Role: roleToXML(record),
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) getRole(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}

	writeXML(w, http.StatusOK, getRoleResponse{
		XMLNS: namespace,
		Result: roleResult{
			Role: roleToXML(role),
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) listRoles(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	pathPrefix := form.Get("PathPrefix")
	if pathPrefix == "" {
		pathPrefix = "/"
	}

	var roles []roleXML
	if err := s.metadata.Scan(rolesBucket, "", func(_, v []byte) error {
		var record roleRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if strings.HasPrefix(record.Path, pathPrefix) {
			roles = append(roles, roleToXML(record))
		}
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(roles, func(i, j int) bool {
		return roles[i].RoleName < roles[j].RoleName
	})

	writeXML(w, http.StatusOK, listRolesResponse{
		XMLNS: namespace,
		Result: listRolesResult{
			IsTruncated: false,
			Roles:       roles,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) deleteRole(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}

	policyNames, err := s.rolePolicyNames(role.RoleName)
	if err != nil {
		return err
	}
	if len(policyNames) > 0 {
		return &apierror.Error{
			StatusCode: http.StatusConflict,
			Code:       "DeleteConflict",
			Message:    "Cannot delete entity, must delete policies first.",
		}
	}

	if err := s.metadata.Delete(rolesBucket, role.RoleName); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, emptyResponse{
		XMLName:          xml.Name{Local: "DeleteRoleResponse"},
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) putRolePolicy(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}
	policyName := form.Get("PolicyName")
	if err := validatePolicyName(policyName); err != nil {
		return err
	}
	doc, err := normalizePolicyDocument(form.Get("PolicyDocument"))
	if err != nil {
		return err
	}

	record := rolePolicyRecord{
		PolicyDocument: doc,
		PolicyName:     policyName,
		RoleName:       role.RoleName,
	}
	if err := s.putRolePolicyRecord(record); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, putRolePolicyResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) getRolePolicy(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}
	policyName := form.Get("PolicyName")
	if err := validatePolicyName(policyName); err != nil {
		return err
	}
	record, err := s.loadRolePolicy(role.RoleName, policyName)
	if err != nil {
		return err
	}

	writeXML(w, http.StatusOK, getRolePolicyResponse{
		XMLNS: namespace,
		Result: getRolePolicyResult{
			PolicyDocument: encodePolicyDocument(record.PolicyDocument),
			PolicyName:     record.PolicyName,
			RoleName:       record.RoleName,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) listRolePolicies(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}
	policyNames, err := s.rolePolicyNames(role.RoleName)
	if err != nil {
		return err
	}

	writeXML(w, http.StatusOK, listRolePoliciesResponse{
		XMLNS: namespace,
		Result: listRolePoliciesResult{
			IsTruncated: false,
			PolicyNames: policyNames,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) deleteRolePolicy(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}
	policyName := form.Get("PolicyName")
	if err := validatePolicyName(policyName); err != nil {
		return err
	}
	if _, err := s.loadRolePolicy(role.RoleName, policyName); err != nil {
		return err
	}
	if err := s.metadata.Delete(rolePoliciesBucket, rolePolicyKey(role.RoleName, policyName)); err != nil {
		return internal(err)
	}

	writeXML(w, http.StatusOK, deleteRolePolicyResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) loadRoleByForm(form url.Values) (roleRecord, error) {
	roleName := form.Get("RoleName")
	if err := validateRoleName(roleName); err != nil {
		return roleRecord{}, err
	}
	return s.loadRole(roleName)
}

func (s *Service) loadRole(roleName string) (roleRecord, error) {
	raw, err := s.metadata.Get(rolesBucket, roleName)
	if err != nil {
		return roleRecord{}, internal(err)
	}
	if raw == nil {
		return roleRecord{}, &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "NoSuchEntity",
			Message:    fmt.Sprintf("The role with name %s cannot be found.", roleName),
		}
	}
	var record roleRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return roleRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putRole(record roleRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(rolesBucket, record.RoleName, raw)
}

func (s *Service) loadRolePolicy(roleName, policyName string) (rolePolicyRecord, error) {
	raw, err := s.metadata.Get(rolePoliciesBucket, rolePolicyKey(roleName, policyName))
	if err != nil {
		return rolePolicyRecord{}, internal(err)
	}
	if raw == nil {
		return rolePolicyRecord{}, &apierror.Error{
			StatusCode: http.StatusNotFound,
			Code:       "NoSuchEntity",
			Message:    fmt.Sprintf("The role policy with name %s cannot be found.", policyName),
		}
	}
	var record rolePolicyRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return rolePolicyRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putRolePolicyRecord(record rolePolicyRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(rolePoliciesBucket, rolePolicyKey(record.RoleName, record.PolicyName), raw)
}

func (s *Service) rolePolicyNames(roleName string) ([]string, error) {
	var names []string
	if err := s.metadata.Scan(rolePoliciesBucket, roleName+"|", func(_, v []byte) error {
		var record rolePolicyRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		names = append(names, record.PolicyName)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Strings(names)
	return names, nil
}

func roleToXML(record roleRecord) roleXML {
	return roleXML{
		Path:                     record.Path,
		Arn:                      roleARN(record.Path, record.RoleName),
		RoleName:                 record.RoleName,
		AssumeRolePolicyDocument: encodePolicyDocument(record.AssumeRolePolicyDocument),
		CreateDate:               record.CreateDate.UTC().Format(time.RFC3339),
		RoleID:                   record.RoleID,
		Description:              record.Description,
		MaxSessionDuration:       record.MaxSessionDuration,
	}
}

func roleARN(path, roleName string) string {
	if path == "/" {
		return fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, roleName)
	}
	return fmt.Sprintf("arn:aws:iam::%s:role%s%s", accountID, path, roleName)
}

func rolePolicyKey(roleName, policyName string) string {
	return roleName + "|" + policyName
}

func encodePolicyDocument(document string) string {
	encoded := url.QueryEscape(document)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	return encoded
}

func normalizePolicyDocument(document string) (string, error) {
	if document == "" {
		return "", badRequest("MalformedPolicyDocument", "Policy document must be provided.")
	}
	var value any
	if err := json.Unmarshal([]byte(document), &value); err != nil {
		return "", badRequest("MalformedPolicyDocument", "Policy document is not valid JSON.")
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return "", internal(err)
	}
	return string(normalized), nil
}

func normalizePath(path string) (string, error) {
	if path == "" {
		return "/", nil
	}
	if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/") || !pathRe.MatchString(path) {
		return "", badRequest("ValidationError", "Path must begin and end with /.")
	}
	return path, nil
}

func normalizeMaxSessionDuration(raw string) (int, error) {
	if raw == "" {
		return 3600, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 3600 || value > 43200 {
		return 0, badRequest("ValidationError", "MaxSessionDuration must be between 3600 and 43200.")
	}
	return value, nil
}

func validateRoleName(roleName string) error {
	if !roleNameRe.MatchString(roleName) {
		return badRequest("ValidationError", "RoleName must be 1 to 64 characters of alphanumeric or _+=,.@-.")
	}
	return nil
}

func validatePolicyName(policyName string) error {
	if !policyNameRe.MatchString(policyName) {
		return badRequest("ValidationError", "PolicyName must be 1 to 128 characters of alphanumeric or _+=,.@-.")
	}
	return nil
}

func (s *Service) newRoleID() string {
	return "AROA" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:16]
}

func parseForm(r *http.Request) (url.Values, error) {
	if err := r.ParseForm(); err != nil {
		return nil, badRequest("InvalidInput", "request body is not valid form data")
	}
	return r.Form, nil
}

func hasIndexedPrefix(form url.Values, prefix string) bool {
	for key := range form {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "ServiceFailure", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}
