// Package process owns cancellable child processes and their process groups.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Spec describes a child without invoking a shell. Argv must contain the
// executable at index zero. Env is the complete child environment; nil inherits
// the parent environment, matching os/exec.
type Spec struct {
	Argv   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Options controls child shutdown.
type Options struct {
	// ShutdownTimeout is the grace period between a graceful process-group
	// termination and a forced kill.
	ShutdownTimeout time.Duration
}

const defaultShutdownTimeout = 2 * time.Second

// Exit records the observable child exit disposition.
type Exit struct {
	Code   int
	Signal string
	Err    error
}

// Process owns one child and the process group created for it.
type Process struct {
	cmd     *exec.Cmd
	timeout time.Duration
	done    chan struct{}

	mu   sync.Mutex
	exit Exit

	shutdownOnce sync.Once
	cleanupOnce  sync.Once
	cleanupErr   error
}

func ownStarted(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) *Process {
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	p := &Process{cmd: cmd, timeout: timeout, done: make(chan struct{})}
	go p.reap()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Shutdown(context.Background())
		case <-p.done:
		}
	}()
	return p
}

// Start launches a child in a new process group. Cancelling ctx initiates a
// bounded shutdown of the whole group, including grandchildren.
func Start(ctx context.Context, spec Spec, opts Options) (*Process, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("process: argv must contain an executable")
	}
	if ctx == nil {
		return nil, errors.New("process: nil context")
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	configureProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("process: start %q: %w", spec.Argv[0], err)
	}

	return ownStarted(ctx, cmd, opts.ShutdownTimeout), nil
}

func (p *Process) reap() {
	err := p.cmd.Wait()
	exit := decodeExit(err, p.cmd.ProcessState)
	p.mu.Lock()
	p.exit = exit
	close(p.done)
	p.mu.Unlock()
}

// PID returns the process ID originally assigned to the child.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Done closes after the child has been reaped.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait waits for the child and returns the same result on every call.
func (p *Process) Wait(ctx context.Context) (Exit, error) {
	if ctx == nil {
		return Exit{}, errors.New("process: nil context")
	}
	select {
	case <-p.done:
		p.mu.Lock()
		exit := p.exit
		p.mu.Unlock()
		return exit, nil
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	}
}

// Shutdown gracefully terminates the process group, then forcibly kills it if
// it does not exit before the configured bound. It is safe to call repeatedly.
func (p *Process) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("process: nil context")
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	p.shutdownOnce.Do(func() { _ = p.signalGroup(terminateSignal()) })

	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		// The group leader can exit before a descendant that ignored TERM.
		// This final group signal is limited to a shutdown that began while the
		// owned leader was live; repeated cleanup takes the fast path above.
		p.cleanupOnce.Do(func() { p.cleanupErr = p.cleanupExitedGroup() })
		return p.cleanupErr
	case <-ctx.Done():
		_ = p.signalGroup(killSignal())
		return ctx.Err()
	case <-timer.C:
		if err := p.signalGroup(killSignal()); err != nil {
			return fmt.Errorf("process: force kill: %w", err)
		}
	}

	forceTimer := time.NewTimer(p.timeout)
	defer forceTimer.Stop()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-forceTimer.C:
		return errors.New("process: child was not reaped after forced kill")
	}
}

// Kill immediately kills the owned process group. It is idempotent.
func (p *Process) Kill() error { return p.signalGroup(killSignal()) }
