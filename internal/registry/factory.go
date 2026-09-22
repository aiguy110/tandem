package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aiguy110/tandem/internal/acp"
	"github.com/aiguy110/tandem/internal/acpadapter"
	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/ptyadapter"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
	"github.com/aiguy110/tandem/internal/terminalhost"
	"github.com/aiguy110/tandem/internal/workspacefs"
)

type DefaultFactory struct {
	Assets     *assets.Store
	Config     config.Config
	MCPServers func(sessionID, workspaceCWD string) ([]browser.MCPServer, error)
}

func (f DefaultFactory) Start(ctx context.Context, req agentadapter.StartRequest) (agentadapter.Adapter, error) {
	launch := req.Spec.ResolvedLaunch
	if launch == nil {
		return nil, errors.New("adapter launch was not resolved")
	}
	if req.Spec.Adapter == "pty" {
		if launch.Terminal == nil {
			return nil, errors.New("terminal launch is missing")
		}
		a := ptyadapter.New(req.SessionID)
		if err := a.Spawn(ctx, ptyadapter.SpawnOptions{Command: launch.Terminal.Cmd, Args: launch.Terminal.StartArgs, Dir: req.CWD, Env: launch.Terminal.Env}); err != nil {
			return nil, err
		}
		return &ptyAdapter{Adapter: a}, nil
	}
	if launch.ACP == nil {
		return nil, errors.New("ACP launch is missing")
	}
	if launch.Distribution == nil {
		if err := runtimeinstall.EnsureAgent(ctx, f.Config, req.Spec.Agent, os.Stderr); err != nil {
			return nil, fmt.Errorf("provision agent %s: %w", req.Spec.Agent, err)
		}
	} else if entry, ok := runtimeinstall.EntryPoint(req.Spec.Agent, launch.Distribution); !ok || launch.ACP == nil || len(launch.ACP.Args) == 0 || launch.ACP.Args[0] != entry {
		return nil, fmt.Errorf("managed agent %s has an invalid pinned distribution", req.Spec.Agent)
	}
	fs, err := workspacefs.Open(req.CWD)
	if err != nil {
		return nil, err
	}
	proxy := &eventAppender{fallback: req.Log}
	host, err := terminalhost.New(terminalhost.Options{DefaultCwd: req.CWD, EventLog: proxy})
	if err != nil {
		fs.Close()
		return nil, err
	}
	var mcpServers []acpadapter.MCPServer
	if f.MCPServers != nil {
		configured, err := f.MCPServers(req.SessionID, req.CWD)
		if err != nil {
			fs.Close()
			host.Close(context.Background())
			return nil, fmt.Errorf("load MCP servers: %w", err)
		}
		for _, server := range configured {
			env := make([]acp.EnvVariable, len(server.Env))
			for i, variable := range server.Env {
				env[i] = acp.EnvVariable{Name: variable.Name, Value: variable.Value}
			}
			headers := make([]acp.EnvVariable, len(server.Headers))
			for i, header := range server.Headers {
				headers[i] = acp.EnvVariable{Name: header.Name, Value: header.Value}
			}
			mcpServers = append(mcpServers, acpadapter.MCPServer{Name: server.Name, Type: server.Type, Command: server.Command, Args: server.Args, Env: env, URL: server.URL, Headers: headers})
		}
	}
	// Agents whose ACP server ignores session/new mcpServers (pi-acp) load the
	// same list through an agent-side bridge; see runtime/pi/mcp-bridge.ts.
	if err := runtimeinstall.StageAgentSupport(f.Config); err != nil {
		slog.Warn("stage agent support files failed", "session", req.SessionID, "agent", req.Spec.Agent, "err", err)
	}
	env := maps.Clone(launch.ACP.Env)
	if env == nil {
		env = map[string]string{}
	}
	serversFile, err := f.writeMCPServersFile(req.SessionID, mcpServers)
	if err != nil {
		slog.Warn("write MCP servers file failed; bridged agents will lack MCP tools", "session", req.SessionID, "agent", req.Spec.Agent, "err", err)
	} else if serversFile != "" {
		env[mcpServersFileEnv] = serversFile
		env[mcpBridgeLogEnv] = filepath.Join(f.Config.Home, "logs", "mcp-bridge.log")
		slog.Info("wrote MCP servers file for agent bridge", "session", req.SessionID, "agent", req.Spec.Agent, "servers", len(mcpServers), "path", serversFile)
	}
	a, err := acpadapter.StartAdapter(ctx, acpadapter.AdapterConfig{SessionID: req.SessionID, Cwd: req.CWD, ResumeSessionID: req.ResumeSessionID, CaptureReplay: req.CaptureReplay, MCPServers: mcpServers, Assets: f.Assets, WorkspaceFS: fs, Terminals: host, ParentToolCallIDPath: launch.ACP.ParentToolCallIDPath, Transport: acp.Config{Command: launch.ACP.Cmd, Args: launch.ACP.Args, Dir: req.CWD, Env: envList(env), Stderr: os.Stderr}})
	if err != nil {
		removeMCPServersFile(serversFile)
		host.Close(context.Background())
		fs.Close()
		return nil, err
	}
	wrapped := &acpAdapter{Adapter: a, fs: fs, host: host, appender: proxy, events: make(chan eventlog.Event, 256), serversFile: serversFile}
	go wrapped.forwardEvents()
	return wrapped, nil
}

