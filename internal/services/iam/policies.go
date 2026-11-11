package iam

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	managedPoliciesBucket = "iam-policies"
	attachmentsBucket     = "iam-attachments"

	maxPolicyVersions = 5
)

type principalKind string

const (
	principalRole principalKind = "role"
	principalUser principalKind = "user"
)

type policyVersion struct {
	VersionID  string    `json:"version_id"`
	Document   string    `json:"document"`
	CreateDate time.Time `json:"create_date"`
}

type managedPolicyRecord struct {
	PolicyName       string          `json:"policy_name"`
	PolicyID         string          `json:"policy_id"`
	Path             string          `json:"path"`
	Description      string          `json:"description,omitempty"`
	DefaultVersionID string          `json:"default_version_id"`
	NextVersion      int             `json:"next_version"`
	Versions         []policyVersion `json:"versions"`
	CreateDate       time.Time       `json:"create_date"`
	UpdateDate       time.Time       `json:"update_date"`
}

func (p managedPolicyRecord) defaultDocument() string {
	for _, v := range p.Versions {
		if v.VersionID == p.DefaultVersionID {
			return v.Document
		}
	}
	return ""
}

type attachmentRecord struct {
	Kind       principalKind `json:"kind"`
	Principal  string        `json:"principal"`
	PolicyArn  string        `json:"policy_arn"`
	PolicyName string        `json:"policy_name"`
}

type policyXML struct {
	PolicyName       string `xml:"PolicyName"`
	PolicyID         string `xml:"PolicyId"`
	Arn              string `xml:"Arn"`
	Path             string `xml:"Path"`
	DefaultVersionID string `xml:"DefaultVersionId"`
	AttachmentCount  int    `xml:"AttachmentCount"`
	IsAttachable     bool   `xml:"IsAttachable"`
	Description      string `xml:"Description,omitempty"`
	CreateDate       string `xml:"CreateDate"`
	UpdateDate       string `xml:"UpdateDate"`
}

type policyVersionXML struct {
	Document         string `xml:"Document,omitempty"`
	VersionID        string `xml:"VersionId"`
	IsDefaultVersion bool   `xml:"IsDefaultVersion"`
	CreateDate       string `xml:"CreateDate"`
}

func (s *Service) handlePolicy(w http.ResponseWriter, form url.Values, operation, requestID string) (bool, error) {
	var err error
	switch operation {
	case "CreatePolicy":
		err = s.createPolicy(w, form, requestID)
	case "GetPolicy":
		err = s.getPolicy(w, form, requestID)
	case "ListPolicies":
		err = s.listPolicies(w, form, requestID)
	case "DeletePolicy":
		err = s.deletePolicy(w, form, requestID)
	case "CreatePolicyVersion":
		err = s.createPolicyVersion(w, form, requestID)
	case "GetPolicyVersion":
		err = s.getPolicyVersion(w, form, requestID)
	case "ListPolicyVersions":
		err = s.listPolicyVersions(w, form, requestID)
	case "DeletePolicyVersion":
		err = s.deletePolicyVersion(w, form, requestID)
	case "SetDefaultPolicyVersion":
		err = s.setDefaultPolicyVersion(w, form, requestID)
	case "AttachRolePolicy":
		err = s.attach(w, form, requestID, operation, principalRole)
	case "DetachRolePolicy":
		err = s.detach(w, form, requestID, operation, principalRole)
	case "AttachUserPolicy":
		err = s.attach(w, form, requestID, operation, principalUser)
	case "DetachUserPolicy":
		err = s.detach(w, form, requestID, operation, principalUser)
	case "ListAttachedRolePolicies":
		err = s.listAttached(w, form, requestID, operation, principalRole)
	case "ListAttachedUserPolicies":
		err = s.listAttached(w, form, requestID, operation, principalUser)
	default:
		return false, nil
	}
	return true, err
}

func (s *Service) createPolicy(w http.ResponseWriter, form url.Values, requestID string) error {
	name := form.Get("PolicyName")
	if err := validatePolicyName(name); err != nil {
		return err
	}
	path, err := normalizePath(form.Get("Path"))
	if err != nil {
		return err
	}
	doc, err := normalizePolicyDocument(form.Get("PolicyDocument"))
	if err != nil {
		return err
	}
	if found, err := exists(s.metadata, managedPoliciesBucket, name); err != nil {
		return err
	} else if found {
		return alreadyExists("A policy called " + name)
	}
	now := s.now().UTC()
	policy := managedPolicyRecord{
		PolicyName:       name,
		PolicyID:         newEntityID("ANPA"),
		Path:             path,
		Description:      form.Get("Description"),
		DefaultVersionID: "v1",
		NextVersion:      2,
		Versions:         []policyVersion{{VersionID: "v1", Document: doc, CreateDate: now}},
		CreateDate:       now,
		UpdateDate:       now,
	}
	if err := putRecord(s.metadata, managedPoliciesBucket, name, policy); err != nil {
		return err
	}
	return respond(w, "CreatePolicy", requestID, struct {
		Policy policyXML `xml:"Policy"`
	}{s.policyToXML(policy, 0)})
}

