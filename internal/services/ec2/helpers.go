package ec2

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/tags"
)

// ---- errors, in EC2's codes ----

func badRequest(code, message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: message}
}

func invalidParameter(message string) error { return badRequest("InvalidParameterValue", message) }

func missingParameter(name string) error {
	return badRequest("MissingParameter", fmt.Sprintf("The request must contain the parameter %s", name))
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalError", Message: err.Error()}
}

func notFound(code, noun, id string) error {
	return badRequest(code, fmt.Sprintf("The %s ID '%s' does not exist", noun, id))
}

// ---- storage ----

func (s *Service) put(bucket, id string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.metadata.Put(bucket, id, raw)
}

// load reads resource id into dst, or answers code (an EC2 *.NotFound code).
func (s *Service) load(bucket, id string, dst any, code, noun string) error {
	if id == "" {
		return notFound(code, noun, id)
	}
	raw, err := s.metadata.Get(bucket, id)
	if err != nil {
		return internal(err)
	}
	if raw == nil {
		return notFound(code, noun, id)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return internal(err)
	}
	return nil
}

// anyIn reports whether any resource in bucket satisfies pred.
func anyIn[T any](s *Service, bucket string, pred func(T) bool) bool {
	errFound := errors.New("found")
	err := s.metadata.Scan(bucket, "", func(_, v []byte) error {
		var r T
		if json.Unmarshal(v, &r) == nil && pred(r) {
			return errFound
		}
		return nil
	})
	return errors.Is(err, errFound)
}

// deleteWhere deletes every resource in bucket that belongs to vpcID.
func (s *Service) deleteWhere(bucket, vpcID string) error {
	var ids []string
	err := s.metadata.Scan(bucket, "", func(k, v []byte) error {
		var r struct {
			VpcID string `json:"vpc_id"`
		}
		if json.Unmarshal(v, &r) == nil && r.VpcID == vpcID {
			ids = append(ids, string(k))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.metadata.Delete(bucket, id); err != nil {
			return err
		}
	}
	return nil
}

// describe emits the resources in bucket that are named in ids (all when ids
// is empty) and match the request's filters. A named ID that does not exist
// is an error, as in EC2.
func describe[T filterable](c *call, bucket string, ids []string, code, noun string, emit func(T)) error {
	filters := filtersFromForm(c.form)
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = false
	}
	err := c.svc.metadata.Scan(bucket, "", func(k, v []byte) error {
		id := string(k)
		if _, named := wanted[id]; len(ids) > 0 && !named {
			return nil
		}
		var r T
		if err := json.Unmarshal(v, &r); err != nil {
			return err
		}
		wanted[id] = true
		if matches(r, filters) {
			emit(r)
		}
		return nil
	})
	if err != nil {
		return internal(err)
	}
	for _, id := range ids {
		if !wanted[id] {
			return notFound(code, noun, id)
		}
	}
	return nil
}

// ---- request parameters ----

// listParam reads <prefix>N values, N from 1.
func listParam(form url.Values, prefix string) []string {
	var out []string
	for n := 1; ; n++ {
		v, ok := form[prefix+strconv.Itoa(n)]
		if !ok {
			return out
		}
		out = append(out, v...)
	}
}

// filtersFromForm reads Filter.N.Name / Filter.N.Value.M.
func filtersFromForm(form url.Values) map[string][]string {
	filters := map[string][]string{}
	for n := 1; ; n++ {
		base := "Filter." + strconv.Itoa(n) + "."
		name := form.Get(base + "Name")
		if name == "" {
			return filters
		}
		filters[name] = append(filters[name], listParam(form, base+"Value.")...)
	}
}

// tagSpecification reads the tags a create call asks to put on a resource
// of resourceType (TagSpecification.N.ResourceType + .Tag.M.Key/Value).
func tagSpecification(form url.Values, resourceType string) (tags.List, error) {
	var list tags.List
	for n := 1; ; n++ {
		base := "TagSpecification." + strconv.Itoa(n) + "."
		kind := form.Get(base + "ResourceType")
		if kind == "" {
			break
		}
		if kind != resourceType {
			return nil, invalidParameter(fmt.Sprintf("'%s' is not a valid taggable resource type for this operation.", kind))
		}
		more, err := tags.FromQuery(form, base+"Tag.")
		if err != nil {
			return nil, invalidParameter(err.Error())
		}
		list = list.Merge(more)
	}
	if err := list.Validate(tags.MaxPerResource); err != nil {
		return nil, invalidParameter(err.Error())
	}
	return list, nil
}

// ---- small helpers ----

// newID returns an EC2-style ID: prefix, a dash and 17 hex digits.
func newID(prefix string) string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)[:17]
}

func prefixOf(id string) string {
	head, _, _ := strings.Cut(id, "-")
	return head
}

func suffix(id string) string {
	_, tail, _ := strings.Cut(id, "-")
	return tail
}

func parseCIDR(block string) (net.IP, *net.IPNet, error) { return net.ParseCIDR(block) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (s *Service) zoneID(zone string) string {
	for i, z := range s.zones() {
		if z == zone {
			return fmt.Sprintf("%s-az%d", s.scope.Region, i+1)
		}
	}
	return ""
}