const (
	mcpServersFileEnv = "TANDEM_MCP_SERVERS_FILE"
	mcpBridgeLogEnv   = "TANDEM_MCP_BRIDGE_LOG"
)

// writeMCPServersFile persists the session's resolved MCP servers, in the
// same JSON shape sent in ACP session/new, to an owner-only file whose path is
// exported to the ACP process as TANDEM_MCP_SERVERS_FILE. The file carries the
// daemon token (tandem-control's env), so it lives under TANDEM_HOME/run and is
// removed when the adapter closes.
func (f DefaultFactory) writeMCPServersFile(sessionID string, servers []acpadapter.MCPServer) (string, error) {
	if len(servers) == 0 || f.Config.Home == "" {
		return "", nil
	}
	dir := filepath.Join(f.Config.Home, "run", "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, strings.ReplaceAll(sessionID, "/", "-")+"-*.json")
	if err != nil {
		return "", err
	}
	_, err = file.Write(data)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func removeMCPServersFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("remove MCP servers file failed", "path", path, "err", err)
	}
}

func envList(m map[string]string) []string {
	out := append([]string{}, os.Environ()...)
	positions := make(map[string]int, len(out))
	for i, entry := range out {
		key, _, _ := strings.Cut(entry, "=")
		positions[key] = i
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := key + "=" + m[key]
		if i, ok := positions[key]; ok {
			out[i] = entry
		} else {
			positions[key] = len(out)
			out = append(out, entry)
		}
	}
	return out
}

type acpAdapter struct {
	*acpadapter.Adapter
	fs       *workspacefs.FS
	host     *terminalhost.Host
	appender *eventAppender
	events   chan eventlog.Event
	// serversFile is the TANDEM_MCP_SERVERS_FILE written for this launch.
	serversFile string
}

func (a *acpAdapter) Events() <-chan eventlog.Event { return a.events }
func (a *acpAdapter) forwardEvents() {
	defer close(a.events)
	for {
		select {
		case ev, ok := <-a.Adapter.Events():
			if !ok {
				return
			}
			select {
			case a.events <- ev:
			case <-a.Adapter.Done():
				a.drainEvents()
				return
			}
		case <-a.Adapter.Done():
			a.drainEvents()
			return
		}
	}
}

func (a *acpAdapter) drainEvents() {
	for {
		select {
		case ev, ok := <-a.Adapter.Events():
			if !ok {
				return
			}
			select {
			case a.events <- ev:
			default:
			}
		default:
			return
		}
	}
}

