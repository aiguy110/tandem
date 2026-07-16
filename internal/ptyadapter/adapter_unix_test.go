//go:build unix

package ptyadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
)

func collect(t *testing.T, a *Adapter) []eventlog.Event {
	t.Helper()
	var events []eventlog.Event
	for event := range a.Events() {
		events = append(events, event)
	}
	return events
}

func eventStatus(t *testing.T, event eventlog.Event) string {
	t.Helper()
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Status
}

func TestPTYSmokeBinaryOutputAndCleanExit(t *testing.T) {
	a := New("shell-smoke")
	err := a.Spawn(context.Background(), SpawnOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", `stty raw -echo; printf 'pty line 1\npty line 2\n\000\001\377'`},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, a)
	if len(events) < 3 || events[0].Kind != "status" || eventStatus(t, events[0]) != "working" {
		t.Fatalf("initial events = %+v", events)
	}
	var output []byte
	for _, event := range events {
		if event.Kind == "raw_pty" {
			output = append(output, event.Data...)
		}
	}
	if want := []byte("pty line 1\npty line 2\n\x00\x01\xff"); !bytes.Equal(output, want) {
		t.Fatalf("output = %v, want %v", output, want)
	}
	if last := events[len(events)-1]; last.Kind != "status" || eventStatus(t, last) != "idle" {
		t.Fatalf("last event = %+v", last)
	}
	exit, err := a.Wait(context.Background())
	if err != nil || exit.Code != 0 {
		t.Fatalf("exit = %+v, %v", exit, err)
	}
	if err := a.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPTYInputAndResize(t *testing.T) {
	a := New("interactive")
	if err := a.Spawn(context.Background(), SpawnOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", `stty raw -echo; dd bs=1 count=3 of="$TANDEM_BYTES" 2>/dev/null; stty size; od -An -tu1 "$TANDEM_BYTES"`},
		Env:     map[string]string{"TANDEM_BYTES": t.TempDir() + "/bytes"},
		Rows:    24, Cols: 80,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Resize(91, 37); err != nil {
		t.Fatal(err)
	}
	if err := a.SendInput([]byte{0, 255, 'x'}); err != nil {
		t.Fatal(err)
	}
	events := collect(t, a)
	var output []byte
	for _, event := range events {
		if event.Kind == "raw_pty" {
			output = append(output, event.Data...)
		}
	}
	if !bytes.Contains(output, []byte("37 91")) || !bytes.Contains(output, []byte("0 255 120")) {
		t.Fatalf("interactive output = %q", output)
	}
}

func TestPTYNonzeroExitEmitsErrorStatus(t *testing.T) {
	a := New("failure")
	if err := a.Spawn(context.Background(), SpawnOptions{Command: "/bin/sh", Args: []string{"-c", "exit 7"}}); err != nil {
		t.Fatal(err)
	}
	events := collect(t, a)
	exit, err := a.Wait(context.Background())
	if err != nil || exit.Code != 7 {
		t.Fatalf("exit = %+v, %v", exit, err)
	}
	if last := events[len(events)-1]; last.Kind != "status" || eventStatus(t, last) != "error" {
		t.Fatalf("last event = %+v", last)
	}
}

func TestPTYForcedCleanupAndDisposeRace(t *testing.T) {
	for i := 0; i < 5; i++ {
		a := New("cleanup")
		if err := a.Spawn(context.Background(), SpawnOptions{
			Command: "/bin/sh", Args: []string{"-c", "trap '' TERM; sleep 60"},
			ShutdownTimeout: 20 * time.Millisecond,
		}); err != nil {
			t.Fatal(err)
		}
		pid := a.PID()
		disposed := make(chan error, 1)
		go func() { disposed <- a.Dispose(context.Background()) }()
		select {
		case err := <-disposed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("dispose timed out")
		}
		select {
		case <-a.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("adapter did not finish")
		}
		if err := syscall.Kill(pid, 0); err == nil || !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("process %d still exists: %v", pid, err)
		}
		if err := a.Dispose(context.Background()); err != nil {
			t.Fatalf("repeated dispose: %v", err)
		}
	}
}
