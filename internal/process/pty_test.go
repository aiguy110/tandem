//go:build unix

package process

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func readPTY(t *testing.T, terminal *PTY) []byte {
	t.Helper()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&out, terminal)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read PTY: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading PTY")
	}
	return out.Bytes()
}

func TestPTYBinaryIOEnvironmentAndCWD(t *testing.T) {
	dir := t.TempDir()
	terminal, err := StartPTY(context.Background(), Spec{
		Argv: []string{"/bin/sh", "-c", `stty raw -echo; printf '%s|' "$TANDEM_PTY"; pwd; printf '\000\001\377'`},
		Dir:  dir, Env: append(os.Environ(), "TANDEM_PTY=yes"),
	}, Size{Rows: 24, Cols: 80}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := readPTY(t, terminal)
	if exit := wait(t, terminal.proc); exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	wantPrefix := []byte("yes|" + dir + "\n")
	want := append(wantPrefix, 0, 1, 255)
	if !bytes.Equal(out, want) {
		t.Fatalf("binary output = %v, want %v", out, want)
	}
	if err := terminal.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPTYResizeAndInput(t *testing.T) {
	terminal, err := StartPTY(context.Background(), Spec{
		Argv: []string{"/bin/sh", "-c", `stty raw -echo; dd bs=1 count=1 >/dev/null 2>/dev/null; stty size`},
		Env:  os.Environ(),
	}, Size{Rows: 24, Cols: 80}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := terminal.Resize(Size{Rows: 37, Cols: 91}); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	out := readPTY(t, terminal)
	if !bytes.Contains(out, []byte("37 91")) {
		t.Fatalf("resized stty output = %q", out)
	}
	if err := terminal.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPTYCancellationAndRepeatedDispose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	terminal, err := StartPTY(ctx, Spec{Argv: []string{"/bin/sh", "-c", `sleep 60`}, Env: os.Environ()}, Size{Rows: 24, Cols: 80}, Options{ShutdownTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pid := terminal.PID()
	cancel()
	if exit := wait(t, terminal.proc); exit.Code == 0 {
		t.Fatalf("cancelled PTY exit = %+v", exit)
	}
	assertProcessGone(t, pid)
	for i := 0; i < 3; i++ {
		if err := terminal.Dispose(context.Background()); err != nil {
			t.Fatalf("dispose %d: %v", i, err)
		}
	}
}

func TestPTYRejectsInvalidSize(t *testing.T) {
	_, err := StartPTY(context.Background(), Spec{Argv: []string{"/bin/sh"}}, Size{}, Options{})
	if err == nil {
		t.Fatal("expected invalid size error")
	}
}
