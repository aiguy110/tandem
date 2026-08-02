package automationmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestToolsAndAuthenticatedRunRequest(t *testing.T) {
	requestSeen := make(chan map[string]any, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/automation/run" || r.Method != http.MethodPost {
			http.Error(w, "route", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requestSeen <- body
		_, _ = io.WriteString(w, `{"runId":"run-1","status":"started"}`)
	}))
	defer httpServer.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), inR, outW, Config{ControlURL: httpServer.URL, Token: "secret", AgentID: "agent-7", WorkspaceCWD: "/worktrees/repo"})
	}()
	responses := make(chan map[string]any, 4)
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			var value map[string]any
			_ = json.Unmarshal(scanner.Bytes(), &value)
			responses <- value
		}
	}()
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	list := receive(t, responses)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %#v", tools)
	}
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"scripts_run","arguments":{"path":".tandem/scripts/check.ts","args":["now"]}}}`+"\n")
	response := receive(t, responses)
	result := response["result"].(map[string]any)
	if result["isError"] == true || result["structuredContent"].(map[string]any)["runId"] != "run-1" {
		t.Fatalf("result = %#v", result)
	}
	request := <-requestSeen
	if request["agentId"] != "agent-7" || request["workspaceCwd"] != "/worktrees/repo" || request["path"] != ".tandem/scripts/check.ts" {
		t.Fatalf("request = %#v", request)
	}
	_ = inW.Close()
	_ = outW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPFailureIsMCPToolError(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"approval required"}`)
	}))
	defer httpServer.Close()
	var out strings.Builder
	err := Run(t.Context(), strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"scripts_preapprove","arguments":{"path":".tandem/scripts/job.ts"}}}`+"\n"), &out, Config{ControlURL: httpServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"isError":true`) || !strings.Contains(out.String(), "approval required") {
		t.Fatalf("response = %s", out.String())
	}
}

func receive(t *testing.T, responses <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(time.Second):
		t.Fatal("MCP response timed out")
		return nil
	}
}
