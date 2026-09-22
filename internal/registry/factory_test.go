package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiguy110/tandem/internal/acp"
	"github.com/aiguy110/tandem/internal/acpadapter"
	"github.com/aiguy110/tandem/internal/config"
)

func TestWriteMCPServersFileIsOwnerOnlyACPShape(t *testing.T) {
	home := t.TempDir()
	f := DefaultFactory{Config: config.Config{Home: home}}
	servers := []acpadapter.MCPServer{
		{Name: "tandem-control", Command: "/bin/tandem", Args: []string{"mcp-control"}, Env: []acp.EnvVariable{{Name: "TANDEM_TOKEN", Value: "secret"}}},
		{Name: "remote", Type: "http", URL: "https://example.test/mcp"},
	}
	path, err := f.writeMCPServersFile("proj/agent-1", servers)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(home, "run", "mcp") {
		t.Fatalf("servers file %q is outside TANDEM_HOME/run/mcp", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("servers file mode = %v, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["command"] != "/bin/tandem" || got[1]["type"] != "http" || got[1]["url"] != "https://example.test/mcp" {
		t.Fatalf("servers file = %s", raw)
	}
	if _, ok := got[1]["headers"]; !ok {
		t.Fatalf("http server lost its ACP-required headers array: %s", raw)
	}
	removeMCPServersFile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("servers file still present after removal: %v", err)
	}
}

func TestWriteMCPServersFileSkipsEmptyList(t *testing.T) {
	f := DefaultFactory{Config: config.Config{Home: t.TempDir()}}
	path, err := f.writeMCPServersFile("agent", nil)
	if err != nil || path != "" {
		t.Fatalf("writeMCPServersFile(nil) = %q, %v; want no file", path, err)
	}
}
