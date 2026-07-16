// Package app implements the top-level Tandem command dispatch.
package app

import (
	"fmt"
	"io"

	"github.com/aiguy110/tandem/internal/buildinfo"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/daemon"
)

const usage = "usage: tandem <version|daemon|debug config>"

// Run executes the requested Tandem subcommand and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
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
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "daemon":
		fmt.Fprintln(stderr, daemon.ErrNotReady)
		return 1
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}
