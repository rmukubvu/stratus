package container

import (
	"net/http"
	"sort"
	"strings"

	"github.com/stratus/internal/apierror"
)

// DefaultLambdaImageRepository is where AWS publishes its Lambda base images.
// Every image there ships the Runtime Interface Emulator, so Stratus runs the
// same runtime code as Lambda instead of a hand-written server per language.
const DefaultLambdaImageRepository = "public.ecr.aws/lambda"

// Environment variables that point Stratus at other images, for private
// registries and air-gapped networks.
const (
	// EnvLambdaImageRepository replaces DefaultLambdaImageRepository for
	// every runtime, e.g. registry.corp.example/aws-lambda.
	EnvLambdaImageRepository = "STRATUS_LAMBDA_IMAGE_REPOSITORY"
	// envLambdaImagePrefix + the runtime, upper-cased with every other
	// character as "_", overrides one runtime's image. For example
	// STRATUS_LAMBDA_IMAGE_JAVA21 or STRATUS_LAMBDA_IMAGE_PYTHON3_12.
	envLambdaImagePrefix = "STRATUS_LAMBDA_IMAGE_"
)

// RuntimeFamily groups runtimes that start the same way.
type RuntimeFamily string

const (
	// FamilyManaged runtimes (Python, Node.js, Java, Ruby, .NET) start
	// through the base image's own entrypoint, given the handler.
	FamilyManaged RuntimeFamily = "managed"
	// FamilyCustom runtimes (provided.*: Go, Rust, anything else) run the
	// function's own bootstrap executable.
	FamilyCustom RuntimeFamily = "custom"
)

// Runtime is one Lambda runtime identifier and the base image that runs it.
type Runtime struct {
	ID     string
	Image  string // repository-relative name and tag, e.g. "java:21"
	Family RuntimeFamily
}

// lambdaRuntimes lists the runtimes Stratus can execute. Deprecated runtimes
// AWS no longer publishes images for are left out.
var lambdaRuntimes = []Runtime{
	{ID: "python3.9", Image: "python:3.9", Family: FamilyManaged},
	{ID: "python3.10", Image: "python:3.10", Family: FamilyManaged},
	{ID: "python3.11", Image: "python:3.11", Family: FamilyManaged},
	{ID: "python3.12", Image: "python:3.12", Family: FamilyManaged},
	{ID: "python3.13", Image: "python:3.13", Family: FamilyManaged},
	{ID: "nodejs18.x", Image: "nodejs:18", Family: FamilyManaged},
	{ID: "nodejs20.x", Image: "nodejs:20", Family: FamilyManaged},
	{ID: "nodejs22.x", Image: "nodejs:22", Family: FamilyManaged},
	{ID: "java8.al2", Image: "java:8.al2", Family: FamilyManaged},
	{ID: "java11", Image: "java:11", Family: FamilyManaged},
	{ID: "java17", Image: "java:17", Family: FamilyManaged},
	{ID: "java21", Image: "java:21", Family: FamilyManaged},
	{ID: "ruby3.2", Image: "ruby:3.2", Family: FamilyManaged},
	{ID: "ruby3.3", Image: "ruby:3.3", Family: FamilyManaged},
	{ID: "dotnet8", Image: "dotnet:8", Family: FamilyManaged},
	{ID: "provided.al2", Image: "provided:al2", Family: FamilyCustom},
	{ID: "provided.al2023", Image: "provided:al2023", Family: FamilyCustom},
}

// rieSourceRuntime is the base image Stratus copies the Runtime Interface
// Emulator from when a container-image function does not bundle one.
const rieSourceRuntime = "provided.al2023"

// RuntimeCatalog resolves runtime identifiers to the images that run them.
type RuntimeCatalog struct {
	byID   map[string]Runtime
	lookup func(string) (string, bool)
}

// NewRuntimeCatalog builds a catalog that reads image overrides through
// lookup, which is os.LookupEnv outside tests.
func NewRuntimeCatalog(lookup func(string) (string, bool)) RuntimeCatalog {
	byID := make(map[string]Runtime, len(lambdaRuntimes))
	for _, rt := range lambdaRuntimes {
		byID[rt.ID] = rt
	}
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	return RuntimeCatalog{byID: byID, lookup: lookup}
}

// Resolve returns the runtime and the full image reference to run it with.
func (c RuntimeCatalog) Resolve(runtimeID string) (Runtime, string, error) {
	rt, ok := c.byID[runtimeID]
	if !ok {
		return Runtime{}, "", &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message: "lambda execution for runtime is not supported yet: " + runtimeID +
				" (supported: " + strings.Join(c.IDs(), ", ") + ")",
		}
	}
	return rt, c.imageFor(rt), nil
}

// RIEImage is the image the Runtime Interface Emulator binary is copied from.
func (c RuntimeCatalog) RIEImage() string {
	if image, ok := c.lookup("STRATUS_LAMBDA_RIE_IMAGE"); ok && image != "" {
		return image
	}
	return c.imageFor(c.byID[rieSourceRuntime])
}

// IDs lists the supported runtime identifiers in sorted order.
func (c RuntimeCatalog) IDs() []string {
	ids := make([]string, 0, len(c.byID))
	for id := range c.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (c RuntimeCatalog) imageFor(rt Runtime) string {
	if image, ok := c.lookup(RuntimeImageEnv(rt.ID)); ok && image != "" {
		return image
	}
	repository := DefaultLambdaImageRepository
	if custom, ok := c.lookup(EnvLambdaImageRepository); ok && custom != "" {
		repository = strings.TrimSuffix(custom, "/")
	}
	return repository + "/" + rt.Image
}

// RuntimeImageEnv names the variable that overrides one runtime's image.
func RuntimeImageEnv(runtimeID string) string {
	var b strings.Builder
	b.WriteString(envLambdaImagePrefix)
	for _, r := range strings.ToUpper(runtimeID) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
