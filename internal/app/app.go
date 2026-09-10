// Package app implements the top-level Tandem command dispatch.
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aiguy110/tandem/internal/automationmcp"
	"github.com/aiguy110/tandem/internal/buildinfo"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/controlmcp"
	"github.com/aiguy110/tandem/internal/daemon"
	"github.com/aiguy110/tandem/internal/setup"
	"github.com/aiguy110/tandem/internal/updater"
	"github.com/mattn/go-isatty"
)

const usage = "usage: tandem [setup [--agent|--complete]|mcp add [--project] [--transport stdio|http] NAME COMMAND-or-URL [ARGS...]|update|version|debug config]"

var stdinIsTerminal = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

// Run executes the requested Tandem subcommand and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	// mcp-scripts is an internal stdio MCP subprocess declared for ACP agents.
	if len(args) == 1 && args[0] == "mcp-scripts" {
		err := automationmcp.Run(context.Background(), os.Stdin, stdout, automationmcp.Config{
			ControlURL: os.Getenv("TANDEM_CONTROL_URL"), Token: os.Getenv("TANDEM_TOKEN"), AgentID: os.Getenv("TANDEM_AGENT_ID"), WorkspaceCWD: os.Getenv("TANDEM_WORKSPACE_CWD"),
		})
		if err != nil {
			fmt.Fprintf(stderr, "run mcp-scripts: %v\n", err)
			return 1
		}
		return 0
	}
	// mcp-control is deliberately omitted from usage: ACP agents spawn this
	// internal stdio MCP subprocess from Tandem's session/new declaration.
	if len(args) == 1 && args[0] == "mcp-control" {
		err := controlmcp.Run(context.Background(), os.Stdin, stdout, controlmcp.Config{
			ControlURL: os.Getenv("TANDEM_CONTROL_URL"), Token: os.Getenv("TANDEM_TOKEN"), AgentID: os.Getenv("TANDEM_AGENT_ID"),
		})
		if err != nil {
			fmt.Fprintf(stderr, "run mcp-control: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) == 2 && args[0] == "debug" && args[1] == "config" {
		resolved, err := config.Load()
		if err != nil {
			fmt.Fprintf(stderr, "load config: %v\n", err)
			return 1
		}
		data, err := config.DebugJSON(resolved)
		if err != nil {
			fmt.Fprintf(stderr, "encode config: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	if len(args) >= 2 && args[0] == "mcp" && args[1] == "add" {
		return addMCP(args[2:], stdout, stderr)
	}
	if len(args) == 0 {
		return runDaemon(stdout, stderr)
	}
	if len(args) == 2 && args[0] == "setup" && args[1] == "--complete" {
		if err := setup.Complete(stdout); err != nil {
			fmt.Fprintf(stderr, "setup: %v\n", err)
			return 1
		}
		return 0
	}
	// --needs-review is an internal status probe used immediately after a
	// self-update. It must be run by the replacement binary because only it
	// knows its configuration compatibility version.
	if len(args) == 2 && args[0] == "setup" && args[1] == "--needs-review" {
		home, err := setupHome()
		if err != nil {
			fmt.Fprintf(stderr, "setup: resolve TANDEM_HOME: %v\n", err)
			return 1
		}
		needsReview, err := setup.ConfigNeedsReview(home)
		if err != nil {
			fmt.Fprintf(stderr, "setup: check configuration compatibility: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, needsReview)
		return 0
	}
	if len(args) == 2 && args[0] == "setup" && args[1] == "--agent" {
		if !stdinIsTerminal() {
			fmt.Fprintln(stderr, "tandem setup --agent requires an interactive terminal")
			return 2
		}
		if err := setup.RunClaudeGuide(context.Background(), stdout); err != nil {
			fmt.Fprintf(stderr, "setup: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch args[0] {
	case "setup":
		if !stdinIsTerminal() {
			fmt.Fprintln(stderr, "tandem setup requires an interactive terminal")
			return 2
		}
		if err := setup.Run(context.Background(), os.Stdin, stdout); err != nil {
			fmt.Fprintf(stderr, "setup: %v\n", err)
			return 1
		}
		return 0
	case "version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "update":
		updated, err := updater.UpdateWithResult(context.Background(), updater.Options{CurrentVersion: buildinfo.Version, Log: stdout})
		if err != nil {
			fmt.Fprintf(stderr, "update tandem: %v\n", err)
			return 1
		}
		if !updated {
			return 0
		}
		needsReview, err := updater.UpdatedBinaryNeedsReview("")
		if err != nil {
			fmt.Fprintf(stderr, "tandem: updated successfully, but could not check whether setup is needed: %v\n", err)
			return 0
		}
		if needsReview {
			fmt.Fprintln(stdout, "tandem: run 'tandem setup' to review configuration changes after an update.")
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

func addMCP(args []string, stdout, stderr io.Writer) int {
	project := false
	transport := "stdio"
	for len(args) > 0 {
		switch args[0] {
		case "--project":
			project = true
			args = args[1:]
		case "--transport":
			if len(args) < 2 || (args[1] != "stdio" && args[1] != "http") {
				fmt.Fprintln(stderr, "usage: tandem mcp add [--project] [--transport stdio|http] NAME COMMAND-or-URL [ARGS...]")
				return 2
			}
			transport, args = args[1], args[2:]
		default:
			goto flagsDone
		}
	}
flagsDone:
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: tandem mcp add [--project] [--transport stdio|http] NAME COMMAND-or-URL [ARGS...]")
		return 2
	}
	home, err := setupHome()
	if err != nil {
		fmt.Fprintf(stderr, "mcp: resolve TANDEM_HOME: %v\n", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "mcp: working directory: %v\n", err)
		return 1
	}
	if project {
		cwd = projectRoot(cwd)
	}
	server := config.MCPServer{Command: args[1], Args: args[2:]}
	if transport == "http" {
		if len(args) != 2 {
			fmt.Fprintln(stderr, "mcp: HTTP transport accepts exactly NAME and URL")
			return 2
		}
		server = config.MCPServer{Type: "http", URL: args[1]}
	}
	path, err := config.AddMCPServer(home, cwd, args[0], server, project)
	if err != nil {
		fmt.Fprintf(stderr, "mcp: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "added MCP server %q to %s; new Tandem agent sessions will use it immediately\n", args[0], path)
	return 0
}

// projectRoot finds the nearest Git worktree/repository root. A .git entry may
// be either a directory or a git-worktree pointer file.
func projectRoot(cwd string) string {
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
	}
}

func setupHome() (string, error) {
	if home := os.Getenv("TANDEM_HOME"); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tandem"), nil
}

func runDaemon(stdout, stderr io.Writer) int {
	if err := daemon.Run(stdout); err != nil {
		fmt.Fprintf(stderr, "run daemon: %v\n", err)
		return 1
	}
	return 0
}
