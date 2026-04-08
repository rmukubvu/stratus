package container

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/stratus/internal/apierror"
	lambdasvc "github.com/stratus/internal/services/lambda"
)

const (
	runtimeImagePython311 = "public.ecr.aws/lambda/python:3.11"
	runtimePort           = "9001/tcp"
)

type Manager struct {
	logger        *slog.Logger
	client        *dockerclient.Client
	blobRoot      string
	runtimeDir    string
	runtimeScript string
	pool          *WarmPool
	httpClient    *http.Client
}

func NewManager(dataDir, blobRoot string, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	opts := []dockerclient.Opt{dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation()}
	if host := defaultDockerHost(); host != "" {
		opts = append(opts, dockerclient.WithHost(host))
	}
	client, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	runtimeDir := filepath.Join(dataDir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create runtime dir: %w", err)
	}

	runtimeScript := filepath.Join(runtimeDir, "lambda_runtime_server.py")
	if err := os.WriteFile(runtimeScript, []byte(runtimeServerScript), 0o644); err != nil {
		return nil, fmt.Errorf("write runtime server script: %w", err)
	}

	return &Manager{
		logger:        logger,
		client:        client,
		blobRoot:      blobRoot,
		runtimeDir:    runtimeDir,
		runtimeScript: runtimeScript,
		pool:          NewWarmPool(5 * time.Minute),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (m *Manager) Close() error {
	return m.pool.CloseAll(context.Background(), m.removeContainer)
}

func (m *Manager) CleanupFunction(ctx context.Context, functionName string) error {
	return m.pool.Remove(ctx, functionName, m.removeContainer)
}

func (m *Manager) Invoke(ctx context.Context, spec lambdasvc.FunctionSpec, payload []byte) (lambdasvc.InvokeResult, error) {
	imageName, err := runtimeImageFor(spec.Runtime)
	if err != nil {
		return lambdasvc.InvokeResult{}, err
	}

	if err := m.ensureImage(ctx, imageName); err != nil {
		return lambdasvc.InvokeResult{}, err
	}

	if err := m.pool.ExpireIdle(ctx, m.removeContainer); err != nil {
		m.logger.Warn("warm pool expiration failed", "error", err)
	}

	warm, err := m.pool.GetOrCreate(ctx, spec.FunctionName, func(ctx context.Context) (*WarmContainer, error) {
		return m.startContainer(ctx, spec, imageName)
	}, func(ctx context.Context, existing *WarmContainer) (bool, error) {
		return m.isHealthy(ctx, existing)
	}, m.removeContainer)
	if err != nil {
		return lambdasvc.InvokeResult{}, err
	}

	result, err := m.invokeHTTP(ctx, warm, spec, payload)
	if err == nil {
		return result, nil
	}

	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == "ServiceException" {
			_ = m.pool.Remove(context.Background(), spec.FunctionName, m.removeContainer)
		}
		return lambdasvc.InvokeResult{}, err
	}

	_ = m.pool.Remove(context.Background(), spec.FunctionName, m.removeContainer)
	return lambdasvc.InvokeResult{}, &apierror.Error{
		StatusCode: http.StatusInternalServerError,
		Code:       "ServiceException",
		Message:    err.Error(),
	}
}

func (m *Manager) ensureImage(ctx context.Context, imageName string) error {
	_, _, err := m.client.ImageInspectWithRaw(ctx, imageName)
	if err == nil {
		return nil
	}
	if dockerclient.IsErrNotFound(err) {
		return &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "lambda runtime image is not available locally: " + imageName,
		}
	}
	return &apierror.Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "ServiceException",
		Message:    "docker image inspection failed: " + err.Error(),
	}
}

