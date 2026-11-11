package iam

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	usersBucket        = "iam-users"
	userPoliciesBucket = "iam-user-policies"
)

type userRecord struct {
	UserName   string    `json:"user_name"`
	UserID     string    `json:"user_id"`
	Path       string    `json:"path"`
	CreateDate time.Time `json:"create_date"`
}

type inlinePolicyRecord struct {
	Owner          string `json:"owner"`
	PolicyName     string `json:"policy_name"`
	PolicyDocument string `json:"policy_document"`
}

type userXML struct {
	Path       string `xml:"Path"`
	UserName   string `xml:"UserName"`
	UserID     string `xml:"UserId"`
	Arn        string `xml:"Arn"`
	CreateDate string `xml:"CreateDate"`
}

type accessKeyXML struct {
	UserName        string `xml:"UserName"`
	AccessKeyID     string `xml:"AccessKeyId"`
	Status          string `xml:"Status"`
	SecretAccessKey string `xml:"SecretAccessKey,omitempty"`
	CreateDate      string `xml:"CreateDate"`
}

func (s *Service) handleUser(w http.ResponseWriter, form url.Values, operation, requestID string) (bool, error) {
	var err error
	switch operation {
	case "CreateUser":
		err = s.createUser(w, form, requestID)
	case "GetUser":
		err = s.getUser(w, form, requestID)
	case "ListUsers":
		err = s.listUsers(w, form, requestID)
	case "DeleteUser":
		err = s.deleteUser(w, form, requestID)
	case "CreateAccessKey":
		err = s.createAccessKey(w, form, requestID)
	case "ListAccessKeys":
		err = s.listAccessKeys(w, form, requestID)
	case "UpdateAccessKey":
		err = s.updateAccessKey(w, form, requestID)
	case "DeleteAccessKey":
		err = s.deleteAccessKey(w, form, requestID)
	case "PutUserPolicy":
		err = s.putUserPolicy(w, form, requestID)
	case "GetUserPolicy":
		err = s.getUserPolicy(w, form, requestID)
	case "ListUserPolicies":
		err = s.listUserPolicies(w, form, requestID)
	case "DeleteUserPolicy":
		err = s.deleteUserPolicy(w, form, requestID)
	default:
		return false, nil
	}
	return true, err
}

func (s *Service) createUser(w http.ResponseWriter, form url.Values, requestID string) error {
	name := form.Get("UserName")
	if err := validateEntityName("UserName", name); err != nil {
		return err
	}
	path, err := normalizePath(form.Get("Path"))
	if err != nil {
		return err
	}
	if found, err := exists(s.metadata, usersBucket, name); err != nil {
		return err
	} else if found {
		return alreadyExists("User with name " + name)
	}
	user := userRecord{UserName: name, UserID: newEntityID("AIDA"), Path: path, CreateDate: s.now().UTC()}
	if err := putRecord(s.metadata, usersBucket, name, user); err != nil {
		return err
	}
	return respond(w, "CreateUser", requestID, struct {
		User userXML `xml:"User"`
	}{s.userToXML(user)})
}

func (s *Service) getUser(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	return respond(w, "GetUser", requestID, struct {
		User userXML `xml:"User"`
	}{s.userToXML(user)})
}

func (s *Service) listUsers(w http.ResponseWriter, form url.Values, requestID string) error {
	users, err := scanRecords[userRecord](s.metadata, usersBucket, "")
	if err != nil {
		return err
	}
	prefix := defaultString(form.Get("PathPrefix"), "/")
	out := []userXML{}
	for _, u := range sortedBy(users, func(u userRecord) string { return u.UserName }) {
		if strings.HasPrefix(u.Path, prefix) {
			out = append(out, s.userToXML(u))
		}
	}
	return respond(w, "ListUsers", requestID, struct {
		Users       []userXML `xml:"Users>member"`
		IsTruncated bool      `xml:"IsTruncated"`
	}{Users: out})
}

func (s *Service) deleteUser(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	keys, err := s.keys.forUser(s.scope.Account, user.UserName)
	if err != nil {
		return err
	}
	inline, err := scanRecords[inlinePolicyRecord](s.metadata, userPoliciesBucket, user.UserName+"|")
	if err != nil {
		return err
	}
	attached, err := s.attachedTo(principalUser, user.UserName)
	if err != nil {
		return err
	}
	if len(keys) > 0 || len(inline) > 0 || len(attached) > 0 {
		return deleteConflict("Cannot delete entity, must delete access keys and policies first.")
	}
	if err := s.metadata.Delete(usersBucket, user.UserName); err != nil {
		return internal(err)
	}
	return respond(w, "DeleteUser", requestID, nil)
}

func (s *Service) createAccessKey(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	existing, err := s.keys.forUser(s.scope.Account, user.UserName)
	if err != nil {
		return err
	}
	if len(existing) >= 2 {
		return badRequest("LimitExceeded", "Cannot exceed quota for AccessKeysPerUser: 2")
	}
	key := AccessKey{
		AccessKeyID:     "AKIA" + randomAlnum(16),
		SecretAccessKey: randomSecret(),
		Account:         s.scope.Account,
		UserName:        user.UserName,
		UserARN:         entityARN(s.scope.Account, "user", user.Path, user.UserName),
		UserID:          user.UserID,
		Status:          "Active",
		CreateDate:      s.now().UTC(),
	}
	if err := s.keys.put(key); err != nil {
		return err
	}
	return respond(w, "CreateAccessKey", requestID, struct {
		AccessKey accessKeyXML `xml:"AccessKey"`
	}{accessKeyToXML(key, true)})
}

