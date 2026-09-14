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
			if r.URL.Query().Get("agentId") != "api-58" {
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
		done <- Run(ctx, inR, outW, Config{ControlURL: server.URL, Token: "secret", SessionID: "api-58", PollInterval: 10 * time.Millisecond})
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