type eventAppender struct {
	mu       sync.RWMutex
	sink     func(eventlog.Event) (eventlog.LoggedEvent, error)
	fallback *eventlog.Log
}

func (a *eventAppender) Append(ev eventlog.Event) (eventlog.LoggedEvent, error) {
	a.mu.RLock()
	sink := a.sink
	a.mu.RUnlock()
	if sink != nil {
		return sink(ev)
	}
	return a.fallback.Append(ev)
}
func (a *eventAppender) bind(sink func(eventlog.Event) (eventlog.LoggedEvent, error)) {
	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
}
func (a *acpAdapter) BindEventSink(sink func(eventlog.Event) (eventlog.LoggedEvent, error)) {
	a.appender.bind(sink)
}

func (a *acpAdapter) Capabilities() agentadapter.Capabilities {
	c := a.Adapter.Capabilities()
	return agentadapter.Capabilities{Structured: c.Structured, Terminals: true, LoadSession: c.LoadSession, ForkSession: c.ForkSession, FS: true, Image: c.Image, Steering: c.Steering}
}
func (a *acpAdapter) Steer(ctx context.Context, b []agentadapter.PromptBlock) error {
	in := make([]acpadapter.PromptBlock, len(b))
	for i, x := range b {
		in[i] = acpadapter.PromptBlock{Type: x.Type, Text: x.Text, AssetID: x.AssetID, MIMEType: x.MIMEType, Name: x.Name}
	}
	return a.Adapter.Steer(ctx, in)
}
func (a *acpAdapter) Prompt(ctx context.Context, b []agentadapter.PromptBlock) (string, error) {
	in := make([]acpadapter.PromptBlock, len(b))
	for i, x := range b {
		in[i] = acpadapter.PromptBlock{Type: x.Type, Text: x.Text, AssetID: x.AssetID, MIMEType: x.MIMEType, Name: x.Name}
	}
	return a.Adapter.Prompt(ctx, in)
}
func (a *acpAdapter) Aside(ctx context.Context, id string, b []agentadapter.PromptBlock) (string, error) {
	in := make([]acpadapter.PromptBlock, len(b))
	for i, x := range b {
		in[i] = acpadapter.PromptBlock{Type: x.Type, Text: x.Text, AssetID: x.AssetID, MIMEType: x.MIMEType, Name: x.Name}
	}
	return a.Adapter.Aside(ctx, id, in)
}
func (a *acpAdapter) ValidatePrompt(b []agentadapter.PromptBlock) error {
	in := make([]acpadapter.PromptBlock, len(b))
	for i, x := range b {
		in[i] = acpadapter.PromptBlock{Type: x.Type, Text: x.Text, AssetID: x.AssetID, MIMEType: x.MIMEType, Name: x.Name}
	}
	return a.Adapter.ValidatePrompt(in)
}
func (a *acpAdapter) SendInput([]byte) error {
	return errors.New("ACP adapter does not accept raw input")
}
func (a *acpAdapter) Resize(uint16, uint16) error {
	return errors.New("ACP adapter does not support resize")
}
func (a *acpAdapter) Close(ctx context.Context) error {
	err := a.Adapter.Close()
	if e := a.host.Close(ctx); err == nil {
		err = e
	}
	if e := a.fs.Close(); err == nil {
		err = e
	}
	removeMCPServersFile(a.serversFile)
	return err
}

type ptyAdapter struct{ *ptyadapter.Adapter }

func (a *ptyAdapter) Capabilities() agentadapter.Capabilities { return agentadapter.Capabilities{} }
func (a *ptyAdapter) ExternalSessionID() string               { return "" }
func (a *ptyAdapter) Prompt(context.Context, []agentadapter.PromptBlock) (string, error) {
	return "", errors.New("PTY adapter does not support structured prompts")
}
func (a *ptyAdapter) RespondPermission(string, string) error {
	return errors.New("PTY adapter does not support permissions")
}
func (a *ptyAdapter) Close(ctx context.Context) error { return a.Dispose(ctx) }
