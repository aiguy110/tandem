//go:build windows

package process

import (
	"errors"
	"os"
	"os/exec"
)

func configureProcessGroup(cmd *exec.Cmd) {}
func terminateSignal() os.Signal          { return os.Kill }
func killSignal() os.Signal               { return os.Kill }

func (p *Process) signalGroup(signal os.Signal) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	err := p.cmd.Process.Signal(signal)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (p *Process) cleanupExitedGroup() error { return nil }

func decodeExit(err error, state *os.ProcessState) Exit {
	exit := Exit{Code: -1, Err: err}
	if state != nil {
		exit.Code = state.ExitCode()
	}
	return exit
}
