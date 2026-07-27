//go:build !windows

package historyimport

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Importers may spawn helpers. Giving each invocation its own process group
// makes timeout/cancellation release inherited stdout/stderr pipes as well as
// stopping the top-level Node process.
func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
