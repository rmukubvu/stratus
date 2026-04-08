package cognitoidp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	userPoolsBucket   = "cognito-user-pools"
	userClientsBucket = "cognito-user-pool-clients"
	usersBucket       = "cognito-users"
	groupsBucket      = "cognito-groups"
	accountID         = "000000000000"
	region            = "us-east-1"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type CreateUserPoolInput struct {
	PoolName           string
	UsernameAttributes []string
}

type CreateUserPoolClientInput struct {
	ClientName        string
	ExplicitAuthFlows []string
	UserPoolID        string
}

type userPool struct {
	ARN                string    `json:"arn"`
	CreatedAt          time.Time `json:"created_at"`
	ID                 string    `json:"id"`
	LastModifiedAt     time.Time `json:"last_modified_at"`
	Name               string    `json:"name"`
	UsernameAttributes []string  `json:"username_attributes,omitempty"`
}

type userPoolClient struct {
	ClientID          string    `json:"client_id"`
	ClientName        string    `json:"client_name"`
	CreatedAt         time.Time `json:"created_at"`
	ExplicitAuthFlows []string  `json:"explicit_auth_flows,omitempty"`
	LastModifiedAt    time.Time `json:"last_modified_at"`
	PoolID            string    `json:"pool_id"`
}

type userRecord struct {
	Confirmed    bool      `json:"confirmed"`
	CreatedAt    time.Time `json:"created_at"`
	Email        string    `json:"email,omitempty"`
	TempPassword bool      `json:"temp_password,omitempty"`
	Password     string    `json:"password"`
	PoolID       string    `json:"pool_id"`
	Sub          string    `json:"sub"`
	Username     string    `json:"username"`
}

type groupRecord struct {
	CreatedAt   time.Time `json:"created_at"`
	Description string    `json:"description,omitempty"`
	GroupName   string    `json:"group_name"`
	PoolID      string    `json:"pool_id"`
	Users       []string  `json:"users,omitempty"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateUserPool":
		return s.createUserPool(w, r)
	case "ListUserPools":
		return s.listUserPools(w, r)
	case "CreateUserPoolClient":
		return s.createUserPoolClient(w, r)
	case "SignUp":
		return s.signUp(w, r)
	case "AdminConfirmSignUp":
		return s.adminConfirmSignUp(w, r)
	case "AdminCreateUser":
		return s.adminCreateUser(w, r)
	case "AdminSetUserPassword":
		return s.adminSetUserPassword(w, r)
	case "ListUsers":
		return s.listUsers(w, r)
	case "CreateGroup":
		return s.createGroup(w, r)
	case "ListGroups":
		return s.listGroups(w, r)
	case "AdminAddUserToGroup":
		return s.adminAddUserToGroup(w, r)
	case "AdminListGroupsForUser":
		return s.adminListGroupsForUser(w, r)
	case "InitiateAuth":
		return s.initiateAuth(w, r)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "cognito-idp operation is not implemented"}
	}
}

func (s *Service) createUserPool(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		PoolName           string   `json:"PoolName"`
		UsernameAttributes []string `json:"UsernameAttributes"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.PoolName == "" {
		return validation("PoolName is required")
	}
	id := fmt.Sprintf("%s_%s", region, uuid.NewString()[:8])
	now := s.now().UTC()
	record := userPool{
		ARN:                fmt.Sprintf("arn:aws:cognito-idp:%s:%s:userpool/%s", region, accountID, id),
		CreatedAt:          now,
		ID:                 id,
		LastModifiedAt:     now,
		Name:               input.PoolName,
		UsernameAttributes: append([]string(nil), input.UsernameAttributes...),
	}
	if err := s.putUserPool(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"UserPool": map[string]any{
			"Arn":                record.ARN,
			"CreationDate":       formatTime(record.CreatedAt),
			"Id":                 record.ID,
			"LastModifiedDate":   formatTime(record.LastModifiedAt),
			"Name":               record.Name,
			"UsernameAttributes": record.UsernameAttributes,
		},
	})
	return nil
}

func (s *Service) CreateUserPool(input CreateUserPoolInput) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if input.PoolName == "" {
		return "", "", validation("PoolName is required")
	}
	id := fmt.Sprintf("%s_%s", region, uuid.NewString()[:8])
	now := s.now().UTC()
	record := userPool{
		ARN:                fmt.Sprintf("arn:aws:cognito-idp:%s:%s:userpool/%s", region, accountID, id),
		CreatedAt:          now,
		ID:                 id,
		LastModifiedAt:     now,
		Name:               input.PoolName,
		UsernameAttributes: append([]string(nil), input.UsernameAttributes...),
	}
	if err := s.putUserPool(record); err != nil {
		return "", "", internal(err)
	}
	return record.ID, record.ARN, nil
}

