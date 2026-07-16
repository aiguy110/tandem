package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func shell(t *testing.T, script string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group semantics")
	}
	return []string{"/bin/sh", "-c", script}
}

func wait(t *testing.T, p *Process) Exit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return exit
}

func TestNormalExitEnvironmentCWDAndIO(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	p, err := Start(context.Background(), Spec{
		Argv: shell(t, `IFS= read -r line; printf '%s|%s|%s' "$TANDEM_TEST" "$PWD" "$line"; printf err >&2`),
		Dir:  dir, Env: append(os.Environ(), "TANDEM_TEST=present"),
		Stdin: strings.NewReader("from-stdin\n"), Stdout: &stdout, Stderr: &stderr,
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	exit := wait(t, p)
	if exit.Code != 0 || exit.Err != nil {
		t.Fatalf("exit = %+v", exit)
	}
	if got, want := stdout.String(), "present|"+dir+"|from-stdin"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.String() != "err" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stdout lockedBuffer
	p, err := Start(ctx, Spec{Argv: shell(t, `sleep 60 & echo $!; wait`), Stdout: &stdout}, Options{ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "\n") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	exit := wait(t, p)
	if exit.Code == 0 {
		t.Fatalf("cancelled child exited successfully: %+v", exit)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil {
		t.Fatalf("descendant pid output %q: %v", stdout.String(), err)
	}
	assertProcessGone(t, childPID)
}

func TestShutdownForcesKillAndIsIdempotent(t *testing.T) {
	var stdout lockedBuffer
	p, err := Start(context.Background(), Spec{
		Argv:   shell(t, `trap '' TERM; echo ready; while :; do sleep 1; done`),
		Stdout: &stdout,
	}, Options{ShutdownTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "ready") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Fatal("child did not install TERM handler")
	}
	started := time.Now()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded shutdown took %v", elapsed)
	}
	if exit := wait(t, p); exit.Signal != syscall.SIGKILL.String() {
		t.Fatalf("expected forced SIGKILL exit, got %+v", exit)
	}
	for i := 0; i < 3; i++ {
		if err := p.Shutdown(context.Background()); err != nil {
			t.Fatalf("repeat shutdown: %v", err)
		}
		if err := p.Kill(); err != nil {
			t.Fatalf("repeat kill: %v", err)
		}
	}
}

func TestParentShutdownDoesNotLeaveChild(t *testing.T) {
	var stdout lockedBuffer
	p, err := Start(context.Background(), Spec{Argv: shell(t, `sleep 60 & echo $!; wait`), Stdout: &stdout}, Options{ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "\n") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = wait(t, p)
	childPID, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil {
		t.Fatal(err)
	}
	assertProcessGone(t, childPID)
}

func TestShutdownKillsDescendantAfterLeaderExits(t *testing.T) {
	var stdout lockedBuffer
	p, err := Start(context.Background(), Spec{
		Argv:   shell(t, `trap 'exit 0' TERM; (trap '' TERM; sleep 60) & echo $!; wait`),
		Stdout: &stdout,
	}, Options{ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "\n") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(stdout.String()))
	if err != nil {
		t.Fatalf("descendant pid output %q: %v", stdout.String(), err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = wait(t, p)
	assertProcessGone(t, childPID)
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil || errors.Is(proc.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists", pid)
}
