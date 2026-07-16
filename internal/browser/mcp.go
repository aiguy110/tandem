package browser

import (
	"os"
	"path/filepath"
)

// MCPServer is an ACP-independent stdio MCP declaration. The ACP agent owns
// the lifecycle of each declared subprocess; Tandem only supplies its launch
// configuration at session/new.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     []MCPEnvVariable
}

type MCPEnvVariable struct{ Name, Value string }

type MCPWiring struct {
	Broker           *Broker
	NodeRuntime      string
	PlaywrightCLI    string
	TandemExecutable string
	ControlURL       string
	Token            string
}

// BuildMCPServers returns the external Playwright MCP and Tandem's internal
// control MCP declarations for one agent. A missing Playwright installation is
// tolerated, matching the Node daemon; tandem-control remains available.
func BuildMCPServers(w MCPWiring, agentID string) []MCPServer {
	servers := make([]MCPServer, 0, 2)
	if w.Broker != nil && w.NodeRuntime != "" && regularFile(w.PlaywrightCLI) {
		servers = append(servers, MCPServer{
			Name: "playwright", Command: w.NodeRuntime,
			Args: []string{w.PlaywrightCLI, "--cdp-endpoint", w.Broker.EndpointFor(agentID)},
			Env:  []MCPEnvVariable{},
		})
	}
	if w.TandemExecutable != "" {
		servers = append(servers, MCPServer{
			Name: "tandem-control", Command: w.TandemExecutable, Args: []string{"mcp-control"},
			Env: []MCPEnvVariable{
				{Name: "TANDEM_CONTROL_URL", Value: w.ControlURL},
				{Name: "TANDEM_TOKEN", Value: w.Token},
				{Name: "TANDEM_AGENT_ID", Value: agentID},
			},
		})
	}
	return servers
}

func regularFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
