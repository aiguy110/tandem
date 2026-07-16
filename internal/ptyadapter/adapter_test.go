package ptyadapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/aiguy110/tandem/internal/eventlog"
)

type chunkReader struct {
	chunks [][]byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}

func TestStreamPreservesSplitUTF8AndArbitraryBytes(t *testing.T) {
	reader := &chunkReader{chunks: [][]byte{
		{0xe2}, {0x82}, {0xac, 0x00, 0xff, 0xfe},
	}}
	var got [][]byte
	err := stream(reader, func(event eventlog.Event) bool {
		got = append(got, bytes.Clone(event.Data))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{{0xe2}, {0x82}, {0xac, 0x00, 0xff, 0xfe}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw events = %v, want %v", got, want)
	}
}

func TestStreamReportsReadErrorAfterBytes(t *testing.T) {
	wantErr := errors.New("boom")
	reader := &dataErrorReader{data: []byte{1, 2, 3}, err: wantErr}
	var got []byte
	err := stream(reader, func(event eventlog.Event) bool {
		got = append(got, event.Data...)
		return true
	})
	if !errors.Is(err, wantErr) || !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("stream = (%v, %v)", got, err)
	}
}

type dataErrorReader struct {
	data []byte
	err  error
}

func (r *dataErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = nil
	err := r.err
	r.err = io.EOF
	return n, err
}

func TestCapabilitiesAndUnsupportedStructuredOperations(t *testing.T) {
	a := New("shell-1")
	if got := a.Capabilities(); got != (Capabilities{}) {
		t.Fatalf("capabilities = %+v", got)
	}
	for name, err := range map[string]error{
		"prompt":     a.Prompt("hello"),
		"permission": a.RespondPermission("request", "allow"),
		"load":       a.LoadSession("session"),
	} {
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("unsupported")) {
			t.Errorf("%s error = %v", name, err)
		}
	}
	if err := a.SendInput([]byte("x")); err == nil {
		t.Fatal("input before spawn should fail")
	}
	if err := a.Resize(80, 24); err == nil {
		t.Fatal("resize before spawn should fail")
	}
}

func TestSpawnFailureAndDisposeBeforeSpawnCloseLifecycle(t *testing.T) {
	a := New("bad-command")
	spawnErr := a.Spawn(context.Background(), SpawnOptions{Command: "tandem-command-that-does-not-exist"})
	if spawnErr == nil {
		t.Fatal("expected spawn failure")
	}
	if _, err := a.Wait(context.Background()); err == nil || err.Error() != spawnErr.Error() {
		t.Fatalf("wait error = %v, want %v", err, spawnErr)
	}
	if _, ok := <-a.Events(); ok {
		t.Fatal("events should close after spawn failure")
	}

	disposed := New("never-started")
	if err := disposed.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := disposed.Spawn(context.Background(), SpawnOptions{}); err == nil {
		t.Fatal("spawn after dispose should fail")
	}
	if _, ok := <-disposed.Events(); ok {
		t.Fatal("events should close after disposal")
	}
}
