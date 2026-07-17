//go:build unix

package session

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
)

// TestUserShellEchoAndExit drives the escape-hatch shell end to end: it opens a
// shell, types a command, and confirms the output arrives as shell_pty events
// and a shell_exit is recorded when the shell ends.
func TestUserShellEchoAndExit(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s, _, _ := testSession(t)

	var mu sync.Mutex
	var out strings.Builder
	exited := make(chan string, 1)
	unsub := s.OnEvent(func(le eventlog.LoggedEvent) {
		switch le.Event.Kind {
		case "shell_pty":
			mu.Lock()
			out.Write(le.Event.Data)
			mu.Unlock()
		case "shell_exit":
			select {
			case exited <- string(le.Event.Payload):
			default:
			}
		}
	})
	defer unsub()

	if err := s.OpenUserShell(t.TempDir(), 80, 24); err != nil {
		t.Fatalf("open user shell: %v", err)
	}
	// Second call must be idempotent (resize, not a second process).
	if err := s.OpenUserShell(t.TempDir(), 100, 30); err != nil {
		t.Fatalf("re-open user shell: %v", err)
	}

	if err := s.UserShellInput([]byte("echo tandem-shell-ok\n")); err != nil {
		t.Fatalf("input: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := out.String()
		mu.Unlock()
		if strings.Contains(got, "tandem-shell-ok") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := out.String()
	mu.Unlock()
	if !strings.Contains(got, "tandem-shell-ok") {
		t.Fatalf("shell output missing echo; got %q", got)
	}

	if err := s.UserShellInput([]byte("exit\n")); err != nil {
		t.Fatalf("exit input: %v", err)
	}
	select {
	case msg := <-exited:
		if !strings.Contains(msg, "shell_exit") {
			t.Fatalf("unexpected shell_exit payload: %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell_exit event not received after exit")
	}

	// After exit, input must be rejected (shell no longer running).
	if err := s.UserShellInput([]byte("noop\n")); err == nil {
		t.Fatal("expected error writing to exited shell")
	}
}

// TestUserShellCloseStopsProcess verifies CloseUserShell tears the shell down
// and that a subsequent OpenUserShell starts a fresh one.
func TestUserShellCloseStopsProcess(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	s, _, _ := testSession(t)

	if err := s.OpenUserShell(t.TempDir(), 80, 24); err != nil {
		t.Fatalf("open: %v", err)
	}
	s.CloseUserShell()
	if err := s.UserShellInput([]byte("x")); err == nil {
		t.Fatal("expected error after CloseUserShell")
	}
	// A fresh open should succeed.
	if err := s.OpenUserShell(t.TempDir(), 80, 24); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := s.UserShellInput([]byte("echo hi\n")); err != nil {
		t.Fatalf("input after reopen: %v", err)
	}
}
