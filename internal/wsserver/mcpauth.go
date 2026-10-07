package wsserver

import (
	"context"
	"errors"
	"time"

	"github.com/aiguy110/tandem/internal/mcpauth"
)

// MCPAuth is the daemon's OAuth holder for remote MCP servers; see
// internal/mcpauth and docs/mcp-auth.md.
type MCPAuth interface {
	List() []mcpauth.Status
	Begin(ctx context.Context, key, redirectOrigin string) (string, error)
	SignOut(key string) error
	Recheck(ctx context.Context, key string) error
}

var errMCPAuthUnavailable = errors.New("MCP authorization is unavailable")

func (c *connection) handleMCPAuth(m clientMessage) {
	auth := c.server.opts.MCPAuth
	switch m.T {
	case "list_mcp_servers":
		if auth == nil {
			c.send(withCorr(map[string]any{"t": "mcp_servers", "error": errMCPAuthUnavailable.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "mcp_servers", "servers": auth.List()}, m.CorrID))
	case "begin_mcp_auth":
		if auth == nil {
			c.send(withCorr(map[string]any{"t": "mcp_auth_url", "error": errMCPAuthUnavailable.Error()}, m.CorrID))
			return
		}
		// Discovery and client registration are network round trips; keep
		// them off this browser's command loop.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			authURL, err := auth.Begin(ctx, m.ID, m.Origin)
			if err != nil {
				c.send(withCorr(map[string]any{"t": "mcp_auth_url", "error": err.Error()}, m.CorrID))
				return
			}
			c.send(withCorr(map[string]any{"t": "mcp_auth_url", "url": authURL}, m.CorrID))
		}()
	case "sign_out_mcp_server":
		if auth == nil {
			c.commandError(m, errMCPAuthUnavailable)
			return
		}
		if err := auth.SignOut(m.ID); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, "")
	case "recheck_mcp_server":
		if auth == nil {
			c.commandError(m, errMCPAuthUnavailable)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := auth.Recheck(ctx, m.ID); err != nil {
				c.commandError(m, err)
				return
			}
			c.commandAck(m, "")
		}()
	}
}

// BroadcastMCPServers pushes the current MCP server auth states to every
// browser so an open MCP servers panel follows sign-ins, refreshes, and
// revocations without polling.
func (h *Handler) BroadcastMCPServers() {
	if h.opts.MCPAuth == nil {
		return
	}
	message := map[string]any{"t": "mcp_servers", "servers": h.opts.MCPAuth.List()}
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.send(message)
	}
}
