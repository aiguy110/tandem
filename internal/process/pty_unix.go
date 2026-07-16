//go:build unix

package process

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	creackpty "github.com/creack/pty"
)

// StartPTY launches a child in a fresh session attached to a real Unix
// pseudoterminal. Linux and macOS are supported by the selected PTY backend.
func StartPTY(ctx context.Context, spec Spec, size Size, opts Options) (*PTY, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("process: argv must contain an executable")
	}
	if ctx == nil {
		return nil, errors.New("process: nil context")
	}
	if size.Rows == 0 || size.Cols == 0 {
		return nil, errors.New("process: PTY rows and columns must be nonzero")
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	master, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{Rows: size.Rows, Cols: size.Cols})
	if err != nil {
		return nil, err
	}
	return &PTY{master: master, proc: ownStarted(ctx, cmd, opts.ShutdownTimeout)}, nil
}

func resizePTY(master *os.File, size Size) error {
	return creackpty.Setsize(master, &creackpty.Winsize{Rows: size.Rows, Cols: size.Cols})
}

func normalizePTYReadError(err error) error {
	// Linux returns EIO when the slave side has closed; consumers expect EOF.
	if errors.Is(err, syscall.EIO) {
		return io.EOF
	}
	return err
}