func (m *Manager) startContainer(ctx context.Context, spec lambdasvc.FunctionSpec, imageName string) (*WarmContainer, error) {
	containerName := containerNameFor(spec.FunctionName)
	_ = m.removeNamedContainer(ctx, containerName)

	codeDir := filepath.Join(m.blobRoot, "lambda", spec.FunctionName, "source")
	if _, err := os.Stat(codeDir); err != nil {
		return nil, &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "ServiceException",
			Message:    "lambda code directory is missing: " + err.Error(),
		}
	}

	portSet := nat.PortSet{
		nat.Port(runtimePort): struct{}{},
	}
	portMap := nat.PortMap{
		nat.Port(runtimePort): []nat.PortBinding{{
			HostIP:   "127.0.0.1",
			HostPort: "",
		}},
	}

	env := []string{
		"PYTHONUNBUFFERED=1",
	}
	pythonPathEntries := []string{"/var/task"}
	for key, value := range spec.Environment {
		env = append(env, key+"="+value)
	}

	cfg := &container.Config{
		Image:        imageName,
		Entrypoint:   []string{"python", "/opt/stratus/lambda_runtime_server.py"},
		Cmd:          []string{"--handler", spec.Handler, "--port", "9001", "--function-name", spec.FunctionName},
		ExposedPorts: portSet,
		WorkingDir:   "/var/task",
	}

	hostCfg := &container.HostConfig{
		AutoRemove: false,
		Mounts: []mount.Mount{
			{
				Type:     mount.TypeBind,
				Source:   codeDir,
				Target:   "/var/task",
				ReadOnly: true,
			},
			{
				Type:     mount.TypeBind,
				Source:   m.runtimeDir,
				Target:   "/opt/stratus",
				ReadOnly: true,
			},
		},
		PortBindings: portMap,
	}
	for idx, layerDir := range spec.LayerDirs {
		target := fmt.Sprintf("/opt/stratus-layers/%d", idx)
		hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   layerDir,
			Target:   target,
			ReadOnly: true,
		})
		pythonPathEntries = append(pythonPathEntries, target, filepath.Join(target, "python"))
	}
	env = append(env, "PYTHONPATH="+strings.Join(pythonPathEntries, ":"))
	cfg.Env = env
	if spec.MemorySize > 0 {
		hostCfg.Memory = int64(spec.MemorySize) * 1024 * 1024
	}

	resp, err := m.client.ContainerCreate(ctx, cfg, hostCfg, &network.NetworkingConfig{}, &ocispec.Platform{}, containerName)
	if err != nil {
		return nil, &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "docker container create failed: " + err.Error(),
		}
	}

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.removeContainer(context.Background(), &WarmContainer{ID: resp.ID, Name: containerName})
		return nil, &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "docker container start failed: " + err.Error(),
		}
	}

	inspect, err := m.client.ContainerInspect(ctx, resp.ID)
	if err != nil {
		_ = m.removeContainer(context.Background(), &WarmContainer{ID: resp.ID, Name: containerName})
		return nil, &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "docker container inspect failed: " + err.Error(),
		}
	}

	portBindings := inspect.NetworkSettings.Ports[nat.Port(runtimePort)]
	if len(portBindings) == 0 {
		_ = m.removeContainer(context.Background(), &WarmContainer{ID: resp.ID, Name: containerName})
		return nil, &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "docker container did not expose a host port",
		}
	}

	hostPort, err := strconv.Atoi(portBindings[0].HostPort)
	if err != nil {
		_ = m.removeContainer(context.Background(), &WarmContainer{ID: resp.ID, Name: containerName})
		return nil, &apierror.Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ServiceException",
			Message:    "invalid runtime host port: " + err.Error(),
		}
	}

	warm := &WarmContainer{
		ID:        resp.ID,
		Name:      containerName,
		Function:  spec.FunctionName,
		HostPort:  hostPort,
		LastUsed:  time.Now(),
		CreatedAt: time.Now(),
	}
	if err := m.waitReady(ctx, warm); err != nil {
		_ = m.removeContainer(context.Background(), warm)
		return nil, err
	}

	m.logger.Info("lambda runtime started", "function", spec.FunctionName, "container_id", resp.ID, "host_port", hostPort)
	return warm, nil
}

func (m *Manager) waitReady(ctx context.Context, warm *WarmContainer) error {
	deadline := time.Now().Add(20 * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", warm.HostPort)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := m.httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &apierror.Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "ServiceException",
		Message:    "lambda runtime did not become ready",
	}
}

