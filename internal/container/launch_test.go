package container

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

func zipInput(runtime string, family RuntimeFamily) launchInput {
	return launchInput{
		Spec: lambdasvc.FunctionSpec{
			FunctionName: "orders",
			Handler:      "example.Handler::handleRequest",
			Runtime:      runtime,
			Timeout:      7,
			MemorySize:   512,
			Region:       "eu-central-1",
			PackageType:  lambdasvc.PackageTypeZip,
			Environment:  map[string]string{"AWS_REGION": "eu-west-1", "TABLE": "orders"},
		},
		Runtime: Runtime{ID: runtime, Family: family},
		Image:   "public.ecr.aws/lambda/" + runtime,
		CodeDir: "/data/lambda/orders/source",
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func TestPlanManagedRuntimeUsesBaseEntrypointWithHandler(t *testing.T) {
	plan, err := planLaunch(zipInput("java21", FamilyManaged))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Entrypoint != nil {
		t.Fatalf("Entrypoint = %v, want the image's own", plan.Entrypoint)
	}
	if !reflect.DeepEqual(plan.Cmd, []string{"example.Handler::handleRequest"}) {
		t.Fatalf("Cmd = %v", plan.Cmd)
	}
	if plan.WorkingDir != "/var/task" || len(plan.Mounts) != 1 || plan.Mounts[0].Target != "/var/task" || !plan.Mounts[0].ReadOnly {
		t.Fatalf("unexpected task mount: %+v in %q", plan.Mounts, plan.WorkingDir)
	}
}

func TestPlanSetsLambdaEnvironmentAndFunctionVariablesWin(t *testing.T) {
	plan, err := planLaunch(zipInput("python3.12", FamilyManaged))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"AWS_LAMBDA_FUNCTION_NAME":        "orders",
		"AWS_LAMBDA_FUNCTION_TIMEOUT":     "7",
		"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "512",
		"AWS_DEFAULT_REGION":              "eu-central-1",
		"AWS_REGION":                      "eu-west-1",
		"TABLE":                           "orders",
	}
	for key, value := range want {
		if got, ok := envValue(plan.Env, key); !ok || got != value {
			t.Fatalf("%s = %q, want %q (env %v)", key, got, value, plan.Env)
		}
	}
}

func TestPlanCustomRuntimeRunsBootstrapUnderEmulator(t *testing.T) {
	in := zipInput("provided.al2023", FamilyCustom)
	in.Spec.Handler = "bootstrap"
	in.CodeHasBootstrap = true
	plan, err := planLaunch(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Entrypoint, []string{"/usr/local/bin/aws-lambda-rie"}) || !reflect.DeepEqual(plan.Cmd, []string{"/var/task/bootstrap"}) {
		t.Fatalf("Entrypoint/Cmd = %v %v", plan.Entrypoint, plan.Cmd)
	}
	if got, _ := envValue(plan.Env, "_HANDLER"); got != "bootstrap" {
		t.Fatalf("_HANDLER = %q", got)
	}
}

func TestPlanCustomRuntimeFallsBackToLayerBootstrap(t *testing.T) {
	in := zipInput("provided.al2", FamilyCustom)
	in.OptDir = "/data/runtime/layers/orders"
	plan, err := planLaunch(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Cmd, []string{"/opt/bootstrap"}) {
		t.Fatalf("Cmd = %v", plan.Cmd)
	}
	if len(plan.Mounts) != 2 || plan.Mounts[1].Target != "/opt" {
		t.Fatalf("Mounts = %+v", plan.Mounts)
	}
}

func TestPlanCustomRuntimeWithoutBootstrapIsRejected(t *testing.T) {
	_, err := planLaunch(zipInput("provided.al2023", FamilyCustom))
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "InvalidParameterValueException" {
		t.Fatalf("error = %v", err)
	}
}

func imageInput(defaults imageDefaults, cfg lambdasvc.ImageConfig) launchInput {
	return launchInput{
		Spec: lambdasvc.FunctionSpec{
			FunctionName: "img",
			PackageType:  lambdasvc.PackageTypeImage,
			ImageURI:     "orders:latest",
			ImageConfig:  cfg,
		},
		Image:       "orders:latest",
		Defaults:    defaults,
		RIEHostPath: "/data/runtime/rie/aws-lambda-rie",
	}
}

func TestPlanAWSBaseImageRunsAsBuilt(t *testing.T) {
	in := imageInput(imageDefaults{Entrypoint: []string{"/lambda-entrypoint.sh"}, Cmd: []string{"app.handler"}, WorkingDir: "/var/task"}, lambdasvc.ImageConfig{})
	if needsMountedRIE(in) {
		t.Fatalf("an AWS base image bundles the emulator")
	}
	plan, err := planLaunch(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Entrypoint, []string{"/lambda-entrypoint.sh"}) || !reflect.DeepEqual(plan.Cmd, []string{"app.handler"}) || len(plan.Mounts) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanImageConfigOverridesImageDefaults(t *testing.T) {
	in := imageInput(
		imageDefaults{Entrypoint: []string{"/lambda-entrypoint.sh"}, Cmd: []string{"app.handler"}, WorkingDir: "/var/task"},
		lambdasvc.ImageConfig{Command: []string{"app.other"}, WorkingDirectory: "/srv"},
	)
	plan, err := planLaunch(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Cmd, []string{"app.other"}) || plan.WorkingDir != "/srv" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanCustomBaseImageIsWrappedInMountedEmulator(t *testing.T) {
	in := imageInput(imageDefaults{Entrypoint: []string{"/usr/local/bin/python", "-m", "awslambdaric"}, Cmd: []string{"app.handler"}}, lambdasvc.ImageConfig{})
	if !needsMountedRIE(in) {
		t.Fatalf("a non-AWS base image needs the mounted emulator")
	}
	plan, err := planLaunch(in)
	if err != nil {
		t.Fatal(err)
	}
	wantEntry := []string{"/usr/local/stratus/aws-lambda-rie", "/usr/local/bin/python", "-m", "awslambdaric"}
	if !reflect.DeepEqual(plan.Entrypoint, wantEntry) || !reflect.DeepEqual(plan.Cmd, []string{"app.handler"}) {
		t.Fatalf("Entrypoint/Cmd = %v %v", plan.Entrypoint, plan.Cmd)
	}
	if len(plan.Mounts) != 1 || plan.Mounts[0].Source != in.RIEHostPath || plan.Mounts[0].Target != "/usr/local/stratus/aws-lambda-rie" {
		t.Fatalf("Mounts = %+v", plan.Mounts)
	}
}
