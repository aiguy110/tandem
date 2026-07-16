//go:build unix

package terminalhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/store"
)

func hostWithLog(t *testing.T, cap int) (*Host, *eventlog.Log) {
	t.Helper()
	backing, err := store.Open(filepath.Join(t.TempDir(), "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backing.Close() })
	log, err := eventlog.New("api-58", backing, 2)
	if err != nil {
		t.Fatal(err)
	}
	host, err := New(Options{DefaultCwd: t.TempDir(), ScrollbackCap: cap, EventLog: log, ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = host.Close(ctx)
	})
	return host, log
}

func intp(value int) *int { return &value }

func createShell(t *testing.T, host *Host, script string, opts ...func(*CreateOptions)) string {
	t.Helper()
	create := CreateOptions{Command: "/bin/sh", Args: []string{"-c", script}}
	for _, apply := range opts {
		apply(&create)
	}
	id, err := host.Create(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitExit(t *testing.T, host *Host, id string) Exit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	exit, err := host.WaitForExit(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return exit
}

func waitOutput(t *testing.T, host *Host, id, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		output, err := host.Output(id)
		if err == nil && strings.Contains(output.Output, want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	output, _ := host.Output(id)
	t.Fatalf("terminal output %q never contained %q", output.Output, want)
}

func TestCreateEnvironmentCwdExitAndDurableEvents(t *testing.T) {
	host, log := hostWithLog(t, 1024)
	cwd := t.TempDir()
	id, err := host.Create(context.Background(), CreateOptions{
		Command: "/bin/sh", Args: []string{"-c", `printf '%s|' "$TANDEM_TERM"; pwd`}, Cwd: cwd,
		Env: []EnvVariable{{Name: "TANDEM_TERM", Value: "works"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	exit := waitExit(t, host, id)
	if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Signal != nil {
		t.Fatalf("exit = %#v", exit)
	}
	out, err := host.Output(id)
	if err != nil || !strings.Contains(out.Output, "works|"+cwd) || out.ExitStatus == nil {
		t.Fatalf("output = %#v, err=%v", out, err)
	}

	history, err := log.FullHistory()
	if err != nil || len(history) == 0 {
		t.Fatalf("history len=%d err=%v", len(history), err)
	}
	var joined strings.Builder
	for _, logged := range history {
		if logged.Event.Kind != "terminal_output" {
			t.Fatalf("event kind = %q", logged.Event.Kind)
		}
		var event struct {
			Kind      string `json:"kind"`
			TermID    string `json:"termId"`
			Chunk     string `json:"chunk"`
			Truncated bool   `json:"truncated"`
		}
		if err := json.Unmarshal(logged.Event.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind != "terminal_output" || event.TermID != id {
			t.Fatalf("normalized event = %#v", event)
		}
		joined.WriteString(event.Chunk)
	}
	if !strings.Contains(joined.String(), "works|"+cwd) {
		t.Fatalf("durable chunks = %q", joined.String())
	}
	if host.EventError() != nil {
		t.Fatal(host.EventError())
	}
}

func TestACPTruncationFromStartUTF8AndOffsets(t *testing.T) {
	host, _ := hostWithLog(t, 1024)
	id := createShell(t, host, `printf 'A\342\202\254BC'`, func(o *CreateOptions) { o.OutputByteLimit = intp(4) })
	waitExit(t, host, id)
	out, err := host.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	// A€BC is six bytes. Starting at byte two would split €, so the ACP
	// boundary advances to byte four and returns BC.
	if out.Output != "BC" || !out.Truncated || out.StartOffset != 4 || out.EndOffset != 6 {
		t.Fatalf("ACP output = %#v", out)
	}
	scroll, _ := host.Scrollback(id)
	if scroll.Output != "A€BC" || scroll.Truncated || scroll.StartOffset != 0 || scroll.EndOffset != 6 {
		t.Fatalf("scrollback = %#v", scroll)
	}
}

func TestIndependentScrollbackBoundAndReplayAfterRelease(t *testing.T) {
	host, log := hostWithLog(t, 8)
	id := createShell(t, host, `printf 'abcdefghijkl'`, func(o *CreateOptions) { o.OutputByteLimit = intp(3) })
	waitExit(t, host, id)
	if err := host.Release(id); err != nil {
		t.Fatal(err)
	}
	if !host.Has(id) {
		t.Fatal("release discarded terminal record")
	}
	out, _ := host.Output(id)
	scroll, _ := host.Scrollback(id)
	if out.Output != "jkl" || !out.Truncated || out.StartOffset != 9 || out.EndOffset != 12 {
		t.Fatalf("ACP output after release = %#v", out)
	}
	if scroll.Output != "efghijkl" || !scroll.Truncated || scroll.StartOffset != 4 || scroll.EndOffset != 12 {
		t.Fatalf("scrollback after release = %#v", scroll)
	}
	// The ring is intentionally too small for this assertion to depend on hot
	// memory: full history proves normalized chunks reached durable storage.
	history, err := log.FullHistory()
	if err != nil || len(history) == 0 {
		t.Fatalf("durable replay len=%d err=%v", len(history), err)
	}
}

func TestReleaseBeforeExitKillsAndRetainsOutput(t *testing.T) {
	host, _ := hostWithLog(t, 1024)
	id := createShell(t, host, `printf before; sleep 60`)
	waitOutput(t, host, id, "before")
	if err := host.Release(id); err != nil {
		t.Fatal(err)
	}
	exit := waitExit(t, host, id)
	if exit.Signal == nil || *exit.Signal != "9" {
		t.Fatalf("release exit = %#v, want node-pty-compatible signal 9", exit)
	}
	out, _ := host.Output(id)
	if !strings.Contains(out.Output, "before") || out.ExitStatus == nil || !host.Has(id) {
		t.Fatalf("released output = %#v has=%v", out, host.Has(id))
	}
	if err := host.Release("missing"); err != nil {
		t.Fatalf("unknown release is not idempotent: %v", err)
	}
}

func TestKillWaitAndCloseCleanup(t *testing.T) {
	host, _ := hostWithLog(t, 1024)
	id := createShell(t, host, `sleep 60`)
	if err := host.Kill(id); err != nil {
		t.Fatal(err)
	}
	exit := waitExit(t, host, id)
	if exit.Signal == nil || *exit.Signal != "9" {
		t.Fatalf("kill exit = %#v, want node-pty-compatible signal 9", exit)
	}
	status, err := host.ExitStatus(id)
	if err != nil || status == nil || status.Signal == nil {
		t.Fatalf("exit status = %#v err=%v", status, err)
	}

	second := createShell(t, host, `sleep 60`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if host.Has(id) || host.Has(second) {
		t.Fatal("close retained terminal records")
	}
	if _, err := host.Create(context.Background(), CreateOptions{Command: "/bin/true"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after close error = %v", err)
	}
}

func TestValidationAndConcurrentReads(t *testing.T) {
	host, _ := hostWithLog(t, 1024)
	if _, err := host.Create(context.Background(), CreateOptions{}); err == nil {
		t.Fatal("empty command accepted")
	}
	if _, err := host.Create(context.Background(), CreateOptions{Command: "/bin/true", OutputByteLimit: intp(-1)}); err == nil {
		t.Fatal("negative outputByteLimit accepted")
	}
	if _, err := host.Output("missing"); err == nil {
		t.Fatal("unknown terminal accepted")
	}

	id := createShell(t, host, `i=0; while [ $i -lt 100 ]; do printf x; i=$((i+1)); done`)
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				_, _ = host.Output(id)
				_, _ = host.Scrollback(id)
			}
		}()
	}
	waitExit(t, host, id)
	readers.Wait()
	out, _ := host.Output(id)
	if len(strings.ReplaceAll(out.Output, "\r", "")) != 100 {
		t.Fatalf("output length = %d, output=%q", len(out.Output), out.Output)
	}
}

type failingAppender struct{ err error }

func (f failingAppender) Append(eventlog.Event) (eventlog.LoggedEvent, error) {
	return eventlog.LoggedEvent{}, f.err
}

func TestEventAppendFailuresAreObservable(t *testing.T) {
	want := errors.New("disk full")
	host, err := New(Options{DefaultCwd: t.TempDir(), ScrollbackCap: 1024, EventLog: failingAppender{want}})
	if err != nil {
		t.Fatal(err)
	}
	id := createShell(t, host, `printf output`)
	waitExit(t, host, id)
	if !errors.Is(host.EventError(), want) {
		t.Fatalf("event error = %v", host.EventError())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.Close(ctx); !errors.Is(err, want) {
		t.Fatalf("close error = %v", err)
	}
}

func TestDefaultEnvironmentIsInheritedAndOverlaid(t *testing.T) {
	host, _ := hostWithLog(t, 1024)
	t.Setenv("TANDEM_INHERITED", "parent")
	id := createShell(t, host, `printf '%s/%s' "$TANDEM_INHERITED" "$TANDEM_OVERLAY"`, func(o *CreateOptions) {
		o.Env = []EnvVariable{{Name: "TANDEM_OVERLAY", Value: "child"}}
	})
	waitExit(t, host, id)
	out, _ := host.Output(id)
	if out.Output != "parent/child" {
		t.Fatalf("environment output = %q; parent env contains=%v", out.Output, os.Getenv("TANDEM_INHERITED"))
	}
}
