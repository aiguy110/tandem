package process

import (
	"context"
	"errors"
	"os"
	"sync"
)

// ErrPTYUnsupported is returned on platforms without a supported PTY backend.
// Tandem's initial release matrix intentionally excludes Windows.
var ErrPTYUnsupported = errors.New("process: pseudoterminals are unsupported on this platform")

// Size is a terminal size in character cells.
type Size struct {
	Rows uint16
	Cols uint16
}

// PTY owns a pseudoterminal master and the child session attached to it.
type PTY struct {
	master *os.File
	proc   *Process

	closeOnce sync.Once
	closeErr  error
}

func (p *PTY) Read(data []byte) (int, error) {
	n, err := p.master.Read(data)
	return n, normalizePTYReadError(err)
}

func (p *PTY) Write(data []byte) (int, error) { return p.master.Write(data) }

// Resize changes the terminal dimensions immediately.
func (p *PTY) Resize(size Size) error {
	if size.Rows == 0 || size.Cols == 0 {
		return errors.New("process: PTY rows and columns must be nonzero")
	}
	return resizePTY(p.master, size)
}

func (p *PTY) PID() int                               { return p.proc.PID() }
func (p *PTY) Done() <-chan struct{}                  { return p.proc.Done() }
func (p *PTY) Wait(ctx context.Context) (Exit, error) { return p.proc.Wait(ctx) }

// Kill immediately kills the child session. It does not close the master, so
// callers may drain output produced immediately before the process stopped.
func (p *PTY) Kill() error { return p.proc.Kill() }

// Dispose stops the child session and closes the PTY master. It is idempotent.
func (p *PTY) Dispose(ctx context.Context) error {
	p.closeOnce.Do(func() {
		shutdownErr := p.proc.Shutdown(ctx)
		closeErr := p.master.Close()
		p.closeErr = errors.Join(shutdownErr, closeErr)
	})
	return p.closeErr
}
