package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("TANDEM_ACP_HELPER") == "" {
		return
	}
	mode := os.Getenv("TANDEM_ACP_HELPER")
	send := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	if mode == "inbound" {
		fmt.Println("not-json")
		send(map[string]any{"jsonrpc": "2.0", "id": 999, "result": map[string]any{}})
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"n": 1}})
		send(map[string]any{"jsonrpc": "2.0", "id": "agent-1", "method": "fs/read_text_file", "params": map[string]any{"path": "/tmp/x"}})
	}
	if mode == "stderr" {
		fmt.Fprint(os.Stderr, "agent diagnostic\n")
	}

	s := bufio.NewScanner(os.Stdin)
	var held json.RawMessage
	var calls []rpcMessage
	for s.Scan() {
		var m rpcMessage
		if json.Unmarshal(s.Bytes(), &m) != nil {
			continue
		}
		switch mode {
		case "order":
			calls = append(calls, m)
			if len(calls) == 2 {
				send(map[string]any{"jsonrpc": "2.0", "id": calls[1].ID, "result": calls[1].Method + "-result"})
				send(map[string]any{"jsonrpc": "2.0", "id": calls[0].ID, "result": calls[0].Method + "-result"})
			}
		case "cancel":
			if m.Method == "never" {
				held = append(held[:0], m.ID...)
			}
			if m.Method == "release" {
				send(map[string]any{"jsonrpc": "2.0", "id": held, "result": true})
			}
		case "eof":
			return
		case "fail":
			os.Exit(7)
		case "stderr":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": true})
			return
		case "inbound":
			if string(m.ID) == `"agent-1"` && m.Result != nil {
				return
			}
		}
	}
}

func startHelper(t *testing.T, mode string, stderr *bytes.Buffer) *Transport {
	t.Helper()
	cfg := Config{Command: os.Args[0], Args: []string{"-test.run=^TestHelperProcess$"}, Env: append(os.Environ(), "TANDEM_ACP_HELPER="+mode)}
	if stderr != nil {
		cfg.Stderr = stderr
	}
	tr, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if tr.cmd.ProcessState == nil {
			_ = tr.Close()
		}
	})
	return tr
}

func TestConcurrentCallsResolveOutOfOrder(t *testing.T) {
	tr := startHelper(t, "order", nil)
	var wg sync.WaitGroup
	results := make(map[string]string)
	var mu sync.Mutex
	for _, method := range []string{"slow", "fast"} {
		method := method
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got string
			if err := tr.Call(context.Background(), method, nil, &got); err != nil {
				t.Errorf("%s: %v", method, err)
				return
			}
			mu.Lock()
			results[method] = got
			mu.Unlock()
		}()
	}
	wg.Wait()
	if results["slow"] != "slow-result" || results["fast"] != "fast-result" {
		t.Fatalf("results: %#v", results)
	}
}

func TestInboundRequestsNotificationsAndProtocolErrors(t *testing.T) {
	tr := startHelper(t, "inbound", nil)

	select {
	case n := <-tr.Notifications():
		if n.Method != "session/update" || !bytes.Contains(n.Params, []byte(`"n":1`)) {
			t.Fatalf("notification: %#v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}
	select {
	case r := <-tr.Requests():
		if r.Method != "fs/read_text_file" || string(r.ID) != `"agent-1"` {
			t.Fatalf("request: %#v", r)
		}
		if err := tr.Respond(r.ID, map[string]string{"content": "ok"}); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request not delivered")
	}

	var malformed, unknown bool
	for i := 0; i < 2; i++ {
		select {
		case err := <-tr.Errors():
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("unexpected error: %v", err)
			}
			malformed = malformed || pe.Kind == "malformed message"
			unknown = unknown || pe.Kind == "unknown"
		case <-time.After(time.Second):
			t.Fatal("protocol error not reported")
		}
	}
	if !malformed || !unknown {
		t.Fatalf("malformed=%v unknown=%v", malformed, unknown)
	}
	if err := tr.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestCallCancellationRemovesPendingRequest(t *testing.T) {
	tr := startHelper(t, "cancel", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Call(ctx, "never", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("call: %v", err)
	}
	if err := tr.Notify("release", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-tr.Errors():
		var pe *ProtocolError
		if !errors.As(err, &pe) || pe.Kind != "unknown" {
			t.Fatalf("late response: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late response was not reported")
	}
}

func TestEOFAndChildFailurePropagate(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{{"eof", "EOF"}, {"fail", "exit status 7"}} {
		t.Run(tc.mode, func(t *testing.T) {
			tr := startHelper(t, tc.mode, nil)
			err := tr.Call(context.Background(), "call", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("call error %v, want %q", err, tc.want)
			}
			if tc.mode == "fail" {
				if err := tr.Wait(); err == nil || !strings.Contains(err.Error(), "exit status 7") {
					t.Fatalf("wait: %v", err)
				}
			}
		})
	}
}

func TestStderrForwarding(t *testing.T) {
	var stderr bytes.Buffer
	tr := startHelper(t, "stderr", &stderr)
	var result bool
	if err := tr.Call(context.Background(), "call", nil, &result); err != nil {
		t.Fatal(err)
	}
	if err := tr.Wait(); err != nil {
		t.Fatal(err)
	}
	if !result || stderr.String() != "agent diagnostic\n" {
		t.Fatalf("result=%v stderr=%q", result, stderr.String())
	}
}

func TestTypedNilStderrIsIgnored(t *testing.T) {
	var stderr *bytes.Buffer
	cfg := Config{Command: os.Args[0], Args: []string{"-test.run=^TestHelperProcess$"}, Env: append(os.Environ(), "TANDEM_ACP_HELPER=stderr"), Stderr: stderr}
	tr, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	var result bool
	if err := tr.Call(context.Background(), "call", nil, &result); err != nil || !result {
		t.Fatalf("result=%v err=%v", result, err)
	}
}