func (s *Service) CreateUserPoolClient(input CreateUserPoolClientInput) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pool, err := s.loadUserPool(input.UserPoolID)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	record := userPoolClient{
		ClientID:          strings.ReplaceAll(uuid.NewString()[:12], "-", ""),
		ClientName:        input.ClientName,
		CreatedAt:         now,
		ExplicitAuthFlows: append([]string(nil), input.ExplicitAuthFlows...),
		LastModifiedAt:    now,
		PoolID:            pool.ID,
	}
	if err := s.putUserPoolClient(record); err != nil {
		return "", internal(err)
	}
	return record.ClientID, nil
}

func (s *Service) DeleteUserPool(poolID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadUserPool(poolID); err != nil {
		return err
	}
	if err := s.metadata.Delete(userPoolsBucket, poolID); err != nil {
		return internal(err)
	}
	if err := s.metadata.Scan(userClientsBucket, "", func(k, v []byte) error {
		var client userPoolClient
		if err := json.Unmarshal(v, &client); err != nil {
			return nil
		}
		if client.PoolID == poolID {
			if err := s.metadata.Delete(userClientsBucket, string(k)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return internal(err)
	}
	if err := s.metadata.DeletePrefix(usersBucket, poolID+"|"); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) DeleteUserPoolClient(clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.loadUserPoolClient(clientID); err != nil {
		return err
	}
	if err := s.metadata.Delete(userClientsBucket, clientID); err != nil {
		return internal(err)
	}
	return nil
}

func (s *Service) listUserPools(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		MaxResults int `json:"MaxResults"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	if err := s.metadata.Scan(userPoolsBucket, "", func(_, v []byte) error {
		var record userPool
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, map[string]any{
			"CreationDate":     formatTime(record.CreatedAt),
			"Id":               record.ID,
			"LastModifiedDate": formatTime(record.LastModifiedAt),
			"Name":             record.Name,
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["Name"].(string) < items[j]["Name"].(string) })
	if input.MaxResults > 0 && len(items) > input.MaxResults {
		items = items[:input.MaxResults]
	}
	writeJSON(w, http.StatusOK, map[string]any{"UserPools": items})
	return nil
}

func (s *Service) createUserPoolClient(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ClientName        string   `json:"ClientName"`
		ExplicitAuthFlows []string `json:"ExplicitAuthFlows"`
		UserPoolID        string   `json:"UserPoolId"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.ClientName == "" || input.UserPoolID == "" {
		return validation("ClientName and UserPoolId are required")
	}
	pool, err := s.loadUserPool(input.UserPoolID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	record := userPoolClient{
		ClientID:          strings.ReplaceAll(uuid.NewString()[:12], "-", ""),
		ClientName:        input.ClientName,
		CreatedAt:         now,
		ExplicitAuthFlows: append([]string(nil), input.ExplicitAuthFlows...),
		LastModifiedAt:    now,
		PoolID:            pool.ID,
	}
	if err := s.putUserPoolClient(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"UserPoolClient": map[string]any{
			"ClientId":          record.ClientID,
			"ClientName":        record.ClientName,
			"CreationDate":      formatTime(record.CreatedAt),
			"ExplicitAuthFlows": record.ExplicitAuthFlows,
			"LastModifiedDate":  formatTime(record.LastModifiedAt),
			"UserPoolId":        record.PoolID,
		},
	})
	return nil
}

func (s *Service) signUp(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ClientID string `json:"ClientId"`
		Password string `json:"Password"`
		Username string `json:"Username"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	client, err := s.loadUserPoolClient(input.ClientID)
	if err != nil {
		return err
	}
	if input.Username == "" || input.Password == "" {
		return validation("Username and Password are required")
	}
	if _, err := s.loadUser(client.PoolID, input.Username); err == nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "UsernameExistsException", Message: "User already exists"}
	}
	record := userRecord{
		Confirmed: false,
		CreatedAt: s.now().UTC(),
		Password:  input.Password,
		PoolID:    client.PoolID,
		Sub:       uuid.NewString(),
		Username:  input.Username,
	}
	if err := s.putUser(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"CodeDeliveryDetails": map[string]any{
			"AttributeName":  "email",
			"DeliveryMedium": "EMAIL",
			"Destination":    "local@example.com",
		},
		"UserConfirmed": false,
		"UserSub":       record.Sub,
	})
	return nil
}

func (s *Service) adminConfirmSignUp(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		UserPoolID string `json:"UserPoolId"`
		Username   string `json:"Username"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.loadUser(input.UserPoolID, input.Username)
	if err != nil {
		return err
	}
	record.Confirmed = true
	if err := s.putUser(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) adminCreateUser(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		MessageAction     string `json:"MessageAction"`
		TemporaryPassword string `json:"TemporaryPassword"`
		UserAttributes    []struct {
			Name  string `json:"Name"`
			Value string `json:"Value"`
		} `json:"UserAttributes"`
		UserPoolID string `json:"UserPoolId"`
		Username   string `json:"Username"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.MessageAction != "" && input.MessageAction != "SUPPRESS" {
		return notImplemented("only SUPPRESS message action is supported")
	}
	if _, err := s.loadUserPool(input.UserPoolID); err != nil {
		return err
	}
	if input.Username == "" {
		return validation("Username is required")
	}
	if _, err := s.loadUser(input.UserPoolID, input.Username); err == nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "UsernameExistsException", Message: "User already exists"}
	}
	email := ""
	for _, attr := range input.UserAttributes {
		if attr.Name == "email" {
			email = attr.Value
		}
	}
	record := userRecord{
		Confirmed:    false,
		CreatedAt:    s.now().UTC(),
		Email:        email,
		Password:     input.TemporaryPassword,
		PoolID:       input.UserPoolID,
		Sub:          uuid.NewString(),
		TempPassword: input.TemporaryPassword != "",
		Username:     input.Username,
	}
	if err := s.putUser(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"User": map[string]any{
			"Attributes":           []map[string]string{{"Name": "sub", "Value": record.Sub}},
			"Enabled":              true,
			"UserCreateDate":       formatTime(record.CreatedAt),
			"UserLastModifiedDate": formatTime(record.CreatedAt),
			"Username":             record.Username,
			"UserStatus":           ternary(record.Confirmed, "CONFIRMED", "FORCE_CHANGE_PASSWORD"),
		},
	})
	return nil
}

func (s *Service) adminSetUserPassword(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Password   string `json:"Password"`
		Permanent  bool   `json:"Permanent"`
		UserPoolID string `json:"UserPoolId"`
		Username   string `json:"Username"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.loadUser(input.UserPoolID, input.Username)
	if err != nil {
		return err
	}
	record.Password = input.Password
	record.TempPassword = !input.Permanent
	record.Confirmed = input.Permanent || record.Confirmed
	if err := s.putUser(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) listUsers(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		UserPoolID string `json:"UserPoolId"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	users := make([]map[string]any, 0)
	if err := s.metadata.Scan(usersBucket, input.UserPoolID+"|", func(_, v []byte) error {
		var record userRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		attrs := []map[string]string{{"Name": "sub", "Value": record.Sub}}
		if record.Email != "" {
			attrs = append(attrs, map[string]string{"Name": "email", "Value": record.Email})
		}
		users = append(users, map[string]any{
			"Attributes":           attrs,
			"Enabled":              true,
			"UserCreateDate":       formatTime(record.CreatedAt),
			"UserLastModifiedDate": formatTime(record.CreatedAt),
			"Username":             record.Username,
			"UserStatus":           ternary(record.Confirmed, "CONFIRMED", "FORCE_CHANGE_PASSWORD"),
		})
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(users, func(i, j int) bool { return users[i]["Username"].(string) < users[j]["Username"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"Users": users})
	return nil
}

func (s *Service) createGroup(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Description string `json:"Description"`
		GroupName   string `json:"GroupName"`
		UserPoolID  string `json:"UserPoolId"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if _, err := s.loadUserPool(input.UserPoolID); err != nil {
		return err
	}
	if input.GroupName == "" {
		return validation("GroupName is required")
	}
	record := groupRecord{
		CreatedAt:   s.now().UTC(),
		Description: input.Description,
		GroupName:   input.GroupName,
		PoolID:      input.UserPoolID,
	}
	if err := s.putGroup(record); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Group": groupResponse(record)})
	return nil
}

func (s *Service) listGroups(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		UserPoolID string `json:"UserPoolId"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	groups, err := s.loadGroups(input.UserPoolID)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		items = append(items, groupResponse(group))
	}
	writeJSON(w, http.StatusOK, map[string]any{"Groups": items})
	return nil
}

func (s *Service) adminAddUserToGroup(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		GroupName  string `json:"GroupName"`
		UserPoolID string `json:"UserPoolId"`
		Username   string `json:"Username"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	group, err := s.loadGroup(input.UserPoolID, input.GroupName)
	if err != nil {
		return err
	}
	if _, err := s.loadUser(input.UserPoolID, input.Username); err != nil {
		return err
	}
	if !containsString(group.Users, input.Username) {
		group.Users = append(group.Users, input.Username)
		sort.Strings(group.Users)
	}
	if err := s.putGroup(group); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) adminListGroupsForUser(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		UserPoolID string `json:"UserPoolId"`
		Username   string `json:"Username"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	groups, err := s.loadGroups(input.UserPoolID)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0)
	for _, group := range groups {
		if containsString(group.Users, input.Username) {
			items = append(items, groupResponse(group))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"Groups": items})
	return nil
}

func (s *Service) initiateAuth(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		AuthFlow       string            `json:"AuthFlow"`
		AuthParameters map[string]string `json:"AuthParameters"`
		ClientID       string            `json:"ClientId"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.AuthFlow != "USER_PASSWORD_AUTH" && input.AuthFlow != "USER_SRP_AUTH" {
		return notImplemented("only USER_PASSWORD_AUTH and USER_SRP_AUTH are supported")
	}
	client, err := s.loadUserPoolClient(input.ClientID)
	if err != nil {
		return err
	}
	username := input.AuthParameters["USERNAME"]
	password := input.AuthParameters["PASSWORD"]
	if username == "" {
		return validation("USERNAME is required")
	}
	user, err := s.loadUser(client.PoolID, username)
	if err != nil {
		return err
	}
	if !user.Confirmed {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "UserNotConfirmedException", Message: "User is not confirmed"}
	}
	if input.AuthFlow == "USER_PASSWORD_AUTH" && user.Password != password {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "NotAuthorizedException", Message: "Incorrect username or password."}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"AuthenticationResult": map[string]any{
			"AccessToken":  "access-" + uuid.NewString(),
			"ExpiresIn":    3600,
			"IdToken":      "id-" + user.Sub,
			"RefreshToken": "refresh-" + uuid.NewString(),
			"TokenType":    "Bearer",
		},
		"ChallengeParameters": map[string]string{},
	})
	return nil
}

func (s *Service) loadUserPool(id string) (userPool, error) {
	raw, err := s.metadata.Get(userPoolsBucket, id)
	if err != nil {
		return userPool{}, internal(err)
	}
	if raw == nil {
		return userPool{}, notFound("ResourceNotFoundException", "User pool not found")
	}
	var record userPool
	if err := json.Unmarshal(raw, &record); err != nil {
		return userPool{}, internal(err)
	}
	return record, nil
}

func (s *Service) putUserPool(record userPool) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(userPoolsBucket, record.ID, raw)
}

func (s *Service) loadUserPoolClient(clientID string) (userPoolClient, error) {
	raw, err := s.metadata.Get(userClientsBucket, clientID)
	if err != nil {
		return userPoolClient{}, internal(err)
	}
	if raw == nil {
		return userPoolClient{}, notFound("ResourceNotFoundException", "User pool client not found")
	}
	var record userPoolClient
	if err := json.Unmarshal(raw, &record); err != nil {
		return userPoolClient{}, internal(err)
	}
	return record, nil
}

func (s *Service) putUserPoolClient(record userPoolClient) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(userClientsBucket, record.ClientID, raw)
}