func (s *Service) getPolicy(w http.ResponseWriter, form url.Values, requestID string) error {
	arn := form.Get("PolicyArn")
	if name, ok := awsManagedName(arn); ok {
		return respond(w, "GetPolicy", requestID, struct {
			Policy policyXML `xml:"Policy"`
		}{awsManagedPolicyXML(arn, name)})
	}
	policy, err := s.loadPolicy(arn)
	if err != nil {
		return err
	}
	count, err := s.attachmentCount(arn)
	if err != nil {
		return err
	}
	return respond(w, "GetPolicy", requestID, struct {
		Policy policyXML `xml:"Policy"`
	}{s.policyToXML(policy, count)})
}

func (s *Service) listPolicies(w http.ResponseWriter, form url.Values, requestID string) error {
	scopeFilter := defaultString(form.Get("Scope"), "All")
	onlyAttached := form.Get("OnlyAttached") == "true"
	prefix := defaultString(form.Get("PathPrefix"), "/")
	out := []policyXML{}
	if scopeFilter != "AWS" {
		policies, err := scanRecords[managedPolicyRecord](s.metadata, managedPoliciesBucket, "")
		if err != nil {
			return err
		}
		for _, p := range sortedBy(policies, func(p managedPolicyRecord) string { return p.PolicyName }) {
			count, err := s.attachmentCount(s.policyARN(p))
			if err != nil {
				return err
			}
			if strings.HasPrefix(p.Path, prefix) && (!onlyAttached || count > 0) {
				out = append(out, s.policyToXML(p, count))
			}
		}
	}
	if scopeFilter != "Local" && !onlyAttached && prefix == "/" {
		names := make([]string, 0, len(awsManagedPolicies))
		for name := range awsManagedPolicies {
			names = append(names, name)
		}
		for _, name := range sortedBy(names, func(n string) string { return n }) {
			out = append(out, awsManagedPolicyXML(awsManagedPrefix+name, name))
		}
	}
	return respond(w, "ListPolicies", requestID, struct {
		Policies    []policyXML `xml:"Policies>member"`
		IsTruncated bool        `xml:"IsTruncated"`
	}{Policies: out})
}

func (s *Service) deletePolicy(w http.ResponseWriter, form url.Values, requestID string) error {
	arn := form.Get("PolicyArn")
	policy, err := s.loadPolicy(arn)
	if err != nil {
		return err
	}
	if count, err := s.attachmentCount(arn); err != nil {
		return err
	} else if count > 0 {
		return deleteConflict("Cannot delete a policy attached to entities.")
	}
	if err := s.metadata.Delete(managedPoliciesBucket, policy.PolicyName); err != nil {
		return internal(err)
	}
	return respond(w, "DeletePolicy", requestID, nil)
}

func (s *Service) createPolicyVersion(w http.ResponseWriter, form url.Values, requestID string) error {
	policy, err := s.loadPolicy(form.Get("PolicyArn"))
	if err != nil {
		return err
	}
	doc, err := normalizePolicyDocument(form.Get("PolicyDocument"))
	if err != nil {
		return err
	}
	if len(policy.Versions) >= maxPolicyVersions {
		return badRequest("LimitExceeded", "A managed policy can have up to 5 versions. Delete a version before creating a new one.")
	}
	now := s.now().UTC()
	version := policyVersion{VersionID: "v" + strconv.Itoa(policy.NextVersion), Document: doc, CreateDate: now}
	policy.NextVersion++
	policy.Versions = append(policy.Versions, version)
	setDefault := form.Get("SetAsDefault") == "true"
	if setDefault {
		policy.DefaultVersionID = version.VersionID
	}
	policy.UpdateDate = now
	if err := putRecord(s.metadata, managedPoliciesBucket, policy.PolicyName, policy); err != nil {
		return err
	}
	return respond(w, "CreatePolicyVersion", requestID, struct {
		PolicyVersion policyVersionXML `xml:"PolicyVersion"`
	}{policyVersionXML{VersionID: version.VersionID, IsDefaultVersion: setDefault, CreateDate: now.Format(time.RFC3339)}})
}

