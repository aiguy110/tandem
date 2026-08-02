package toolregistry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/aiguy110/tandem/internal/browser"
)

const mcpProtocolVersion = "2025-06-18"

// MCPClient owns one stdio MCP subprocess. It supports concurrent JSON-RPC
// calls and turns the server's tools/list response into registry Tools.
type MCPClient struct {
	server browser.MCPServer
	cmd    *exec.Cmd
	in     io.WriteCloser

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan mcpResponse
	tools   []mcpTool
	done    chan struct{}
	waitErr error
	cancel  context.CancelFunc
	once    sync.Once
}

type mcpResponse struct {
	result json.RawMessage
	err    error
}

type mcpTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

type mcpWireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *mcpWireError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpWireError   `json:"error,omitempty"`
}

// MCPError is returned for JSON-RPC errors produced by an MCP server.
type MCPError struct {
	Server, Method string
	Code           int
	Message        string
	Data           json.RawMessage
}

func (e *MCPError) Error() string {
	return fmt.Sprintf("mcp server %s %s: rpc error %d: %s", e.Server, e.Method, e.Code, e.Message)
}

// StartMCP starts and initializes an existing daemon MCP declaration.
func StartMCP(ctx context.Context, server browser.MCPServer, dir string, stderr io.Writer) (*MCPClient, error) {
	if server.Name == "" || strings.Contains(server.Name, ".") {
		return nil, fmt.Errorf("mcp server name %q must be non-empty and contain no dots", server.Name)
	}
	if server.Command == "" {
		return nil, errors.New("mcp command is required")
	}
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, server.Command, server.Args...)
	cmd.Dir = dir
	cmd.Env = mergeEnvironment(os.Environ(), server.Env)
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start mcp server %s: %w", server.Name, err)
	}
	c := &MCPClient{server: server, cmd: cmd, in: in, pending: make(map[string]chan mcpResponse), done: make(chan struct{}), cancel: cancel}
	go c.read(out)
	go c.reap()
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "tandem", "version": "0.1.0"},
	}, &initialized); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize mcp server %s: %w", server.Name, err)
	}
	if initialized.ProtocolVersion == "" {
		_ = c.Close()
		return nil, fmt.Errorf("initialize mcp server %s: missing protocolVersion", server.Name)
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		_ = c.Close()
		return nil, err
	}
	var listed struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &listed); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("list mcp server %s tools: %w", server.Name, err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "" || strings.Contains(tool.Name, ".") {
			_ = c.Close()
			return nil, fmt.Errorf("mcp server %s returned invalid tool name %q", server.Name, tool.Name)
		}
		if len(tool.InputSchema) == 0 {
			tool.InputSchema = json.RawMessage(`{"type":"object"}`)
		}
		if !json.Valid(tool.InputSchema) {
			_ = c.Close()
			return nil, fmt.Errorf("mcp server %s tool %s has invalid input schema", server.Name, tool.Name)
		}
		c.tools = append(c.tools, tool)
	}
	return c, nil
}

// Tools returns handlers qualified as server.tool.
func (c *MCPClient) Tools() []Tool {
	tools := make([]Tool, 0, len(c.tools))
	for _, declared := range c.tools {
		declared := declared
		qualified := c.server.Name + "." + declared.Name
		tools = append(tools, Tool{
			Declaration: Declaration{Name: qualified, Description: declared.Description, InputSchema: cloneRaw(declared.InputSchema), OutputSchema: cloneRaw(declared.OutputSchema)},
			Available:   c.Availability,
			Invoke: func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
				return c.CallTool(ctx, declared.Name, arguments)
			},
		})
	}
	return tools
}

func (c *MCPClient) Availability(context.Context) error {
	select {
	case <-c.done:
		if c.waitErr != nil {
			return c.waitErr
		}
		return io.EOF
	default:
		return nil
	}
}

func (c *MCPClient) CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var result json.RawMessage
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Register adds all discovered MCP tools, rolling back if any registration
// fails. The client remains caller-owned and must still be closed.
func (c *MCPClient) Register(r *Registry) error {
	registered := make([]string, 0, len(c.tools))
	for _, tool := range c.Tools() {
		if err := r.Register(tool); err != nil {
			for _, name := range registered {
				r.Unregister(name)
			}
			return err
		}
		registered = append(registered, tool.Declaration.Name)
	}
	return nil
}

func (c *MCPClient) Close() error {
	c.once.Do(c.cancel)
	<-c.done
	return c.waitErr
}

func (c *MCPClient) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("%d", c.nextID)
	response := make(chan mcpResponse, 1)
	c.pending[id] = response
	c.mu.Unlock()
	if err := c.write(mcpMessage{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: marshalMCP(params)}); err != nil {
		c.removePending(id)
		return err
	}
	select {
	case got := <-response:
		if got.err != nil {
			if wire, ok := got.err.(*mcpWireError); ok {
				return &MCPError{Server: c.server.Name, Method: method, Code: wire.Code, Message: wire.Message, Data: wire.Data}
			}
			return got.err
		}
		if result == nil {
			return nil
		}
		if raw, ok := result.(*json.RawMessage); ok {
			*raw = cloneRaw(got.result)
			return nil
		}
		if err := json.Unmarshal(got.result, result); err != nil {
			return fmt.Errorf("decode mcp %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case <-c.done:
		c.removePending(id)
		return c.unavailableError()
	}
}

func (c *MCPClient) notify(method string, params any) error {
	return c.write(mcpMessage{JSONRPC: "2.0", Method: method, Params: marshalMCP(params)})
}

func (c *MCPClient) write(message mcpMessage) error {
	line, err := json.Marshal(message)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.unavailableError()
	default:
	}
	if _, err := c.in.Write(line); err != nil {
		return fmt.Errorf("write mcp server %s: %w", c.server.Name, err)
	}
	return nil
}

func (c *MCPClient) read(out io.Reader) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	for scanner.Scan() {
		var message mcpMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil || message.JSONRPC != "2.0" {
			continue
		}
		// Current Tandem MCP servers never issue client requests. Ignore server
		// notifications and requests so unsolicited traffic cannot corrupt calls.
		if message.Method != "" || len(message.ID) == 0 {
			continue
		}
		id := string(message.ID)
		c.mu.Lock()
		pending := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if pending == nil {
			continue
		}
		if message.Error != nil {
			pending <- mcpResponse{err: message.Error}
		} else {
			pending <- mcpResponse{result: message.Result}
		}
	}
}

func (c *MCPClient) reap() {
	err := c.cmd.Wait()
	c.mu.Lock()
	c.waitErr = err
	pending := c.pending
	c.pending = make(map[string]chan mcpResponse)
	c.mu.Unlock()
	for _, response := range pending {
		response <- mcpResponse{err: c.unavailableError()}
	}
	close(c.done)
}

func (c *MCPClient) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *MCPClient) unavailableError() error {
	if c.waitErr != nil {
		return fmt.Errorf("mcp server %s exited: %w", c.server.Name, c.waitErr)
	}
	return fmt.Errorf("mcp server %s closed", c.server.Name)
}

func marshalMCP(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func mergeEnvironment(base []string, overrides []browser.MCPEnvVariable) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, entry := range overrides {
		if _, seen := values[entry.Name]; !seen {
			order = append(order, entry.Name)
		}
		values[entry.Name] = entry.Value
	}
	merged := make([]string, 0, len(order))
	for _, name := range order {
		merged = append(merged, name+"="+values[name])
	}
	return merged
}
