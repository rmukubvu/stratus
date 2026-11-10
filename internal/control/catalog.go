package control

import (
	"fmt"
	"sort"
	"strings"
)

// ServiceState describes where one emulated service keeps its state.
type ServiceState struct {
	// Name is the service identifier used by AWS SDKs, e.g. "s3", "sqs".
	Name string
	// BucketPrefixes select the metadata buckets owned by the service.
	BucketPrefixes []string
	// BlobNamespaces are top-level blob directories owned by the service.
	BlobNamespaces []string
	// UsesRuntime is true when the service runs code in Lambda containers,
	// which must be recycled when its state changes underneath them.
	UsesRuntime bool
}

// Catalog maps emulated services to the state they own. A metadata bucket
// belongs to the service with the longest matching prefix, so overlapping
// prefixes such as "dynamodb-" and "dynamodb-stream" resolve correctly.
type Catalog struct {
	services map[string]ServiceState
}

// NewCatalog builds a catalog and rejects duplicate service names.
func NewCatalog(states ...ServiceState) (Catalog, error) {
	services := make(map[string]ServiceState, len(states))
	for _, st := range states {
		if st.Name == "" {
			return Catalog{}, fmt.Errorf("catalog: service name must not be empty")
		}
		if _, dup := services[st.Name]; dup {
			return Catalog{}, fmt.Errorf("catalog: duplicate service %q", st.Name)
		}
		services[st.Name] = st
	}
	return Catalog{services: services}, nil
}

// DefaultCatalog describes the services Stratus ships with.
func DefaultCatalog() Catalog {
	c, err := NewCatalog(
		ServiceState{Name: "acm", BucketPrefixes: []string{"acm-"}},
		ServiceState{Name: "apigateway", BucketPrefixes: []string{"apigateway-"}},
		ServiceState{Name: "apigatewayv2", BucketPrefixes: []string{"apigatewayv2-"}},
		ServiceState{Name: "bedrock", BucketPrefixes: []string{"bedrock-"}},
		ServiceState{Name: "cloudformation", BucketPrefixes: []string{"cloudformation-"}},
		ServiceState{Name: "cognitoidp", BucketPrefixes: []string{"cognito-"}},
		ServiceState{Name: "dynamodb", BucketPrefixes: []string{"dynamodb-"}},
		ServiceState{Name: "dynamodbstreams", BucketPrefixes: []string{"dynamodb-stream"}},
		ServiceState{Name: "ecr", BucketPrefixes: []string{"ecr-"}},
		ServiceState{Name: "ecs", BucketPrefixes: []string{"ecs-"}},
		ServiceState{Name: "elasticache", BucketPrefixes: []string{"elasticache-"}},
		ServiceState{Name: "elbv2", BucketPrefixes: []string{"elbv2-"}},
		ServiceState{Name: "events", BucketPrefixes: []string{"events-"}},
		ServiceState{Name: "fis", BucketPrefixes: []string{"fis-"}},
		ServiceState{Name: "iam", BucketPrefixes: []string{"iam-"}},
		ServiceState{Name: "kafka", BucketPrefixes: []string{"kafka-"}},
		ServiceState{Name: "kinesis", BucketPrefixes: []string{"kinesis-"}},
		ServiceState{Name: "kms", BucketPrefixes: []string{"kms-"}},
		ServiceState{Name: "lambda", BucketPrefixes: []string{"lambda-"}, BlobNamespaces: []string{"lambda", "lambda-layers"}, UsesRuntime: true},
		ServiceState{Name: "logs", BucketPrefixes: []string{"logs-"}},
		ServiceState{Name: "monitoring", BucketPrefixes: []string{"cloudwatch-"}},
		ServiceState{Name: "rds", BucketPrefixes: []string{"rds-"}},
		ServiceState{Name: "route53", BucketPrefixes: []string{"route53-"}},
		ServiceState{Name: "s3", BucketPrefixes: []string{"s3-"}, BlobNamespaces: []string{"s3"}},
		ServiceState{Name: "scheduler", BucketPrefixes: []string{"scheduler-"}},
		ServiceState{Name: "secretsmanager", BucketPrefixes: []string{"secretsmanager-"}},
		ServiceState{Name: "servicediscovery", BucketPrefixes: []string{"servicediscovery-"}},
		ServiceState{Name: "ses", BucketPrefixes: []string{"ses-"}},
		ServiceState{Name: "sns", BucketPrefixes: []string{"sns-"}},
		ServiceState{Name: "sqs", BucketPrefixes: []string{"sqs-"}},
		ServiceState{Name: "ssm", BucketPrefixes: []string{"ssm-"}},
		ServiceState{Name: "stepfunctions", BucketPrefixes: []string{"stepfunctions-"}},
		ServiceState{Name: "sts"},
	)
	if err != nil {
		panic(err) // static data; covered by tests
	}
	return c
}

// Names returns all service names, sorted.
func (c Catalog) Names() []string {
	names := make([]string, 0, len(c.services))
	for name := range c.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Lookup returns the state description for a service.
func (c Catalog) Lookup(name string) (ServiceState, bool) {
	st, ok := c.services[name]
	return st, ok
}

// OwnerOf returns the service that owns a metadata bucket.
func (c Catalog) OwnerOf(bucket string) (string, bool) {
	owner, best := "", 0
	for name, st := range c.services {
		for _, prefix := range st.BucketPrefixes {
			if strings.HasPrefix(bucket, prefix) && len(prefix) > best {
				owner, best = name, len(prefix)
			}
		}
	}
	return owner, best > 0
}

// Resolve validates service names, removes duplicates and sorts them.
// An empty input resolves to an empty slice; callers decide what that means.
func (c Catalog) Resolve(names []string) ([]string, error) {
	seen := make(map[string]struct{}, len(names))
	var unknown []string
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if _, ok := c.services[name]; !ok {
			unknown = append(unknown, raw)
			continue
		}
		seen[name] = struct{}{}
	}
	if len(unknown) > 0 {
		return nil, &Error{
			Kind:    KindInvalid,
			Code:    "UnknownService",
			Message: fmt.Sprintf("unknown service(s): %s", strings.Join(unknown, ", ")),
			Hint:    "valid services are: " + strings.Join(c.Names(), ", "),
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
