package iam

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/tags"
)

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// tagSetXML is a pointer in the responses so that no tags leaves the Tags
// element out entirely, as AWS does.
type tagSetXML struct {
	Members []tagXML `xml:"member"`
}

type listRoleTagsResponse struct {
	XMLName          xml.Name           `xml:"ListRoleTagsResponse"`
	XMLNS            string             `xml:"xmlns,attr"`
	Result           listRoleTagsResult `xml:"ListRoleTagsResult"`
	ResponseMetadata responseMetadata   `xml:"ResponseMetadata"`
}

type listRoleTagsResult struct {
	IsTruncated bool      `xml:"IsTruncated"`
	Tags        tagSetXML `xml:"Tags"`
}

func tagsToXML(list tags.List) tagSetXML {
	out := tagSetXML{Members: make([]tagXML, 0, len(list))}
	for _, t := range list {
		out.Members = append(out.Members, tagXML{Key: t.Key, Value: t.Value})
	}
	return out
}

// optionalTagsXML is nil for no tags, so a resource's Tags element is left out.
func optionalTagsXML(list tags.List) *tagSetXML {
	if len(list) == 0 {
		return nil
	}
	set := tagsToXML(list)
	return &set
}

// roleTagsFromForm reads and validates the Tags.member.N list IAM takes on
// CreateRole and TagRole.
func roleTagsFromForm(form url.Values) (tags.List, error) {
	list, err := tags.FromQuery(form, "Tags.member.")
	if err != nil {
		return nil, tagError(err)
	}
	if err := list.Validate(tags.MaxPerResource); err != nil {
		return nil, tagError(err)
	}
	return list, nil
}

// tagError maps a tag validation failure to IAM's error codes.
func tagError(err error) error {
	var verr *tags.ValidationError
	if !errors.As(err, &verr) {
		return internal(err)
	}
	if verr.Reason == tags.TooMany {
		return &apierror.Error{StatusCode: http.StatusConflict, Code: "LimitExceeded", Message: verr.Error()}
	}
	return badRequest("InvalidInput", verr.Error())
}

func (s *Service) tagRole(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}
	add, err := roleTagsFromForm(form)
	if err != nil {
		return err
	}
	merged := role.Tags.Merge(add)
	if err := merged.Validate(tags.MaxPerResource); err != nil {
		return tagError(err)
	}
	role.Tags = merged
	if err := s.putRole(role); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, emptyResponse{
		XMLName:          xml.Name{Local: "TagRoleResponse"},
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) untagRole(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}
	role.Tags = role.Tags.Without(tags.KeysFromQuery(form, "TagKeys.member."))
	if err := s.putRole(role); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, emptyResponse{
		XMLName:          xml.Name{Local: "UntagRoleResponse"},
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

func (s *Service) listRoleTags(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	role, err := s.loadRoleByForm(form)
	if err != nil {
		return err
	}
	writeXML(w, http.StatusOK, listRoleTagsResponse{
		XMLNS:            namespace,
		Result:           listRoleTagsResult{Tags: tagsToXML(role.Tags)},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}
