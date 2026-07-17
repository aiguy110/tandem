package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
	proc "github.com/aiguy110/tandem/internal/process"
)

// userShellShutdownTimeout bounds how long we wait for the shell to exit on
// forced disposal before it is killed.
const userShellShutdownTimeout = 2 * time.Second

// OpenUserShell lazily spawns the user's default shell in cwd for the Terminal
// tab, streaming its bytes as shell_pty events. It is idempotent: if a shell is
// already running it just applies the requested size. The shell is independent
// of the agent adapter, so it is unaffected by ACP↔CLI control swaps and keeps
// running while the agent works.
func (s *Session) OpenUserShell(cwd string, cols, rows uint16) error {
	s.shellMu.Lock()
	defer s.shellMu.Unlock()
	if s.shellRunning && s.shell != nil {
		if cols > 0 && rows > 0 {
			_ = s.shell.Resize(proc.Size{Rows: rows, Cols: cols})
		}
		return nil
	}
	if cols == 0 {
		cols = 100
	}
	if rows == 0 {
		rows = 30
	}
	ctx, cancel := context.WithCancel(context.Background())
	pty, err := proc.StartPTY(ctx, proc.Spec{
		Argv: []string{defaultUserShell()},
		Dir:  cwd,
		Env:  os.Environ(),
	}, proc.Size{Rows: rows, Cols: cols}, proc.Options{ShutdownTimeout: userShellShutdownTimeout})
	if err != nil {
		cancel()
		return fmt.Errorf("user shell: spawn: %w", err)
	}
	done := make(chan struct{})
	s.shell = pty
	s.shellCancel = cancel
	s.shellDone = done
	s.shellRunning = true
	go s.pumpUserShell(pty, cancel, done)
	return nil
}

// pumpUserShell copies shell output into shell_pty events and, on exit, records
// a shell_exit event so the pane can offer a restart. Bytes are forwarded
// verbatim (no UTF-8 decoding) so control sequences survive intact.
func (s *Session) pumpUserShell(pty *proc.PTY, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32*1024)
	var readErr error
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			s.emit(eventlog.ShellPTY(buf[:n]))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	exit, waitErr := pty.Wait(context.Background())
	_ = pty.Dispose(context.Background())
	cancel()

	s.shellMu.Lock()
	// A concurrent CloseUserShell may already have swapped in a new state.
	if s.shell == pty {
		s.shell = nil
		s.shellCancel = nil
		s.shellDone = nil
		s.shellRunning = false
	}
	s.shellMu.Unlock()

	s.emit(eventlog.ShellExit(userShellExitMessage(exit, waitErr, readErr)))
}

// UserShellInput writes bytes to the running shell.
func (s *Session) UserShellInput(data []byte) error {
	s.shellMu.Lock()
	pty := s.shell
	running := s.shellRunning
	s.shellMu.Unlock()
	if !running || pty == nil {
		return errors.New("user shell is not running")
	}
	for len(data) > 0 {
		n, err := pty.Write(data)
		if err != nil {
			return fmt.Errorf("user shell: input: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// UserShellResize applies a new terminal size to the running shell.
func (s *Session) UserShellResize(cols, rows uint16) error {
	s.shellMu.Lock()
	pty := s.shell
	running := s.shellRunning
	s.shellMu.Unlock()
	if !running || pty == nil {
		return errors.New("user shell is not running")
	}
	if err := pty.Resize(proc.Size{Rows: rows, Cols: cols}); err != nil {
		return fmt.Errorf("user shell: resize: %w", err)
	}
	return nil
}

// CloseUserShell tears down the shell if one is running. It is safe to call
// when no shell exists (e.g. on session dispose).
func (s *Session) CloseUserShell() {
	s.shellMu.Lock()
	pty := s.shell
	cancel := s.shellCancel
	done := s.shellDone
	s.shell = nil
	s.shellCancel = nil
	s.shellDone = nil
	s.shellRunning = false
	s.shellMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if pty != nil {
		_ = pty.Dispose(context.Background())
	}
	// Wait for the pump goroutine to emit its final shell_exit and exit, so no
	// event is appended after the session (and its event store) is torn down.
	if done != nil {
		<-done
	}
}

func userShellExitMessage(exit proc.Exit, waitErr, readErr error) string {
	if readErr != nil {
		return fmt.Sprintf("shell error: %v", readErr)
	}
	if waitErr != nil {
		return fmt.Sprintf("shell error: %v", waitErr)
	}
	if exit.Signal != "" {
		return fmt.Sprintf("exited (signal %s)", exit.Signal)
	}
	return fmt.Sprintf("exited (code %d)", exit.Code)
}

func defaultUserShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}
