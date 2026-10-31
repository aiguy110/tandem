package browser

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildMCPServersUsesConfiguredNodeAndPerAgentBrokerURL(t *testing.T) {
	b := NewBroker(&fakeDriver{provisions: map[string]int{}, teardowns: map[string]int{}}, BrokerConfig{})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Stop(t.Context()) })
	cli := filepath.Join(t.TempDir(), "cli.js")
	if err := os.WriteFile(cli, []byte("// fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := MCPWiring{Broker: b, JSRuntime: "/tools/node", PlaywrightCLI: cli, TandemExecutable: "/bin/tandem", ControlURL: "http://127.0.0.1:7717", Token: "secret", BrowserEnabled: true}
	routed := BuildMCPServers(w, "api/58", "/worktrees/api-58")
	wantRouterEnv := []MCPEnvVariable{{Name: "TANDEM_CONTROL_URL", Value: "http://127.0.0.1:7717"}, {Name: "TANDEM_TOKEN", Value: "secret"}, {Name: "TANDEM_AGENT_ID", Value: "api/58"}, {Name: "TANDEM_JS_RUNTIME", Value: "/tools/node"}, {Name: "TANDEM_PLAYWRIGHT_MCP_CLI", Value: cli}, {Name: "TANDEM_BROWSER_CDP_BASE", Value: b.CDPBase()}, {Name: "TANDEM_BROWSER_OUTPUT_DIR", Value: filepath.Join(os.TempDir(), "api-58")}}
	if len(routed) != 5 || routed[0].Name != BrowserMCPName || routed[0].Command != "/bin/tandem" || !reflect.DeepEqual(routed[0].Args, []string{"mcp-browser"}) || !reflect.DeepEqual(routed[0].Env, wantRouterEnv) {
		t.Fatalf("router declaration = %#v", routed)
	}
	w.DirectPlaywright = true
	got := BuildMCPServers(w, "api/58", "/worktrees/api-58")
	if len(got) != 5 {
		t.Fatalf("servers = %#v", got)
	}
	wantOutputDir := filepath.Join(os.TempDir(), "api-58")
	if got[0].Name != PlaywrightMCPName || got[0].Command != "/tools/node" || !reflect.DeepEqual(got[0].Args, []string{cli, "--cdp-endpoint", b.EndpointFor("api/58"), "--output-dir", wantOutputDir}) {
		t.Fatalf("playwright declaration = %#v", got[0])
	}
	if info, err := os.Stat(wantOutputDir); err != nil || !info.IsDir() {
		t.Fatalf("expected output dir %s to be created: %v", wantOutputDir, err)
	}
	wantEnv := []MCPEnvVariable{{Name: "TANDEM_CONTROL_URL", Value: "http://127.0.0.1:7717"}, {Name: "TANDEM_TOKEN", Value: "secret"}, {Name: "TANDEM_AGENT_ID", Value: "api/58"}, {Name: "TANDEM_BROWSER_ENABLED", Value: "true"}}
	if got[1].Name != "tandem-control" || got[1].Command != "/bin/tandem" || !reflect.DeepEqual(got[1].Args, []string{"mcp-control"}) || !reflect.DeepEqual(got[1].Env, wantEnv) {
		t.Fatalf("control declaration = %#v", got[1])
	}
	wantScriptEnv := append(append([]MCPEnvVariable{}, wantEnv[:3]...), MCPEnvVariable{Name: "TANDEM_WORKSPACE_CWD", Value: "/worktrees/api-58"})
	if got[2].Name != "tandem-scripts" || !reflect.DeepEqual(got[2].Args, []string{"mcp-scripts"}) || !reflect.DeepEqual(got[2].Env, wantScriptEnv) {
		t.Fatalf("scripts declaration = %#v", got[2])
	}
	if got[3].Name != "tandem-messages" || !reflect.DeepEqual(got[3].Args, []string{"mcp-messages"}) || !reflect.DeepEqual(got[3].Env, wantEnv[:3]) {
		t.Fatalf("messages declaration = %#v", got[3])
	}
	if got[4].Name != ArtifactsMCPName || !reflect.DeepEqual(got[4].Args, []string{"mcp-artifacts"}) || !reflect.DeepEqual(got[4].Env, wantScriptEnv) {
		t.Fatalf("artifacts declaration = %#v", got[4])
	}
}

func TestBuildMCPServersSkipsMissingPlaywright(t *testing.T) {
	got := BuildMCPServers(MCPWiring{JSRuntime: "/tools/node", PlaywrightCLI: "/missing/cli.js", TandemExecutable: "/bin/tandem", BrowserEnabled: true}, "one", "/repo")
	if len(got) != 4 || got[0].Name != "tandem-control" || got[1].Name != "tandem-scripts" || got[2].Name != "tandem-messages" || got[3].Name != ArtifactsMCPName {
		t.Fatalf("servers = %#v", got)
	}
}

func TestBuildMCPServersIncludesScriptsAndMessagesWhenBrowserMCPDisabled(t *testing.T) {
	got := BuildMCPServers(MCPWiring{TandemExecutable: "/bin/tandem"}, "one", "/repo")
	if len(got) != 4 || got[0].Name != "tandem-control" || got[1].Name != "tandem-scripts" || got[2].Name != "tandem-messages" || got[3].Name != ArtifactsMCPName {
		t.Fatalf("servers = %#v", got)
	}
}
