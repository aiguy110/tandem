//go:build unix

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateSignal() os.Signal { return syscall.SIGTERM }
func killSignal() os.Signal      { return syscall.SIGKILL }

func (p *Process) signalGroup(signal os.Signal) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return p.signalOwnedGroup(signal)
}

func (p *Process) signalOwnedGroup(signal os.Signal) error {
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return errors.New("process: invalid Unix signal")
	}
	err := syscall.Kill(-p.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (p *Process) cleanupExitedGroup() error {
	return p.signalOwnedGroup(killSignal())
}

func decodeExit(err error, state *os.ProcessState) Exit {
	exit := Exit{Code: -1, Err: err}
	if state == nil {
		return exit
	}
	exit.Code = state.ExitCode()
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		exit.Signal = status.Signal().String()
		exit.SignalNumber = int(status.Signal())
	}
	return exit
}
