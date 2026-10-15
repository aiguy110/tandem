//go:build windows

package historyimport

import "os/exec"

func configureCommand(cmd *exec.Cmd) {
	// CommandContext's default Cancel kills the directly launched Bun process.
	// Windows job-object containment can be added without changing the protocol.
}
