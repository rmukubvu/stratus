package container

import (
	"errors"
	"testing"

	"github.com/stratus/internal/apierror"
)

func envLookup(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestRuntimeCatalogResolvesAWSBaseImages(t *testing.T) {
	catalog := NewRuntimeCatalog(nil)
	cases := map[string]struct {
		image  string
		family RuntimeFamily
	}{
		"python3.11":      {"public.ecr.aws/lambda/python:3.11", FamilyManaged},
		"nodejs20.x":      {"public.ecr.aws/lambda/nodejs:20", FamilyManaged},
		"java21":          {"public.ecr.aws/lambda/java:21", FamilyManaged},
		"java8.al2":       {"public.ecr.aws/lambda/java:8.al2", FamilyManaged},
		"provided.al2023": {"public.ecr.aws/lambda/provided:al2023", FamilyCustom},
		"dotnet8":         {"public.ecr.aws/lambda/dotnet:8", FamilyManaged},
	}
	for id, want := range cases {
		rt, image, err := catalog.Resolve(id)
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v", id, err)
		}
		if image != want.image || rt.Family != want.family {
			t.Fatalf("Resolve(%q) = %q/%s, want %q/%s", id, image, rt.Family, want.image, want.family)
		}
	}
}

func TestRuntimeCatalogRejectsUnknownRuntime(t *testing.T) {
	_, _, err := NewRuntimeCatalog(nil).Resolve("cobol1")
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "NotImplementedException" {
		t.Fatalf("Resolve(cobol1) error = %v, want NotImplementedException", err)
	}
}

func TestRuntimeCatalogUsesRepositoryOverride(t *testing.T) {
	catalog := NewRuntimeCatalog(envLookup(map[string]string{
		EnvLambdaImageRepository: "registry.corp.example/aws-lambda/",
	}))
	_, image, err := catalog.Resolve("java17")
	if err != nil {
		t.Fatal(err)
	}
	if image != "registry.corp.example/aws-lambda/java:17" {
		t.Fatalf("image = %q", image)
	}
	if got := catalog.RIEImage(); got != "registry.corp.example/aws-lambda/provided:al2023" {
		t.Fatalf("RIEImage() = %q", got)
	}
}

func TestRuntimeCatalogPerRuntimeOverrideWins(t *testing.T) {
	catalog := NewRuntimeCatalog(envLookup(map[string]string{
		EnvLambdaImageRepository:          "registry.corp.example/aws-lambda",
		"STRATUS_LAMBDA_IMAGE_PYTHON3_12": "python-lambda:hardened",
	}))
	_, image, err := catalog.Resolve("python3.12")
	if err != nil {
		t.Fatal(err)
	}
	if image != "python-lambda:hardened" {
		t.Fatalf("image = %q", image)
	}
}

func TestRuntimeImageEnv(t *testing.T) {
	cases := map[string]string{
		"java21":          "STRATUS_LAMBDA_IMAGE_JAVA21",
		"nodejs20.x":      "STRATUS_LAMBDA_IMAGE_NODEJS20_X",
		"provided.al2023": "STRATUS_LAMBDA_IMAGE_PROVIDED_AL2023",
	}
	for id, want := range cases {
		if got := RuntimeImageEnv(id); got != want {
			t.Fatalf("RuntimeImageEnv(%q) = %q, want %q", id, got, want)
		}
	}
}
