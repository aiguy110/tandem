package controlmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPHandshakeAndTakeoverBlocksUntilRelease(t *testing.T) {
	var released atomic.Bool
	var posted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.Method {
		case http.MethodPost:
			if r.URL.Query().Get("sessionId") != "api-58" {
				http.Error(w, "agent", 400)
				return
			}
			posted.Store(true)
			_, _ = io.WriteString(w, `{"reqId":"tk_1"}`)
		case http.MethodGet:
			fmt.Fprintf(w, `{"resolved":%t}`, released.Load())
		}
	}))
	defer server.Close()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, inR, outW, Config{ControlURL: server.URL, Token: "secret", SessionID: "api-58", BrowserEnabled: true, PollInterval: 10 * time.Millisecond})
	}()
	responses := make(chan map[string]any, 8)
	go func() {
		s := bufio.NewScanner(outR)
		for s.Scan() {
			var v map[string]any
			if json.Unmarshal(s.Bytes(), &v) == nil {
				responses <- v
			}
		}
	}()
	send := func(line string) { _, _ = io.WriteString(inW, line+"\n") }
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	for range 2 {
		select {
		case <-responses:
		case <-time.After(time.Second):
			t.Fatal("MCP handshake timed out")
		}
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"browser_request_takeover","arguments":{"reason":"log in"}}}`)
	deadline := time.Now().Add(time.Second)
	for !posted.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !posted.Load() {
		t.Fatal("takeover was not registered")
	}
	select {
	case got := <-responses:
		t.Fatalf("tool resolved before release: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
	released.Store(true)
	select {
	case got := <-responses:
		data, _ := json.Marshal(got)
		if !strings.Contains(string(data), "handed control back") {
			t.Fatalf("tool response = %s", data)
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not resolve after release")
	}
	_ = inW.Close()
	_ = outW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

// startTakeover runs the MCP server against a daemon stub whose GET status
// body comes from status, and issues one takeover call with id 7.
func startTakeover(t *testing.T, status func() string) (send func(string), responses chan map[string]any, polls *atomic.Int64) {
	t.Helper()
	polls = &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"reqId":"tk_1"}`)
			return
		}
		polls.Add(1)
		_, _ = io.WriteString(w, status())
	}))
	t.Cleanup(server.Close)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = inW.Close(); _ = outW.Close() })
	go func() {
		_ = Run(ctx, inR, outW, Config{ControlURL: server.URL, Token: "secret", SessionID: "s", BrowserEnabled: true, PollInterval: 5 * time.Millisecond})
	}()
	responses = make(chan map[string]any, 8)
	go func() {
		s := bufio.NewScanner(outR)
		for s.Scan() {
			var v map[string]any
			if json.Unmarshal(s.Bytes(), &v) == nil {
				responses <- v
			}
		}
	}()
	send = func(line string) { _, _ = io.WriteString(inW, line+"\n") }
	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"browser_request_takeover","arguments":{"reason":"log in"}}}`)
	return send, responses, polls
}

func TestTakeoverStopsPollingWhenClientCancels(t *testing.T) {
	send, responses, polls := startTakeover(t, func() string { return `{"resolved":false}` })
	deadline := time.Now().Add(time.Second)
	for polls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"turn cancelled"}}`)
	select {
	case <-responses:
	case <-time.After(time.Second):
		t.Fatal("cancelled takeover call did not finish")
	}
	settled := polls.Load()
	time.Sleep(50 * time.Millisecond)
	if polls.Load() != settled {
		t.Fatal("takeover kept polling after cancellation")
	}
}

func TestTakeoverReportsDismissal(t *testing.T) {
	_, responses, _ := startTakeover(t, func() string { return `{"resolved":true,"dismissed":true}` })
	select {
	case got := <-responses:
		data, _ := json.Marshal(got)
		if !strings.Contains(string(data), "dismissed") || !strings.Contains(string(data), `"error"`) {
			t.Fatalf("tool response = %s, want dismissal error", data)
		}
	case <-time.After(time.Second):
		t.Fatal("dismissed takeover did not finish")
	}
}
