package control

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestOwnerOfUsesLongestPrefix(t *testing.T) {
	c := DefaultCatalog()
	cases := map[string]string{
		"dynamodb-tables":         "dynamodb",
		"dynamodb-items":          "dynamodb",
		"dynamodb-streams":        "dynamodbstreams",
		"dynamodb-stream-records": "dynamodbstreams",
		"apigateway-rest-apis":    "apigateway",
		"apigatewayv2-routes":     "apigatewayv2",
		"cloudwatch-metrics":      "monitoring",
		"cognito-user-pools":      "cognitoidp",
		"s3-objects":              "s3",
	}
	for bucket, want := range cases {
		got, ok := c.OwnerOf(bucket)
		if !ok || got != want {
			t.Errorf("OwnerOf(%q) = %q, %v; want %q", bucket, got, ok, want)
		}
	}
	if _, ok := c.OwnerOf("unknown-bucket"); ok {
		t.Error("unknown bucket should have no owner")
	}
}

func TestResolveNormalisesDeduplicatesAndSorts(t *testing.T) {
	got, err := DefaultCatalog().Resolve([]string{" SQS", "s3", "sqs", ""})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := []string{"s3", "sqs"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolveRejectsUnknownServicesWithHint(t *testing.T) {
	_, err := DefaultCatalog().Resolve([]string{"s3", "dynamo"})
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "UnknownService" || ce.Kind != KindInvalid {
		t.Fatalf("expected UnknownService error, got %v", err)
	}
	if ce.Hint == "" {
		t.Fatal("expected a hint listing valid services")
	}
}

func TestNewCatalogRejectsDuplicates(t *testing.T) {
	if _, err := NewCatalog(ServiceState{Name: "s3"}, ServiceState{Name: "s3"}); err == nil {
		t.Fatal("expected duplicate error")
	}
}

// TestCatalogCoversEveryServiceBucket guards against drift: every bucket
// constant declared in a service package must have an owner in the catalog,
// so reset and snapshot never silently miss state. A package may declare
// another service's bucket (CloudFormation writes to SQS, IAM, ...), but only
// if the owning package declares it too.
func TestCatalogCoversEveryServiceBucket(t *testing.T) {
	servicesDir := filepath.Join("..", "services")
	dirs, err := os.ReadDir(servicesDir)
	if err != nil {
		t.Fatalf("read services dir: %v", err)
	}
	bucketDecl := regexp.MustCompile(`[A-Za-z]*Bucket\s*=\s*"([^"]+)"`)
	catalog := DefaultCatalog()

	declared := map[string]map[string]string{} // service -> bucket -> file
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		service := dir.Name()
		if _, ok := catalog.Lookup(service); !ok {
			t.Errorf("service package %q is missing from DefaultCatalog", service)
		}
		declared[service] = map[string]string{}
		files, _ := filepath.Glob(filepath.Join(servicesDir, service, "*.go"))
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			for _, m := range bucketDecl.FindAllStringSubmatch(string(src), -1) {
				declared[service][m[1]] = file
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no service packages; is the test running from the right directory?")
	}

	for service, buckets := range declared {
		for bucket, file := range buckets {
			owner, ok := catalog.OwnerOf(bucket)
			switch {
			case !ok:
				t.Errorf("%s declares bucket %q, which no catalog service owns", file, bucket)
			case owner != service:
				if _, ownerDeclares := declared[owner][bucket]; !ownerDeclares {
					t.Errorf("%s declares bucket %q owned by %q, but %q never declares it", file, bucket, owner, owner)
				}
			}
		}
	}
}
