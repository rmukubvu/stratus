// Package awsmodel is Stratus's catalogue of the AWS operations it emulates,
// generated from the service models botocore ships (scripts/models).
//
// For each Stratus service it holds every operation AWS defines, how the
// operation is bound to HTTP, the errors it declares and the constraints AWS
// enforces on its input. It answers "does AWS have this operation?", backs
// request validation, and, joined with coverage.json, "does Stratus implement
// it?".
package awsmodel

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
)

//go:embed models/*.json
var modelFiles embed.FS

// Service is one Stratus service's catalogue. Most services are one AWS API;
// SES is two (v1 and v2).
type Service struct {
	Name     string `json:"service"`
	Botocore string `json:"botocore"`
	APIs     []*API `json:"models"`
}

// API is one AWS service model.
type API struct {
	Model        string               `json:"model"`
	EndpointName string               `json:"name"`
	APIVersion   string               `json:"apiVersion"`
	Protocol     string               `json:"protocol"`
	JSONVersion  string               `json:"jsonVersion"`
	TargetPrefix string               `json:"targetPrefix"`
	SigningName  string               `json:"signingName"`
	Operations   map[string]Operation `json:"operations"`
	Shapes       map[string]*Shape    `json:"shapes"`
}

// Operation is one AWS operation.
type Operation struct {
	HTTP       HTTPBinding `json:"http"`
	Input      string      `json:"input"`
	Errors     []string    `json:"errors"`
	Deprecated bool        `json:"deprecated"`
}

// HTTPBinding is how an operation is sent over HTTP.
type HTTPBinding struct {
	Method     string `json:"method"`
	RequestURI string `json:"requestUri"`
}

// Shape is an input shape and the constraints AWS enforces on it.
type Shape struct {
	Type     string            `json:"type"`
	Required []string          `json:"required"`
	Min      *float64          `json:"min"`
	Max      *float64          `json:"max"`
	Pattern  string            `json:"pattern"`
	Enum     []string          `json:"enum"`
	Members  map[string]Member `json:"members"`
	Member   string            `json:"member"`
	Key      string            `json:"key"`
	Value    string            `json:"value"`
}

// Member is a structure member: the shape it refers to and where it travels.
type Member struct {
	Shape        string `json:"shape"`
	Location     string `json:"location"`
	LocationName string `json:"locationName"`
	JSONName     string `json:"jsonName"`
}

// Body reports whether the member travels in the request body rather than
// the URI, query string or headers.
func (m Member) Body() bool { return m.Location == "" }

// Operation finds an operation by name across the service's APIs.
func (s *Service) Operation(name string) (*API, Operation, bool) {
	for _, api := range s.APIs {
		if op, ok := api.Operations[name]; ok {
			return api, op, true
		}
	}
	return nil, Operation{}, false
}

// OperationNames lists every operation the service's APIs define, sorted and
// without duplicates.
func (s *Service) OperationNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, api := range s.APIs {
		for name := range api.Operations {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// Shape returns the named shape, or nil when the model does not define it.
func (a *API) Shape(name string) *Shape { return a.Shapes[name] }

var (
	mu    sync.Mutex
	cache = map[string]*Service{}
)

// Load returns the named Stratus service's catalogue. The result is shared
// and must not be modified.
func Load(service string) (*Service, error) {
	mu.Lock()
	defer mu.Unlock()
	if svc, ok := cache[service]; ok {
		return svc, nil
	}
	data, err := modelFiles.ReadFile(path.Join("models", service+".json"))
	if err != nil {
		return nil, fmt.Errorf("awsmodel: no catalogue for service %q", service)
	}
	var svc Service
	if err := json.Unmarshal(data, &svc); err != nil {
		return nil, fmt.Errorf("awsmodel: decode %s: %w", service, err)
	}
	cache[service] = &svc
	return &svc, nil
}

// Services lists the Stratus services the catalogue covers, sorted.
func Services() []string {
	entries, err := modelFiles.ReadDir("models")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), ".json"); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
