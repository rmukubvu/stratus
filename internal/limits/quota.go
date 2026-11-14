// Package limits enforces AWS service quotas on traffic to Stratus: request
// rates per account, per resource or per S3 prefix, and concurrency caps.
// A breach returns the same throttling error AWS would, so an application
// under load meets its limits locally, in the same order it would in AWS.
//
// Quotas come from a Profile: a built-in base (aws-default or strict),
// optional overrides, and a scale factor. Stratus handles a few thousand
// requests per second, far below most real quotas, so scaling them down
// lets a laptop-sized load reach the same limits a production load would.
//
// The package is off unless a profile is applied, and costs one atomic load
// per request while it is off.
package limits

import (
	"strings"
	"time"
)

// Clock lets tests control time.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// KeyScope says which traffic shares one bucket of a rate rule.
type KeyScope string

const (
	// ScopeAccount shares the rule's bucket across an account and region.
	ScopeAccount KeyScope = "account"
	// ScopeResource gives each resource (queue, table, function, secret,
	// S3 bucket) its own bucket.
	ScopeResource KeyScope = "resource"
	// ScopePrefix gives each S3 key prefix its own bucket, as S3 scales
	// request rates per prefix.
	ScopePrefix KeyScope = "prefix"
)

// QuotaKey names one bucket or semaphore.
type QuotaKey struct {
	RuleID   string `json:"rule_id"`
	Account  string `json:"account"`
	Region   string `json:"region"`
	Resource string `json:"resource,omitempty"`
}

func (k QuotaKey) String() string {
	parts := []string{k.RuleID, k.Account, k.Region}
	if k.Resource != "" {
		parts = append(parts, k.Resource)
	}
	return strings.Join(parts, "/")
}

// Request describes an AWS API request for quota purposes.
type Request struct {
	RequestID string
	Account   string
	Region    string
	Service   string
	Operation string
	// Resource names what the request acts on, such as a queue, table,
	// function or S3 bucket. Empty when unknown.
	Resource string
	// Prefix is the S3 key prefix, "bucket/dir/"; empty for other
	// services.
	Prefix string
}

// Kind says which sort of quota a breach exceeded.
type Kind string

const (
	KindRate        Kind = "rate"
	KindConcurrency Kind = "concurrency"
)

// Breach records one request that exceeded a quota. It is a domain event:
// diagnostics and load reports subscribe to it.
type Breach struct {
	Time      time.Time `json:"time"`
	RuleID    string    `json:"rule_id"`
	Kind      Kind      `json:"kind"`
	Key       string    `json:"key"`
	Limit     float64   `json:"limit"`
	RequestID string    `json:"request_id,omitempty"`
	Service   string    `json:"service"`
	Operation string    `json:"operation,omitempty"`
	Resource  string    `json:"resource,omitempty"`
	// Reason refines a capacity breach, as DynamoDB's ThrottlingReasons
	// do: TableWriteKeyRangeThroughputExceeded and the like.
	Reason string `json:"reason,omitempty"`
}
