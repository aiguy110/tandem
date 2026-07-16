// Package app implements the top-level Tandem command dispatch.
package app

import (
	"fmt"
	"io"

	"github.com/aiguy110/tandem/internal/buildinfo"
	"github.com/aiguy110/tandem/internal/daemon"
)

const usage = "usage: tandem <version|daemon>"

// Run executes the requested Tandem subcommand and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
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