func (m *Manager) isHealthy(ctx context.Context, warm *WarmContainer) (bool, error) {
	inspect, err := m.client.ContainerInspect(ctx, warm.ID)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !inspect.State.Running {
		return false, nil
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", warm.HostPort), nil)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false, nil
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

func (m *Manager) invokeHTTP(ctx context.Context, warm *WarmContainer, spec lambdasvc.FunctionSpec, payload []byte) (lambdasvc.InvokeResult, error) {
	requestPayload, err := json.Marshal(map[string]string{
		"payload_b64": base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		return lambdasvc.InvokeResult{}, err
	}

	timeout := time.Duration(spec.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	invokeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(invokeCtx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/invoke", warm.HostPort), bytes.NewReader(requestPayload))
	if err != nil {
		return lambdasvc.InvokeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		if errors.Is(invokeCtx.Err(), context.DeadlineExceeded) {
			_ = m.removeContainer(context.Background(), warm)
			return lambdasvc.InvokeResult{
				Payload:       timeoutPayload(spec.Timeout),
				FunctionError: "Unhandled",
			}, nil
		}
		return lambdasvc.InvokeResult{}, &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "ServiceException",
			Message:    "lambda runtime exited unexpectedly",
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(resp.Body)
		return lambdasvc.InvokeResult{}, &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "ServiceException",
			Message:    "lambda runtime returned unexpected status: " + strings.TrimSpace(body.String()),
		}
	}

	var envelope struct {
		OK            bool   `json:"ok"`
		PayloadBase64 string `json:"payload_base64"`
		Logs          string `json:"logs"`
		FunctionError string `json:"function_error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return lambdasvc.InvokeResult{}, &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "ServiceException",
			Message:    "lambda runtime returned invalid payload",
		}
	}

	resultBytes, err := base64.StdEncoding.DecodeString(envelope.PayloadBase64)
	if err != nil {
		return lambdasvc.InvokeResult{}, &apierror.Error{
			StatusCode: http.StatusInternalServerError,
			Code:       "ServiceException",
			Message:    "lambda runtime returned invalid base64 payload",
		}
	}

	return lambdasvc.InvokeResult{
		Payload:       resultBytes,
		Logs:          envelope.Logs,
		FunctionError: envelope.FunctionError,
	}, nil
}

func (m *Manager) removeContainer(ctx context.Context, warm *WarmContainer) error {
	if warm == nil || warm.ID == "" {
		return nil
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = m.client.ContainerStop(stopCtx, warm.ID, container.StopOptions{Timeout: intPtr(1)})
	err := m.client.ContainerRemove(stopCtx, warm.ID, container.RemoveOptions{Force: true})
	if err != nil && !dockerclient.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (m *Manager) removeNamedContainer(ctx context.Context, name string) error {
	containers, err := m.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}
	for _, item := range containers {
		for _, candidate := range item.Names {
			if strings.TrimPrefix(candidate, "/") == name {
				return m.removeContainer(ctx, &WarmContainer{ID: item.ID, Name: name})
			}
		}
	}
	return nil
}

func runtimeImageFor(runtime string) (string, error) {
	switch runtime {
	case "python3.11":
		return runtimeImagePython311, nil
	default:
		return "", &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "lambda execution for runtime is not supported yet: " + runtime,
		}
	}
}

func timeoutPayload(seconds int) []byte {
	timeoutSeconds := seconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 3
	}
	return []byte(fmt.Sprintf(`{"errorMessage":"Task timed out after %d.00 seconds"}`, timeoutSeconds))
}

func containerNameFor(functionName string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, functionName)
	if len(name) > 48 {
		name = name[:48]
	}
	return "stratus-lambda-" + name
}

func intPtr(v int) *int {
	return &v
}

func defaultDockerHost() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	socketPath := filepath.Join(home, ".docker", "run", "docker.sock")
	if _, err := os.Stat(socketPath); err == nil {
		return "unix://" + socketPath
	}
	return ""
}

const runtimeServerScript = `#!/usr/bin/env python3
import argparse
import asyncio
import base64
import contextlib
import inspect
import importlib
import io
import json
import sys
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

def parse_event(payload_b64):
    raw = base64.b64decode(payload_b64.encode("utf-8")) if payload_b64 else b""
    if not raw:
        return None
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:
        try:
            return raw.decode("utf-8")
        except Exception:
            return None

def payload_bytes(value):
    if value is None:
        return b"null"
    if isinstance(value, (bytes, bytearray)):
        return bytes(value)
    if isinstance(value, str):
        return value.encode("utf-8")
    return json.dumps(value).encode("utf-8")

class LambdaContext:
    def __init__(self, function_name):
        self.function_name = function_name
        self.function_version = "$LATEST"
        self.memory_limit_in_mb = 128
        self.aws_request_id = "stratus"
        self.invoked_function_arn = function_name
        self.log_group_name = "/aws/lambda/" + function_name
        self.log_stream_name = "stratus"

class HandlerServer(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        if self.path != "/invoke":
            self.send_response(404)
            self.end_headers()
            return
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        request = json.loads(raw.decode("utf-8")) if raw else {}
        stdout_buf = io.StringIO()
        stderr_buf = io.StringIO()
        response = {}
        with contextlib.redirect_stdout(stdout_buf), contextlib.redirect_stderr(stderr_buf):
            try:
                event = parse_event(request.get("payload_b64", ""))
                result = self.server.lambda_handler(event, LambdaContext(self.server.function_name))
                if inspect.isawaitable(result):
                    result = asyncio.run(result)
                response = {
                    "ok": True,
                    "payload_base64": base64.b64encode(payload_bytes(result)).decode("utf-8"),
                    "logs": stdout_buf.getvalue() + stderr_buf.getvalue(),
                    "function_error": "",
                }
            except BaseException as exc:
                error_payload = json.dumps({
                    "errorMessage": str(exc),
                    "errorType": exc.__class__.__name__,
                    "stackTrace": traceback.format_exc().splitlines(),
                }).encode("utf-8")
                response = {
                    "ok": False,
                    "payload_base64": base64.b64encode(error_payload).decode("utf-8"),
                    "logs": stdout_buf.getvalue() + stderr_buf.getvalue() + traceback.format_exc(),
                    "function_error": "Handled",
                }
        encoded = json.dumps(response).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, format, *args):
        return

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--handler", required=True)
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--function-name", required=True)
    args = parser.parse_args()
    module_name, attr_name = args.handler.rsplit(".", 1)
    module = importlib.import_module(module_name)
    handler = getattr(module, attr_name)
    server = ThreadingHTTPServer(("0.0.0.0", args.port), HandlerServer)
    server.lambda_handler = handler
    server.function_name = args.function_name
    server.serve_forever()

if __name__ == "__main__":
    main()
`
