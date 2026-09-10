package browser

import (
	"os"
	"path/filepath"
	"strings"
)

// MCPServer is an ACP-independent stdio MCP declaration. The ACP agent owns
// the lifecycle of each declared subprocess; Tandem only supplies its launch
// configuration at session/new.
type MCPServer struct {
	Name    string
	Type    string
	Command string
	Args    []string
	Env     []MCPEnvVariable
	URL     string
	Headers []MCPEnvVariable
}

type MCPEnvVariable struct{ Name, Value string }

// PlaywrightMCPName is deliberately Tandem-namespaced. Claude Code applies
// disabledMcpServers from a user's configuration even to MCP servers supplied
// on its command line. A generic "playwright" name is commonly disabled there,
// which would silently hide Tandem's browser tools.
const PlaywrightMCPName = "tandem-playwright"

type MCPWiring struct {
	Broker           *Broker
	NodeRuntime      string
	PlaywrightCLI    string
	TandemExecutable string
	ControlURL       string
	Token            string
	BrowserEnabled   bool
}

// BuildMCPServers returns the external Playwright MCP and Tandem's internal
// control MCP declarations for one agent. A missing Playwright installation is
// tolerated; tandem-control remains available.
func BuildMCPServers(w MCPWiring, agentID, workspaceCWD string) []MCPServer {
	servers := make([]MCPServer, 0, 3)
	if w.BrowserEnabled && w.Broker != nil && w.NodeRuntime != "" && regularFile(w.PlaywrightCLI) {
		outputDir := filepath.Join(os.TempDir(), sanitizeAgentSlug(agentID))
		_ = os.MkdirAll(outputDir, 0o755)
		servers = append(servers, MCPServer{
			Name: PlaywrightMCPName, Command: w.NodeRuntime,
			Args: []string{w.PlaywrightCLI, "--cdp-endpoint", w.Broker.EndpointFor(agentID), "--output-dir", outputDir},
			Env:  []MCPEnvVariable{},
		})
	}
	if w.BrowserEnabled && w.TandemExecutable != "" {
		servers = append(servers, MCPServer{
			Name: "tandem-control", Command: w.TandemExecutable, Args: []string{"mcp-control"},
			Env: []MCPEnvVariable{
				{Name: "TANDEM_CONTROL_URL", Value: w.ControlURL},
				{Name: "TANDEM_TOKEN", Value: w.Token},
				{Name: "TANDEM_AGENT_ID", Value: agentID},
			},
		})
	}
	if w.TandemExecutable != "" {
		servers = append(servers, MCPServer{
			Name: "tandem-scripts", Command: w.TandemExecutable, Args: []string{"mcp-scripts"},
			Env: []MCPEnvVariable{
				{Name: "TANDEM_CONTROL_URL", Value: w.ControlURL},
				{Name: "TANDEM_TOKEN", Value: w.Token},
				{Name: "TANDEM_AGENT_ID", Value: agentID},
				{Name: "TANDEM_WORKSPACE_CWD", Value: workspaceCWD},
			},
		})
	}
	return servers
}

// sanitizeAgentSlug makes an agent ID safe to use as a single path component,
// since agent IDs may contain "/" (e.g. catalog-derived spawn-option IDs).
func sanitizeAgentSlug(agentID string) string {
	return strings.ReplaceAll(agentID, "/", "-")
}

func regularFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
