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
	w := MCPWiring{Broker: b, NodeRuntime: "/tools/node", PlaywrightCLI: cli, TandemExecutable: "/bin/tandem", ControlURL: "http://127.0.0.1:7717", Token: "secret"}
	got := BuildMCPServers(w, "api/58")
	if len(got) != 2 {
		t.Fatalf("servers = %#v", got)
	}
	wantOutputDir := filepath.Join(os.TempDir(), "api-58")
	if got[0].Name != "playwright" || got[0].Command != "/tools/node" || !reflect.DeepEqual(got[0].Args, []string{cli, "--cdp-endpoint", b.EndpointFor("api/58"), "--output-dir", wantOutputDir}) {
		t.Fatalf("playwright declaration = %#v", got[0])
	}
	if info, err := os.Stat(wantOutputDir); err != nil || !info.IsDir() {
		t.Fatalf("expected output dir %s to be created: %v", wantOutputDir, err)
	}
	wantEnv := []MCPEnvVariable{{Name: "TANDEM_CONTROL_URL", Value: "http://127.0.0.1:7717"}, {Name: "TANDEM_TOKEN", Value: "secret"}, {Name: "TANDEM_AGENT_ID", Value: "api/58"}}
	if got[1].Name != "tandem-control" || got[1].Command != "/bin/tandem" || !reflect.DeepEqual(got[1].Args, []string{"mcp-control"}) || !reflect.DeepEqual(got[1].Env, wantEnv) {
		t.Fatalf("control declaration = %#v", got[1])
	}
}

func TestBuildMCPServersSkipsMissingPlaywright(t *testing.T) {
	got := BuildMCPServers(MCPWiring{NodeRuntime: "/tools/node", PlaywrightCLI: "/missing/cli.js", TandemExecutable: "/bin/tandem"}, "one")
	if len(got) != 1 || got[0].Name != "tandem-control" {
		t.Fatalf("servers = %#v", got)
	}
}