func (s *Service) listAccessKeys(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	keys, err := s.keys.forUser(s.scope.Account, user.UserName)
	if err != nil {
		return err
	}
	out := make([]accessKeyXML, 0, len(keys))
	for _, k := range keys {
		out = append(out, accessKeyToXML(k, false))
	}
	return respond(w, "ListAccessKeys", requestID, struct {
		AccessKeyMetadata []accessKeyXML `xml:"AccessKeyMetadata>member"`
		IsTruncated       bool           `xml:"IsTruncated"`
	}{AccessKeyMetadata: out})
}

func (s *Service) updateAccessKey(w http.ResponseWriter, form url.Values, requestID string) error {
	key, err := s.userKey(form)
	if err != nil {
		return err
	}
	status := form.Get("Status")
	if status != "Active" && status != "Inactive" {
		return badRequest("ValidationError", "Status must be Active or Inactive.")
	}
	key.Status = status
	if err := s.keys.put(key); err != nil {
		return err
	}
	return respond(w, "UpdateAccessKey", requestID, nil)
}

func (s *Service) deleteAccessKey(w http.ResponseWriter, form url.Values, requestID string) error {
	key, err := s.userKey(form)
	if err != nil {
		return err
	}
	if err := s.keys.delete(key.AccessKeyID); err != nil {
		return err
	}
	return respond(w, "DeleteAccessKey", requestID, nil)
}

// userKey finds the access key a request names, which must belong to the
// named user of this account.
func (s *Service) userKey(form url.Values) (AccessKey, error) {
	id, err := required(form, "AccessKeyId")
	if err != nil {
		return AccessKey{}, err
	}
	key, ok := s.keys.Lookup(id)
	if !ok || key.Account != s.scope.Account || (form.Get("UserName") != "" && key.UserName != form.Get("UserName")) {
		return AccessKey{}, noSuchEntity("access key " + id)
	}
	return key, nil
}

func (s *Service) putUserPolicy(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	name := form.Get("PolicyName")
	if err := validatePolicyName(name); err != nil {
		return err
	}
	doc, err := normalizePolicyDocument(form.Get("PolicyDocument"))
	if err != nil {
		return err
	}
	record := inlinePolicyRecord{Owner: user.UserName, PolicyName: name, PolicyDocument: doc}
	if err := putRecord(s.metadata, userPoliciesBucket, rolePolicyKey(user.UserName, name), record); err != nil {
		return err
	}
	return respond(w, "PutUserPolicy", requestID, nil)
}

func (s *Service) getUserPolicy(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	name := form.Get("PolicyName")
	record, err := getRecord[inlinePolicyRecord](s.metadata, userPoliciesBucket, rolePolicyKey(user.UserName, name), "user policy with name "+name)
	if err != nil {
		return err
	}
	return respond(w, "GetUserPolicy", requestID, struct {
		UserName       string `xml:"UserName"`
		PolicyName     string `xml:"PolicyName"`
		PolicyDocument string `xml:"PolicyDocument"`
	}{record.Owner, record.PolicyName, encodePolicyDocument(record.PolicyDocument)})
}

func (s *Service) listUserPolicies(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	records, err := scanRecords[inlinePolicyRecord](s.metadata, userPoliciesBucket, user.UserName+"|")
	if err != nil {
		return err
	}
	names := []string{}
	for _, r := range sortedBy(records, func(r inlinePolicyRecord) string { return r.PolicyName }) {
		names = append(names, r.PolicyName)
	}
	return respond(w, "ListUserPolicies", requestID, struct {
		PolicyNames []string `xml:"PolicyNames>member"`
		IsTruncated bool     `xml:"IsTruncated"`
	}{PolicyNames: names})
}

func (s *Service) deleteUserPolicy(w http.ResponseWriter, form url.Values, requestID string) error {
	user, err := s.loadUser(form.Get("UserName"))
	if err != nil {
		return err
	}
	name := form.Get("PolicyName")
	key := rolePolicyKey(user.UserName, name)
	if found, err := exists(s.metadata, userPoliciesBucket, key); err != nil {
		return err
	} else if !found {
		return noSuchEntity("user policy with name " + name)
	}
	if err := s.metadata.Delete(userPoliciesBucket, key); err != nil {
		return internal(err)
	}
	return respond(w, "DeleteUserPolicy", requestID, nil)
}

func (s *Service) loadUser(name string) (userRecord, error) {
	if err := validateEntityName("UserName", name); err != nil {
		return userRecord{}, err
	}
	return getRecord[userRecord](s.metadata, usersBucket, name, "user with name "+name)
}

func (s *Service) userToXML(u userRecord) userXML {
	return userXML{
		Path:       u.Path,
		UserName:   u.UserName,
		UserID:     u.UserID,
		Arn:        entityARN(s.scope.Account, "user", u.Path, u.UserName),
		CreateDate: u.CreateDate.Format(time.RFC3339),
	}
}

func accessKeyToXML(k AccessKey, withSecret bool) accessKeyXML {
	out := accessKeyXML{UserName: k.UserName, AccessKeyID: k.AccessKeyID, Status: k.Status, CreateDate: k.CreateDate.Format(time.RFC3339)}
	if withSecret {
		out.SecretAccessKey = k.SecretAccessKey
	}
	return out
}

func validateEntityName(field, name string) error {
	if !roleNameRe.MatchString(name) {
		return badRequest("ValidationError", field+" must be 1 to 64 characters of alphanumeric or _+=,.@-.")
	}
	return nil
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
