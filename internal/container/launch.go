package container

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/docker/docker/api/types/mount"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

// Paths inside a Lambda container.
const (
	taskRoot        = "/var/task"
	layerRoot       = "/opt"
	baseEntrypoint  = "/lambda-entrypoint.sh"
	imageRIEPath    = "/usr/local/bin/aws-lambda-rie"
	mountedRIEPath  = "/usr/local/stratus/aws-lambda-rie"
	rieListenPort   = "8080/tcp"
	defaultTimeoutS = 3
)

// imageDefaults is what the image itself configures, read from docker inspect.
type imageDefaults struct {
	Entrypoint []string
	Cmd        []string
	WorkingDir string
}

// launchInput is everything a launch plan is derived from. Host paths are
// empty when the function has nothing there.
type launchInput struct {
	Spec     lambdasvc.FunctionSpec
	Runtime  Runtime // zero for container-image functions
	Image    string
	Defaults imageDefaults

	CodeDir          string // host directory holding the extracted zip
	CodeHasBootstrap bool   // CodeDir contains a bootstrap executable
	OptDir           string // host directory holding the merged layers
	RIEHostPath      string // host copy of aws-lambda-rie, for images without one
}

// launchPlan is how one Lambda container is started.
type launchPlan struct {
	Image      string
	Entrypoint []string // nil keeps the image's own
	Cmd        []string
	WorkingDir string
	Env        []string
	Mounts     []mount.Mount
}

// planLaunch decides how to start a function so that the Runtime Interface
// Emulator listens on rieListenPort and runs the function's runtime.
func planLaunch(in launchInput) (launchPlan, error) {
	plan := launchPlan{Image: in.Image, Env: lambdaEnv(in.Spec)}
	if in.Spec.PackageType == lambdasvc.PackageTypeImage {
		return planImage(plan, in), nil
	}
	return planZip(plan, in)
}

func planZip(plan launchPlan, in launchInput) (launchPlan, error) {
	plan.WorkingDir = taskRoot
	plan.Mounts = append(plan.Mounts, readOnlyBind(in.CodeDir, taskRoot))
	if in.OptDir != "" {
		plan.Mounts = append(plan.Mounts, readOnlyBind(in.OptDir, layerRoot))
	}

	switch in.Runtime.Family {
	case FamilyCustom:
		// Lambda runs the bootstrap from the code, or else from a layer.
		bootstrap := taskRoot + "/bootstrap"
		if !in.CodeHasBootstrap {
			if in.OptDir == "" {
				return launchPlan{}, &apierror.Error{
					StatusCode: http.StatusBadRequest,
					Code:       "InvalidParameterValueException",
					Message:    "runtime " + in.Spec.Runtime + " needs an executable named bootstrap at the root of the zip or a layer",
				}
			}
			bootstrap = layerRoot + "/bootstrap"
		}
		plan.Entrypoint = []string{imageRIEPath}
		plan.Cmd = []string{bootstrap}
		plan.Env = append(plan.Env, "_HANDLER="+in.Spec.Handler)
	default:
		// The base image's entrypoint starts the emulator around the
		// runtime and takes the handler as its only argument.
		plan.Cmd = []string{in.Spec.Handler}
	}
	return plan, nil
}

// needsMountedRIE reports whether a launch has to mount the host copy of the
// emulator: a container image whose entrypoint does not start one.
func needsMountedRIE(in launchInput) bool {
	return in.Spec.PackageType == lambdasvc.PackageTypeImage && !bundlesRIE(imageEntrypoint(in))
}

func imageEntrypoint(in launchInput) []string {
	return firstNonEmpty(in.Spec.ImageConfig.EntryPoint, in.Defaults.Entrypoint)
}

func planImage(plan launchPlan, in launchInput) launchPlan {
	cfg := in.Spec.ImageConfig
	entrypoint := imageEntrypoint(in)
	cmd := firstNonEmpty(cfg.Command, in.Defaults.Cmd)
	plan.WorkingDir = cfg.WorkingDirectory
	if plan.WorkingDir == "" {
		plan.WorkingDir = in.Defaults.WorkingDir
	}

	if bundlesRIE(entrypoint) {
		plan.Entrypoint = entrypoint
		plan.Cmd = cmd
		return plan
	}

	// Images built on a non-AWS base start the runtime interface client
	// directly, which expects to be inside Lambda. Wrap it in the emulator,
	// as AWS documents for testing such images locally.
	plan.Entrypoint = append([]string{mountedRIEPath}, entrypoint...)
	plan.Cmd = cmd
	plan.Mounts = append(plan.Mounts, mount.Mount{
		Type:     mount.TypeBind,
		Source:   in.RIEHostPath,
		Target:   mountedRIEPath,
		ReadOnly: true,
	})
	return plan
}

// bundlesRIE reports whether an entrypoint already starts the emulator: an
// AWS base image's entrypoint script, or the emulator itself.
func bundlesRIE(entrypoint []string) bool {
	if len(entrypoint) == 0 {
		return false
	}
	switch entrypoint[0] {
	case baseEntrypoint, imageRIEPath, "aws-lambda-rie", mountedRIEPath:
		return true
	}
	return false
}

// lambdaEnv is the environment Lambda gives every function, then the
// function's own variables, which win.
func lambdaEnv(spec lambdasvc.FunctionSpec) []string {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultTimeoutS
	}
	memory := spec.MemorySize
	if memory <= 0 {
		memory = 128
	}
	vars := map[string]string{
		"AWS_LAMBDA_FUNCTION_NAME":        spec.FunctionName,
		"AWS_LAMBDA_FUNCTION_VERSION":     "$LATEST",
		"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": strconv.Itoa(memory),
		"AWS_LAMBDA_FUNCTION_TIMEOUT":     strconv.Itoa(timeout),
		"AWS_LAMBDA_LOG_GROUP_NAME":       "/aws/lambda/" + spec.FunctionName,
		"AWS_LAMBDA_LOG_STREAM_NAME":      "stratus",
		"LAMBDA_TASK_ROOT":                taskRoot,
	}
	if spec.Region != "" {
		vars["AWS_REGION"] = spec.Region
		vars["AWS_DEFAULT_REGION"] = spec.Region
	}
	for key, value := range spec.Environment {
		vars[key] = value
	}
	env := make([]string, 0, len(vars))
	for key, value := range vars {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env
}

func readOnlyBind(source, target string) mount.Mount {
	return mount.Mount{Type: mount.TypeBind, Source: source, Target: target, ReadOnly: true}
}

func firstNonEmpty(values ...[]string) []string {
	for _, v := range values {
		if len(v) > 0 {
			return append([]string(nil), v...)
		}
	}
	return nil
}
