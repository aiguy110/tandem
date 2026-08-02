package toolregistry

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/aiguy110/tandem/internal/browser"
)

func TestMCPClientDiscoversRegistersAndCallsConcurrently(t *testing.T) {
	server := browser.MCPServer{
		Name: "fake", Command: os.Args[0], Args: []string{"-test.run=TestMCPHelperProcess", "--"},
		Env: []browser.MCPEnvVariable{{Name: "GO_WANT_MCP_HELPER", Value: "1"}},
	}
	client, err := StartMCP(t.Context(), server, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	r := New()
	if err := client.Register(r); err != nil {
		t.Fatal(err)
	}
	status, ok := r.Lookup(t.Context(), "fake.echo")
	if !ok || !status.Available || status.Declaration.Description != "echo input" {
		t.Fatalf("Lookup = %#v, %v", status, ok)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := json.RawMessage(fmt.Sprintf(`{"value":%d}`, i))
			got, err := r.Invoke(context.Background(), "fake.echo", args)
			if err != nil {
				errs <- err
				return
			}
			var result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(got, &result); err != nil || len(result.Content) != 1 || result.Content[0].Text != fmt.Sprintf("%d", i) {
				errs <- fmt.Errorf("call %d result %s: %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "echo input", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			var params struct {
				Arguments struct {
					Value int `json:"value"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &params)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(params.Arguments.Value)}}}
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "unknown"}})
			continue
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
	os.Exit(0)
}
