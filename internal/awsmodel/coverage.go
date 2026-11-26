package awsmodel

import (
	_ "embed"
	"encoding/json"
	"sort"
	"sync"
)

// coverageJSON lists, per service, the operations Stratus implements, as
// measured by scripts/models/coverage.py against a running Stratus.
//
//go:embed coverage.json
var coverageJSON []byte

var loadCoverage = sync.OnceValue(func() map[string]map[string]bool {
	var raw map[string][]string
	if err := json.Unmarshal(coverageJSON, &raw); err != nil {
		panic("awsmodel: coverage.json: " + err.Error())
	}
	out := make(map[string]map[string]bool, len(raw))
	for service, ops := range raw {
		set := make(map[string]bool, len(ops))
		for _, op := range ops {
			set[op] = true
		}
		out[service] = set
	}
	return out
})

// Support is how far Stratus covers an AWS operation.
type Support string

const (
	// Implemented operations are served by Stratus.
	Implemented Support = "implemented"
	// NotImplemented operations exist in AWS; Stratus answers 501.
	NotImplemented Support = "not_implemented"
	// UnknownOperation names no operation the AWS model defines.
	UnknownOperation Support = "unknown_operation"
	// UnknownService names a service Stratus does not emulate.
	UnknownService Support = "unknown_service"
)

// Lookup reports how Stratus supports one operation of one service.
func Lookup(service, operation string) Support {
	svc, err := Load(service)
	if err != nil {
		return UnknownService
	}
	if _, _, ok := svc.Operation(operation); !ok {
		return UnknownOperation
	}
	if loadCoverage()[service][operation] {
		return Implemented
	}
	return NotImplemented
}

// ServiceCoverage is one service's operations split by support.
type ServiceCoverage struct {
	Service        string   `json:"service"`
	Implemented    []string `json:"implemented"`
	NotImplemented []string `json:"not_implemented"`
}

// Coverage splits a service's AWS operations into those Stratus implements
// and those it answers with 501.
func Coverage(service string) (ServiceCoverage, error) {
	svc, err := Load(service)
	if err != nil {
		return ServiceCoverage{}, err
	}
	out := ServiceCoverage{Service: service, Implemented: []string{}, NotImplemented: []string{}}
	done := loadCoverage()[service]
	for _, op := range svc.OperationNames() {
		if done[op] {
			out.Implemented = append(out.Implemented, op)
		} else {
			out.NotImplemented = append(out.NotImplemented, op)
		}
	}
	sort.Strings(out.Implemented)
	return out, nil
}
