package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiguy110/tandem/internal/acp"
	"github.com/aiguy110/tandem/internal/acpadapter"
	"github.com/aiguy110/tandem/internal/agentadapter"
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

func TestSeedCodexModel(t *testing.T) {
	t.Setenv("CODEX_CONFIG", `{"model":"default","model_provider":"gateway","nested":{"keep":true}}`)
	for _, tc := range []struct {
		name, agent, config string
		env                 map[string]string
		wantModel           string
		wantErr             bool
	}{
		{"inherited config", "codex", `{"configOptions":{"model":"saved"}}`, map[string]string{}, "saved", false},
		{"launch override", "codex", `{"configOptions":{"model":"saved"}}`, map[string]string{"CODEX_CONFIG": `{"model":"override","custom":42}`}, "saved", false},
		{"null config", "codex", `{"configOptions":{"model":"saved"}}`, map[string]string{"CODEX_CONFIG": "null"}, "saved", false},
		{"invalid config", "codex", `{"configOptions":{"model":"saved"}}`, map[string]string{"CODEX_CONFIG": "invalid"}, "", true},
		{"other agent", "claude", `{"configOptions":{"model":"saved"}}`, map[string]string{}, "", false},
		{"no model", "codex", `{"configOptions":{"reasoning_effort":"high"}}`, map[string]string{}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.env)
			req := agentadapter.StartRequest{SessionID: "test", ResumeSessionID: "upstream", Spec: agentadapter.Spec{Agent: tc.agent, SessionConfig: json.RawMessage(tc.config)}}
			err := seedCodexModel(req, tc.env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if tc.wantErr {
				return
			}
			if tc.wantModel == "" {
				after, _ := json.Marshal(tc.env)
				if string(before) != string(after) {
					t.Fatal("unexpected env mutation")
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(tc.env["CODEX_CONFIG"]), &got); err != nil {
				t.Fatal(err)
			}
			if got["model"] != tc.wantModel {
				t.Fatalf("model = %v", got["model"])
			}
			if tc.name == "inherited config" && (got["model_provider"] != "gateway" || got["nested"] == nil) {
				t.Fatal("lost inherited config")
			}
			if tc.name == "launch override" && got["custom"] != float64(42) {
				t.Fatal("lost launch config")
			}
			if os.Getenv("CODEX_CONFIG") != `{"model":"default","model_provider":"gateway","nested":{"keep":true}}` {
				t.Fatal("mutated process environment")
			}
		})
	}
}
