package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/stratus/internal/ci"
	"github.com/stratus/internal/control/client"
	"github.com/stratus/internal/mcp"
)

// MCPDeps are the collaborators `stratus mcp` needs; tests substitute them.
type MCPDeps struct {
	Start   ci.ServerStarter
	Ready   ci.ReadyWaiter
	TempDir func() (string, error)
	Remove  func(string) error
	Version string
}

const mcpUsage = `usage: stratus mcp [flags]

Runs a Model Context Protocol server on stdin/stdout so coding agents can
check health, reset state, save and restore snapshots, run chaos experiments
and diagnose failures. For example, with Claude Code:

  claude mcp add stratus -- stratus mcp --start

Flags:
`

// RunMCP serves MCP until stdin closes or ctx is cancelled.
func RunMCP(ctx context.Context, args []string, deps MCPDeps, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stratus mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, mcpUsage)
		fs.PrintDefaults()
	}
	defaultEndpoint := os.Getenv(EndpointEnv)
	if defaultEndpoint == "" {
		defaultEndpoint = client.DefaultEndpoint
	}
	endpoint := fs.String("endpoint", defaultEndpoint, "Stratus to control when not using --start (env "+EndpointEnv+")")
	start := fs.Bool("start", false, "Start a private Stratus for this session on a free port")
	dataDir := fs.String("data-dir", "", "With --start: keep state in this directory (default: a temporary one, deleted on exit)")
	seed := fs.String("seed", "", "With --start: snapshot directory to start from")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}

	if *start {
		dir := *dataDir
		if dir == "" {
			tmp, err := deps.TempDir()
			if err != nil {
				fmt.Fprintf(stderr, "stratus mcp: %v\n", err)
				return 1
			}
			dir = tmp
			defer deps.Remove(tmp)
		}
		server, err := deps.Start(ctx, ci.ServerConfig{DataDir: dir, Seed: *seed, LogLevel: "error"})
		if err != nil {
			fmt.Fprintf(stderr, "stratus mcp: start stratus: %v\n", err)
			return 1
		}
		defer server.Stop()
		if err := deps.Ready(ctx, server.Endpoint()); err != nil {
			fmt.Fprintf(stderr, "stratus mcp: %v\n", err)
			return 1
		}
		*endpoint = server.Endpoint()
		fmt.Fprintf(stderr, "stratus mcp: started Stratus at %s\n", *endpoint)
	}

	c, err := client.New(*endpoint, nil)
	if err != nil {
		fmt.Fprintf(stderr, "stratus mcp: %v\n", err)
		return 2
	}
	srv, err := mcp.NewServer("stratus", deps.Version, mcp.Instructions, mcp.Tools(c)...)
	if err != nil {
		fmt.Fprintf(stderr, "stratus mcp: %v\n", err)
		return 1
	}
	if err := srv.Serve(ctx, stdin, stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "stratus mcp: %v\n", err)
		return 1
	}
	return 0
}
