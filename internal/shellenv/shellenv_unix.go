//go:build unix

package shellenv

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func configureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// passwdShell is the login shell recorded for this uid in /etc/passwd.
func passwdShell() string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	uid := strconv.Itoa(os.Getuid())
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) >= 7 && fields[2] == uid {
			return fields[6]
		}
	}
	return ""
}
