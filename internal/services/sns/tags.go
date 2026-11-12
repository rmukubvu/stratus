package sns

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

type listTagsForResourceResponse struct {
	XMLName          xml.Name                  `xml:"ListTagsForResourceResponse"`
	XMLNS            string                    `xml:"xmlns,attr"`
	Result           listTagsForResourceResult `xml:"ListTagsForResourceResult"`
	ResponseMetadata responseMetadata          `xml:"ResponseMetadata"`
}

type listTagsForResourceResult struct {
	Tags []tagXML `xml:"Tags>member"`
}

// topicTagsFromForm reads and validates the Tags.member.N list SNS takes on
// CreateTopic and TagResource.
func topicTagsFromForm(form url.Values) (tags.List, error) {
	list, err := tags.FromQuery(form, "Tags.member.")
	if err != nil {
		return nil, tagError(err)
	}
	if err := list.Validate(tags.MaxPerResource); err != nil {
		return nil, tagError(err)
	}
	return list, nil
}

// tagError maps a tag validation failure to SNS's error codes.
func tagError(err error) error {
	var verr *tags.ValidationError
	if !errors.As(err, &verr) {
		return internal(err)
	}
	if verr.Reason == tags.TooMany {
		return badRequest("TagLimitExceeded", "Could not complete request: tag quota of per resource exceeded")
	}
	return badRequest("InvalidParameter", "Invalid parameter: Tags Reason: "+verr.Error())
}

// loadTaggedTopic resolves ResourceArn as the tagging API does: a missing
// topic is ResourceNotFound, not NotFound.
func (s *Service) loadTaggedTopic(arn string) (topicRecord, error) {
	record, err := s.loadTopicByARN(arn)
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) && apiErr.Code == "NotFound" {
		return topicRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFound", Message: "Resource does not exist"}
	}
	return record, err
}

func (s *Service) tagResource(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadTaggedTopic(form.Get("ResourceArn"))
	if err != nil {
		return err
	}
	add, err := topicTagsFromForm(form)
	if err != nil {
		return err
	}
	merged := record.Tags.Merge(add)
	if err := merged.Validate(tags.MaxPerResource); err != nil {
		return tagError(err)
	}
	record.Tags = merged
	if err := s.putTopic(record); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, emptyResult("TagResource", requestID))
	return nil
}

func (s *Service) untagResource(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadTaggedTopic(form.Get("ResourceArn"))
	if err != nil {
		return err
	}
	record.Tags = record.Tags.Without(tags.KeysFromQuery(form, "TagKeys.member."))
	if err := s.putTopic(record); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, emptyResult("UntagResource", requestID))
	return nil
}

func (s *Service) listTagsForResource(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadTaggedTopic(form.Get("ResourceArn"))
	if err != nil {
		return err
	}
	out := make([]tagXML, 0, len(record.Tags))
	for _, t := range record.Tags {
		out = append(out, tagXML{Key: t.Key, Value: t.Value})
	}
	writeXML(w, http.StatusOK, listTagsForResourceResponse{
		XMLNS:            namespace,
		Result:           listTagsForResourceResult{Tags: out},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	})
	return nil
}

// emptyResult is <ActionResponse><ActionResult/>…</ActionResponse>.
func emptyResult(action, requestID string) any {
	return struct {
		XMLName          xml.Name
		XMLNS            string `xml:"xmlns,attr"`
		Result           struct{ XMLName xml.Name }
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{
		XMLName:          xml.Name{Local: action + "Response"},
		XMLNS:            namespace,
		Result:           struct{ XMLName xml.Name }{XMLName: xml.Name{Local: action + "Result"}},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}
}
