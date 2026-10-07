//go:build !unix

package shellenv

import (
	"os/exec"
)

func configureDetached(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func passwdShell() string { return "" }
