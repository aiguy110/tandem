package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/toolregistry"
)

// StartMCPToolSession starts only the MCP servers needed by requested qualified
// tool names. The returned session rejects every undeclared tool even though a
// selected MCP server may advertise additional capabilities.
func StartMCPToolSession(ctx context.Context, servers []browser.MCPServer, cwd string, requested []string, stderr io.Writer, onClose func()) (ToolSession, error) {
	byName := make(map[string]browser.MCPServer, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}
	wantedServers := map[string]bool{}
	allowed := make(map[string]bool, len(requested))
	for _, qualified := range requested {
		server, _, ok := strings.Cut(qualified, ".")
		if !ok || server == "" {
			return nil, fmt.Errorf("automation tool %q must be qualified as server.tool", qualified)
		}
		if _, exists := byName[server]; !exists {
			return nil, fmt.Errorf("MCP server %q is unavailable", server)
		}
		wantedServers[server], allowed[qualified] = true, true
	}
	names := make([]string, 0, len(wantedServers))
	for name := range wantedServers {
		names = append(names, name)
	}
	sort.Strings(names)
	session := &mcpToolSession{registry: toolregistry.New(), allowed: allowed, onClose: onClose}
	for _, name := range names {
		client, err := toolregistry.StartMCP(ctx, byName[name], cwd, stderr)
		if err != nil {
			_ = session.Close()
			return nil, err
		}
		session.clients = append(session.clients, client)
		if err := client.Register(session.registry); err != nil {
			_ = session.Close()
			return nil, err
		}
	}
	for qualified := range allowed {
		if _, exists := session.registry.Lookup(ctx, qualified); !exists {
			_ = session.Close()
			return nil, fmt.Errorf("MCP tool %q is unavailable", qualified)
		}
	}
	return session, nil
}

type mcpToolSession struct {
	registry *toolregistry.Registry
	clients  []*toolregistry.MCPClient
	allowed  map[string]bool
	onClose  func()
}

func (s *mcpToolSession) Invoke(ctx context.Context, name string, arguments json.RawMessage) (any, error) {
	if !s.allowed[name] {
		return nil, fmt.Errorf("tool %q was not approved for this run", name)
	}
	raw, err := s.registry.Invoke(ctx, name, arguments)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode MCP tool %q result: %w", name, err)
	}
	return value, nil
}

func (s *mcpToolSession) Close() error {
	var first error
	for i := len(s.clients) - 1; i >= 0; i-- {
		if err := s.clients[i].Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.onClose != nil {
		s.onClose()
		s.onClose = nil
	}
	return first
}