func (s *Service) getPolicyVersion(w http.ResponseWriter, form url.Values, requestID string) error {
	arn := form.Get("PolicyArn")
	versionID := form.Get("VersionId")
	if doc, ok := awsManagedDocument(arn); ok {
		return respond(w, "GetPolicyVersion", requestID, struct {
			PolicyVersion policyVersionXML `xml:"PolicyVersion"`
		}{policyVersionXML{Document: encodePolicyDocument(doc), VersionID: "v1", IsDefaultVersion: true, CreateDate: awsManagedDate}})
	}
	policy, err := s.loadPolicy(arn)
	if err != nil {
		return err
	}
	for _, v := range policy.Versions {
		if v.VersionID == versionID {
			return respond(w, "GetPolicyVersion", requestID, struct {
				PolicyVersion policyVersionXML `xml:"PolicyVersion"`
			}{policyVersionXML{Document: encodePolicyDocument(v.Document), VersionID: v.VersionID, IsDefaultVersion: v.VersionID == policy.DefaultVersionID, CreateDate: v.CreateDate.Format(time.RFC3339)}})
		}
	}
	return noSuchEntity("policy version " + versionID)
}

func (s *Service) listPolicyVersions(w http.ResponseWriter, form url.Values, requestID string) error {
	policy, err := s.loadPolicy(form.Get("PolicyArn"))
	if err != nil {
		return err
	}
	out := make([]policyVersionXML, 0, len(policy.Versions))
	for i := len(policy.Versions) - 1; i >= 0; i-- {
		v := policy.Versions[i]
		out = append(out, policyVersionXML{VersionID: v.VersionID, IsDefaultVersion: v.VersionID == policy.DefaultVersionID, CreateDate: v.CreateDate.Format(time.RFC3339)})
	}
	return respond(w, "ListPolicyVersions", requestID, struct {
		Versions    []policyVersionXML `xml:"Versions>member"`
		IsTruncated bool               `xml:"IsTruncated"`
	}{Versions: out})
}

func (s *Service) deletePolicyVersion(w http.ResponseWriter, form url.Values, requestID string) error {
	policy, err := s.loadPolicy(form.Get("PolicyArn"))
	if err != nil {
		return err
	}
	versionID := form.Get("VersionId")
	if versionID == policy.DefaultVersionID {
		return deleteConflict("Cannot delete the default version of a policy.")
	}
	kept := policy.Versions[:0]
	for _, v := range policy.Versions {
		if v.VersionID != versionID {
			kept = append(kept, v)
		}
	}
	if len(kept) == len(policy.Versions) {
		return noSuchEntity("policy version " + versionID)
	}
	policy.Versions = kept
	if err := putRecord(s.metadata, managedPoliciesBucket, policy.PolicyName, policy); err != nil {
		return err
	}
	return respond(w, "DeletePolicyVersion", requestID, nil)
}

func (s *Service) setDefaultPolicyVersion(w http.ResponseWriter, form url.Values, requestID string) error {
	policy, err := s.loadPolicy(form.Get("PolicyArn"))
	if err != nil {
		return err
	}
	versionID := form.Get("VersionId")
	for _, v := range policy.Versions {
		if v.VersionID == versionID {
			policy.DefaultVersionID = versionID
			policy.UpdateDate = s.now().UTC()
			if err := putRecord(s.metadata, managedPoliciesBucket, policy.PolicyName, policy); err != nil {
				return err
			}
			return respond(w, "SetDefaultPolicyVersion", requestID, nil)
		}
	}
	return noSuchEntity("policy version " + versionID)
}

func (s *Service) attach(w http.ResponseWriter, form url.Values, requestID, operation string, kind principalKind) error {
	principal, err := s.principalName(form, kind)
	if err != nil {
		return err
	}
	arn := form.Get("PolicyArn")
	policyName, err := s.attachablePolicyName(arn)
	if err != nil {
		return err
	}
	record := attachmentRecord{Kind: kind, Principal: principal, PolicyArn: arn, PolicyName: policyName}
	if err := putRecord(s.metadata, attachmentsBucket, attachmentKey(kind, principal, arn), record); err != nil {
		return err
	}
	return respond(w, operation, requestID, nil)
}

func (s *Service) detach(w http.ResponseWriter, form url.Values, requestID, operation string, kind principalKind) error {
	principal, err := s.principalName(form, kind)
	if err != nil {
		return err
	}
	key := attachmentKey(kind, principal, form.Get("PolicyArn"))
	if found, err := exists(s.metadata, attachmentsBucket, key); err != nil {
		return err
	} else if !found {
		return noSuchEntity("policy " + form.Get("PolicyArn") + " attached to " + string(kind) + " " + principal)
	}
	if err := s.metadata.Delete(attachmentsBucket, key); err != nil {
		return internal(err)
	}
	return respond(w, operation, requestID, nil)
}

