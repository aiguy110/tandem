// Package app implements the top-level Tandem command dispatch.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aiguy110/tandem/internal/buildinfo"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/controlmcp"
	"github.com/aiguy110/tandem/internal/daemon"
	"github.com/aiguy110/tandem/internal/setup"
	"github.com/aiguy110/tandem/internal/updater"
	"github.com/mattn/go-isatty"
)

const usage = "usage: tandem <version|daemon|debug config>"

// Run executes the requested Tandem subcommand and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
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
	if len(args) == 0 {
		if isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd()) {
			if err := setup.Run(context.Background(), os.Stdin, stdout); err != nil {
				fmt.Fprintf(stderr, "setup: %v\n", err)
				return 1
			}
			return 0
		}
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "daemon":
		if err := updater.CheckAtStartup(context.Background(), updater.Options{CurrentVersion: buildinfo.Version, Log: stderr}); err != nil {
			fmt.Fprintf(stderr, "tandem: update check failed: %v\n", err)
			// The binary was replaced but the restart failed; don't run stale code.
			if errors.Is(err, updater.ErrRestartRequired) {
				return 1
			}
		}
		if err := daemon.Run(stdout); err != nil {
			fmt.Fprintf(stderr, "run daemon: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}
