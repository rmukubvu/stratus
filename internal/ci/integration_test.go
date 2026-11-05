package ci

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestRunWithRealServer starts a real in-process Stratus and checks that the
// command can reach it, and that the data directory is gone afterwards.
func TestRunWithRealServer(t *testing.T) {
	var dataDir string
	runner := DefaultRunner()
	runner.TempDir = func() (string, error) {
		dir, err := os.MkdirTemp(t.TempDir(), "run-*")
		dataDir = dir
		return dir, err
	}
	runner.Execute = func(_ context.Context, _ []string, env []string) (int, error) {
		endpoint := lookup(env, "AWS_ENDPOINT_URL")
		resp, err := http.Get(endpoint + "/_stratus/control/v1/ready")
		if err != nil {
			return 1, nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ready": true`) {
			return 2, fmt.Errorf("not ready: %d %s", resp.StatusCode, body)
		}
		return 0, nil
	}

	code, err := runner.Run(context.Background(), Options{Command: []string{"probe"}, LogLevel: "error"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir %s still exists after run", dataDir)
	}
}

func lookup(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// The helper-process pattern lets the tests exec a real child process.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("STRATUS_CI_HELPER") != "1" {
		t.Skip("helper process only")
	}
	if os.Getenv("AWS_ENDPOINT_URL") == "" {
		os.Exit(9)
	}
	var code int
	fmt.Sscanf(os.Getenv("HELPER_EXIT"), "%d", &code)
	os.Exit(code)
}

func TestExecCommandReturnsChildExitCode(t *testing.T) {
	for _, want := range []int{0, 1, 7} {
		env := append(os.Environ(), "STRATUS_CI_HELPER=1", fmt.Sprintf("HELPER_EXIT=%d", want), "AWS_ENDPOINT_URL=x")
		got, err := ExecCommand(context.Background(), []string{os.Args[0], "-test.run=TestHelperProcess"}, env)
		if err != nil || got != want {
			t.Errorf("exit %d: got %d, %v", want, got, err)
		}
	}
}

func TestExecCommandReportsMissingExecutable(t *testing.T) {
	code, err := ExecCommand(context.Background(), []string{"stratus-definitely-not-a-command"}, os.Environ())
	if err == nil || code != ExitRunnerFailed {
		t.Fatalf("code=%d err=%v", code, err)
	}
}
