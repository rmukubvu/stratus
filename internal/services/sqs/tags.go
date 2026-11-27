package sqs

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/stratus/internal/tags"
)

// maxTagsPerQueue is SQS's quota of tags per queue.
const maxTagsPerQueue = 50

// tagsFromForm reads the Tag.N.Key / Tag.N.Value pairs that CreateQueue
// and TagQueue take in the query protocol (the JSON protocol's tag map is
// converted to the same form).
func tagsFromForm(form url.Values) (tags.List, error) {
	list, err := tags.FromQuery(form, "Tag.")
	if err != nil {
		return nil, tagError(err)
	}
	if err := list.Validate(maxTagsPerQueue); err != nil {
		return nil, tagError(err)
	}
	return list, nil
}

func tagError(err error) error {
	var verr *tags.ValidationError
	if errors.As(err, &verr) {
		return badRequest("InvalidParameterValue", verr.Error())
	}
	return internal(err)
}

func appendTagMap(form url.Values, raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		form.Set(fmt.Sprintf("Tag.%d.Key", i+1), k)
		form.Set(fmt.Sprintf("Tag.%d.Value", i+1), scalarString(m[k]))
	}
}

// changeTags applies TagQueue or UntagQueue to the queue named in form
// and returns the queue's tags after the change (for ListQueueTags, its
// tags unchanged).
func (s *Service) changeTags(r *http.Request, form url.Values, operation string) (tags.List, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, err := s.queueFromForm(r, form)
	if err != nil {
		return nil, err
	}
	switch operation {
	case "TagQueue":
		add, err := tagsFromForm(form)
		if err != nil {
			return nil, err
		}
		merged := queue.Tags.Merge(add)
		if err := merged.Validate(maxTagsPerQueue); err != nil {
			return nil, tagError(err)
		}
		queue.Tags = merged
	case "UntagQueue":
		queue.Tags = queue.Tags.Without(tags.KeysFromQuery(form, "TagKey."))
	default:
		return queue.Tags, nil
	}
	if err := s.putQueue(queue); err != nil {
		return nil, internal(err)
	}
	return queue.Tags, nil
}

func (s *Service) handleTagsJSON(w http.ResponseWriter, r *http.Request, form url.Values, operation string) error {
	list, err := s.changeTags(r, form, operation)
	if err != nil {
		return err
	}
	if operation != "ListQueueTags" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	}
	out := map[string]string{}
	for _, t := range list {
		out[t.Key] = t.Value
	}
	writeJSON(w, http.StatusOK, map[string]any{"Tags": out})
	return nil
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func (s *Service) handleTagsQuery(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	list, err := s.changeTags(r, form, operation)
	if err != nil {
		return err
	}
	type response struct {
		XMLName          xml.Name
		XMLNS            string           `xml:"xmlns,attr"`
		Tags             *[]tagXML        `xml:"ListQueueTagsResult>Tag,omitempty"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	resp := response{XMLName: xml.Name{Local: operation + "Response"}, XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	if operation == "ListQueueTags" {
		out := make([]tagXML, 0, len(list))
		for _, t := range list {
			out = append(out, tagXML{Key: t.Key, Value: t.Value})
		}
		resp.Tags = &out
	}
	writeXML(w, http.StatusOK, resp)
	return nil
}