func (s *Service) listAttached(w http.ResponseWriter, form url.Values, requestID, operation string, kind principalKind) error {
	principal, err := s.principalName(form, kind)
	if err != nil {
		return err
	}
	attached, err := s.attachedTo(kind, principal)
	if err != nil {
		return err
	}
	out := make([]attachedPolicyXML, 0, len(attached))
	for _, a := range attached {
		out = append(out, attachedPolicyXML{PolicyArn: a.PolicyArn, PolicyName: a.PolicyName})
	}
	return respond(w, operation, requestID, struct {
		AttachedPolicies []attachedPolicyXML `xml:"AttachedPolicies>member"`
		IsTruncated      bool                `xml:"IsTruncated"`
	}{AttachedPolicies: out})
}

// principalName checks the role or user a request names exists.
func (s *Service) principalName(form url.Values, kind principalKind) (string, error) {
	if kind == principalRole {
		role, err := s.loadRoleByForm(form)
		return role.RoleName, err
	}
	user, err := s.loadUser(form.Get("UserName"))
	return user.UserName, err
}

// attachablePolicyName resolves the name of a policy that can be attached:
// one of this account's managed policies, or any AWS managed policy.
func (s *Service) attachablePolicyName(arn string) (string, error) {
	if name, ok := awsManagedName(arn); ok {
		return name, nil
	}
	policy, err := s.loadPolicy(arn)
	return policy.PolicyName, err
}

func (s *Service) attachedTo(kind principalKind, principal string) ([]attachmentRecord, error) {
	records, err := scanRecords[attachmentRecord](s.metadata, attachmentsBucket, string(kind)+"|"+principal+"|")
	if err != nil {
		return nil, err
	}
	return sortedBy(records, func(a attachmentRecord) string { return a.PolicyName }), nil
}

func (s *Service) attachmentCount(arn string) (int, error) {
	records, err := scanRecords[attachmentRecord](s.metadata, attachmentsBucket, "")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, a := range records {
		if a.PolicyArn == arn {
			count++
		}
	}
	return count, nil
}

// loadPolicy finds one of this account's managed policies by ARN.
func (s *Service) loadPolicy(arn string) (managedPolicyRecord, error) {
	prefix := "arn:aws:iam::" + s.scope.Account + ":policy/"
	if !strings.HasPrefix(arn, prefix) {
		return managedPolicyRecord{}, noSuchEntity("policy " + arn)
	}
	name := arn[strings.LastIndex(arn, "/")+1:]
	policy, err := getRecord[managedPolicyRecord](s.metadata, managedPoliciesBucket, name, "policy "+arn)
	if err != nil {
		return managedPolicyRecord{}, err
	}
	if s.policyARN(policy) != arn {
		return managedPolicyRecord{}, noSuchEntity("policy " + arn)
	}
	return policy, nil
}

func (s *Service) policyARN(p managedPolicyRecord) string {
	return entityARN(s.scope.Account, "policy", p.Path, p.PolicyName)
}

func (s *Service) policyToXML(p managedPolicyRecord, attachments int) policyXML {
	return policyXML{
		PolicyName:       p.PolicyName,
		PolicyID:         p.PolicyID,
		Arn:              s.policyARN(p),
		Path:             p.Path,
		DefaultVersionID: p.DefaultVersionID,
		AttachmentCount:  attachments,
		IsAttachable:     true,
		Description:      p.Description,
		CreateDate:       p.CreateDate.Format(time.RFC3339),
		UpdateDate:       p.UpdateDate.Format(time.RFC3339),
	}
}

const awsManagedDate = "2015-02-06T18:39:46Z"

func awsManagedPolicyXML(arn, name string) policyXML {
	path := "/" + strings.TrimSuffix(strings.TrimPrefix(arn, awsManagedPrefix), name)
	return policyXML{
		PolicyName:       name,
		PolicyID:         awsManagedPolicyID(name),
		Arn:              arn,
		Path:             path,
		DefaultVersionID: "v1",
		IsAttachable:     true,
		CreateDate:       awsManagedDate,
		UpdateDate:       awsManagedDate,
	}
}

func attachmentKey(kind principalKind, principal, arn string) string {
	return string(kind) + "|" + principal + "|" + arn
}

// awsManagedPolicyID derives a stable policy ID from an AWS managed policy's
// name.
func awsManagedPolicyID(name string) string {
	id := strings.ToUpper(strings.NewReplacer("_", "", "-", "").Replace(name))
	return "ANPA" + id[:min(17, len(id))]
}
