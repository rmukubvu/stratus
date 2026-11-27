package events

import (
	"errors"
	"net/http"
	"strings"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/tags"
)

const maxTagsPerResource = 50

func (s *Service) describeEventBus(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Name string `json:"Name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	name := defaultBus(input.Name)
	if strings.HasPrefix(name, "arn:") {
		name = name[strings.LastIndex(name, "/")+1:]
	}
	bus, err := s.loadEventBus(name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Arn":              bus.Arn,
		"CreationTime":     float64(bus.CreatedAt.UnixMilli()) / 1000,
		"LastModifiedTime": float64(bus.CreatedAt.UnixMilli()) / 1000,
		"Name":             bus.Name,
	})
	return nil
}

// taggable is an event bus or rule found by its ARN, with a way to store
// changed tags back.
type taggable struct {
	tags tags.List
	save func(tags.List) error
}

// resourceByARN resolves the ARNs EventBridge tags: event-bus/<name>,
// rule/<name> on the default bus and rule/<bus>/<name>.
func (s *Service) resourceByARN(arn string) (taggable, error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[2] != "events" {
		return taggable{}, validation("Parameter ResourceARN is not valid.")
	}
	path := strings.Split(parts[5], "/")
	switch {
	case path[0] == "event-bus" && len(path) == 2:
		bus, err := s.loadEventBus(path[1])
		if err != nil {
			return taggable{}, err
		}
		return taggable{tags: bus.Tags, save: func(l tags.List) error { bus.Tags = l; return s.putEventBus(bus) }}, nil
	case path[0] == "rule" && (len(path) == 2 || len(path) == 3):
		busName, name := defaultBusName, path[len(path)-1]
		if len(path) == 3 {
			busName = path[1]
		}
		rule, err := s.loadRule(busName, name)
		if err != nil {
			return taggable{}, err
		}
		return taggable{tags: rule.Tags, save: func(l tags.List) error { rule.Tags = l; return s.putRuleRecord(rule) }}, nil
	}
	return taggable{}, validation("Parameter ResourceARN is not valid.")
}

func (s *Service) tagResource(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ResourceARN string    `json:"ResourceARN"`
		Tags        tags.List `json:"Tags"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	res, err := s.resourceByARN(input.ResourceARN)
	if err != nil {
		return err
	}
	merged := res.tags.Merge(input.Tags)
	if err := validateTags(merged); err != nil {
		return err
	}
	if err := res.save(merged); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) untagResource(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ResourceARN string   `json:"ResourceARN"`
		TagKeys     []string `json:"TagKeys"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	res, err := s.resourceByARN(input.ResourceARN)
	if err != nil {
		return err
	}
	if err := res.save(res.tags.Without(input.TagKeys)); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

func (s *Service) listTagsForResource(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ResourceARN string `json:"ResourceARN"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	res, err := s.resourceByARN(input.ResourceARN)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"Tags": tagsJSON(res.tags)})
	return nil
}

func tagsJSON(list tags.List) []map[string]string {
	out := make([]map[string]string, 0, len(list))
	for _, t := range list {
		out = append(out, map[string]string{"Key": t.Key, "Value": t.Value})
	}
	return out
}

// validateTags maps a tag list AWS would refuse to EventBridge's errors.
func validateTags(list tags.List) error {
	err := list.Validate(maxTagsPerResource)
	var verr *tags.ValidationError
	if errors.As(err, &verr) {
		if verr.Reason == tags.TooMany {
			return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "LimitExceededException", Message: verr.Error()}
		}
		return validation(verr.Error())
	}
	return err
}
