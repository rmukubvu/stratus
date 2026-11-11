package iam

import "encoding/json"

// IdentityPolicies returns the identity-based policy documents of a role or
// user: its inline policies and the default versions of its attached managed
// policies. AWS managed policies Stratus does not know grant nothing and are
// left out.
func (s *Service) IdentityPolicies(kind principalKind, name string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bucket := rolePoliciesBucket
	if kind == principalUser {
		bucket = userPoliciesBucket
	}
	var docs []string
	err := s.metadata.Scan(bucket, name+"|", func(_, v []byte) error {
		// Role and user inline policy records both carry policy_document.
		var record inlinePolicyRecord
		if err := json.Unmarshal(v, &record); err == nil && record.PolicyDocument != "" {
			docs = append(docs, record.PolicyDocument)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}

	attached, err := s.attachedTo(kind, name)
	if err != nil {
		return nil, err
	}
	for _, a := range attached {
		if doc, ok := awsManagedDocument(a.PolicyArn); ok {
			docs = append(docs, doc)
			continue
		}
		if _, ok := awsManagedName(a.PolicyArn); ok {
			continue
		}
		policy, err := s.loadPolicy(a.PolicyArn)
		if err != nil {
			continue // deleted underneath the attachment
		}
		docs = append(docs, policy.defaultDocument())
	}
	return docs, nil
}

// RolePolicies returns a role's identity-based policy documents.
func (s *Service) RolePolicies(roleName string) ([]string, error) {
	return s.IdentityPolicies(principalRole, roleName)
}

// UserPolicies returns a user's identity-based policy documents.
func (s *Service) UserPolicies(userName string) ([]string, error) {
	return s.IdentityPolicies(principalUser, userName)
}

// TrustPolicy returns a role's trust (assume-role) policy document.
func (s *Service) TrustPolicy(roleName string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := s.metadata.Get(rolesBucket, roleName)
	if err != nil {
		return "", false, internal(err)
	}
	if raw == nil {
		return "", false, nil
	}
	var role roleRecord
	if err := json.Unmarshal(raw, &role); err != nil {
		return "", false, internal(err)
	}
	return role.AssumeRolePolicyDocument, true, nil
}