func (s *Service) loadUser(poolID, username string) (userRecord, error) {
	raw, err := s.metadata.Get(usersBucket, userKey(poolID, username))
	if err != nil {
		return userRecord{}, internal(err)
	}
	if raw == nil {
		return userRecord{}, notFound("UserNotFoundException", "User does not exist")
	}
	var record userRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return userRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putUser(record userRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(usersBucket, userKey(record.PoolID, record.Username), raw)
}

func (s *Service) loadGroup(poolID, groupName string) (groupRecord, error) {
	raw, err := s.metadata.Get(groupsBucket, poolID+"|"+groupName)
	if err != nil {
		return groupRecord{}, internal(err)
	}
	if raw == nil {
		return groupRecord{}, notFound("ResourceNotFoundException", "Group does not exist")
	}
	var record groupRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return groupRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadGroups(poolID string) ([]groupRecord, error) {
	var items []groupRecord
	if err := s.metadata.Scan(groupsBucket, poolID+"|", func(_, v []byte) error {
		var record groupRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].GroupName < items[j].GroupName })
	return items, nil
}

func (s *Service) putGroup(record groupRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.metadata.Put(groupsBucket, record.PoolID+"|"+record.GroupName, raw)
}

func groupResponse(record groupRecord) map[string]any {
	return map[string]any{
		"CreationDate": formatTime(record.CreatedAt),
		"Description":  record.Description,
		"GroupName":    record.GroupName,
		"UserPoolId":   record.PoolID,
	}
}

func userKey(poolID, username string) string {
	return poolID + "|" + username
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func decodeJSON(r *http.Request, out any) error {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterException", Message: message}
}

func notFound(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalErrorException", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func ternary(condition bool, left, right string) string {
	if condition {
		return left
	}
	return right
}
