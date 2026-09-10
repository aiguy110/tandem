package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type catalogGolden struct {
	DefaultAgent   string             `json:"defaultAgent"`
	DefaultHarness string             `json:"defaultHarness,omitempty"`
	Agents         map[string]Agent   `json:"agents"`
	Harnesses      map[string]Harness `json:"harnesses"`
}

func options(t *testing.T, env map[string]string) Options {
	t.Helper()
	home := t.TempDir()
	if env == nil {
		env = map[string]string{}
	}
	env["TANDEM_HOME"] = home
	return Options{Env: env, HomeDir: "/fixtures/user", RuntimeRoot: "/fixtures/tandem/runtime", TandemRoot: "/fixtures/tandem"}
}

func TestDefaultsAndEnvironment(t *testing.T) {
	o := options(t, map[string]string{
		"TANDEM_NODE_CMD": "/fixtures/bin/node", "TANDEM_BIND": "0.0.0.0", "TANDEM_PORT": "8123",
		"TANDEM_UI_DIR": "/ui", "TANDEM_PROJECT_ROOTS": filepath.Join("", "one") + string(os.PathListSeparator) + filepath.Join("", "two"),
		"TANDEM_DIR_SCAN_DEPTH": "3", "TANDEM_BROWSER_DRIVER": "steel", "TANDEM_BROWSER_MCP": "off",
		"TANDEM_CHROMIUM_EXECUTABLE": "/fixtures/bin/chromium",
		"STEEL_BASE_URL":             "https://steel.invalid", "STEEL_API_KEY": "secret", "STEEL_SESSION_OPTIONS": `{"width":1280}`,
		"TANDEM_ACP_CMD": `["mock","--stdio"]`, "TANDEM_ACP_CMD_CODEX": "codex-custom --acp",
		"TANDEM_RESUME_CMD_PI": `["pi-custom","--session","{sessionId}"]`,
	})
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "0.0.0.0" || c.Port != 8123 || c.DirScanDepth != 3 || c.UIDir != "/ui" {
		t.Fatalf("environment not applied: %+v", c)
	}
	if c.ACP.Default != "claude" || len(c.Agents) != 4 || c.Agents["claude"].Name != "Claude" ||
		c.Agents["opencode"].Name != "OpenCode" {
		t.Fatalf("shipped catalog not loaded: %+v", c.Agents)
	}
	if c.Agents["claude"].ACP.Cmd != "/fixtures/bin/node" {
		t.Fatalf("explicit node launcher = %q", c.Agents["claude"].ACP.Cmd)
	}
	if c.Agents["codex"].ACP.Cmd != "codex-custom" || c.Agents["pi"].Terminal.Cmd != "pi-custom" {
		t.Fatalf("legacy overrides missing: %+v", c.Agents)
	}
	if c.ACP.Override == nil || c.ACP.Override.Cmd != "mock" {
		t.Fatalf("global ACP override missing: %+v", c.ACP.Override)
	}
	if c.Browser.Driver != "steel" || c.Browser.MCPEnabled || c.Browser.SteelSessionOptions["width"] != float64(1280) {
		t.Fatalf("browser env missing: %+v", c.Browser)
	}
	if c.Browser.ChromiumExecutable != "/fixtures/bin/chromium" {
		t.Fatalf("explicit chromium executable missing: %+v", c.Browser)
	}
	if c.Browser.NodeRuntime != "/fixtures/bin/node" || c.Browser.PlaywrightMCPCLI != "/fixtures/tandem/runtime/node_modules/@playwright/mcp/cli.js" {
		t.Fatalf("browser MCP tool runtime missing: %+v", c.Browser)
	}
	if c.DBPath != filepath.Join(c.Home, "tandem.db") || c.TokenPath != filepath.Join(c.Home, "token") {
		t.Fatalf("home derivation incorrect: %+v", c)
	}
}

func TestDefaultVoicePreparationInstructionsExpandTechnicalNotation(t *testing.T) {
	instructions := DefaultVoicePreparationInstructions
	if len(strings.Fields(instructions)) >= 100 {
		t.Fatalf("default voice instructions should remain concise: %d words", len(strings.Fields(instructions)))
	}
	for _, want := range []string{
		"Expand abbreviations, symbols, and compact technical notation",
		"MiB/token",
		"megabytes per token",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("default voice instructions missing %q: %q", want, instructions)
		}
	}
}

func TestSettingsFilePrecedence(t *testing.T) {
	o := options(t, nil)
	home := o.Env["TANDEM_HOME"]
	s := Settings{
		ProjectRoots:  []string{"/work/a", "/work/b"},
		Bind:          "0.0.0.0",
		Port:          9000,
		BrowserDriver: "steel",
		SteelBaseURL:  "https://steel.example",
		SteelAPIKey:   "file-secret",
		Node:          NodeSettings{Mode: "managed", Version: "20.10.0"},
		LanguageModel: LanguageModelSettings{Endpoint: "http://clean/v1/chat/completions", APIKey: "clean-secret", Model: "cleaner"},
		Voice:         VoiceSettings{Enabled: true, TTSEndpoint: "http://speak/v1/audio/speech", TTSAPIKey: "tts-secret", TTSModel: "speaker", TTSVoice: "voice", TTSFormat: "wav"},
	}
	if err := SaveSettings(home, s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ConfigFilePath(home))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %o, want 600", info.Mode().Perm())
	}
	got, err := LoadSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round-trip settings = %+v, want %+v", got, s)
	}
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "0.0.0.0" || c.Port != 9000 || c.Browser.Driver != "steel" || c.Browser.SteelBaseURL != "https://steel.example" || c.Browser.SteelAPIKey != "file-secret" {
		t.Fatalf("settings not applied: host=%q port=%d browser=%+v", c.Host, c.Port, c.Browser)
	}
	if !reflect.DeepEqual(c.ProjectRoots, []string{"/work/a", "/work/b", c.HomeBaseDir}) {
		t.Fatalf("project roots = %v", c.ProjectRoots)
	}
	if c.LanguageModel.Model != "cleaner" || c.LanguageModel.Endpoint != "http://clean/v1/chat/completions" || !c.Voice.Enabled || c.Voice.TTSVoice != "voice" || c.Voice.TTSFormat != "wav" {
		t.Fatalf("language/voice settings not applied: language=%+v voice=%+v", c.LanguageModel, c.Voice)
	}
	redacted := Redacted(c)
	if redacted.LanguageModel.APIKey != "[REDACTED]" || redacted.Voice.TTSAPIKey != "[REDACTED]" {
		t.Fatalf("language/voice credentials not redacted: language=%+v voice=%+v", redacted.LanguageModel, redacted.Voice)
	}
	wantNode, wantNpm := ManagedNodePaths(filepath.Join(home, "node"))
	if !c.Node.Managed || c.Node.Version != "20.10.0" || c.Node.Command != wantNode || c.Node.Npm != wantNpm {
		t.Fatalf("managed node not resolved: %+v", c.Node)
	}
	if c.Agents["claude"].ACP.Cmd != wantNode {
		t.Fatalf("managed node not fed into acp {node}: %q", c.Agents["claude"].ACP.Cmd)
	}
}

func TestHomeBaseRootAlwaysSurfaced(t *testing.T) {
	o := options(t, map[string]string{"TANDEM_PROJECT_ROOTS": "/env/root"})
	home := o.Env["TANDEM_HOME"]
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	wantHomeBase := filepath.Join(home, "home-base")
	if c.HomeBaseDir != wantHomeBase {
		t.Fatalf("HomeBaseDir = %q, want %q", c.HomeBaseDir, wantHomeBase)
	}
	if !reflect.DeepEqual(c.ProjectRoots, []string{"/env/root", wantHomeBase}) {
		t.Fatalf("home base not appended to roots: %v", c.ProjectRoots)
	}
	// The home base is registered by its exact path, never TANDEM_HOME, so the
	// spawn-palette scan can never reach the daemon's own worktrees dir.
	for _, root := range c.ProjectRoots {
		if root == home || root == c.WorktreesDir {
			t.Fatalf("scan root %q would expose internal $TANDEM_HOME contents", root)
		}
	}
}

func TestHomeBaseRootDeduped(t *testing.T) {
	o := options(t, nil)
	home := o.Env["TANDEM_HOME"]
	homeBase := filepath.Join(home, "home-base")
	// A user who already lists the home base must not get it twice.
	o.Env["TANDEM_PROJECT_ROOTS"] = homeBase
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.ProjectRoots, []string{homeBase}) {
		t.Fatalf("home base duplicated in roots: %v", c.ProjectRoots)
	}
}

func TestEnvOverridesSettings(t *testing.T) {
	o := options(t, map[string]string{
		"TANDEM_PORT": "7000", "TANDEM_BIND": "10.0.0.1",
		"TANDEM_PROJECT_ROOTS": "/env/root", "TANDEM_NODE_CMD": "/env/bin/node",
		"TANDEM_VOICE_ENABLED": "true", "TANDEM_LANGUAGE_MODEL_ENDPOINT": "http://env/clean",
		"TANDEM_LANGUAGE_MODEL_MODEL": "env-cleaner", "TANDEM_VOICE_TTS_ENDPOINT": "http://env/speak",
		"TANDEM_VOICE_TTS_MODEL": "env-tts", "TANDEM_VOICE_TTS_VOICE": "env-voice",
	})
	home := o.Env["TANDEM_HOME"]
	if err := SaveSettings(home, Settings{Port: 9000, Bind: "0.0.0.0", ProjectRoots: []string{"/file/root"}, Node: NodeSettings{Mode: "managed"}}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 7000 || c.Host != "10.0.0.1" {
		t.Fatalf("env did not override settings: port=%d host=%q", c.Port, c.Host)
	}
	if !reflect.DeepEqual(c.ProjectRoots, []string{"/env/root", c.HomeBaseDir}) {
		t.Fatalf("env roots not applied: %v", c.ProjectRoots)
	}
	if c.Node.Managed || c.Node.Command != "/env/bin/node" {
		t.Fatalf("TANDEM_NODE_CMD should force system node: %+v", c.Node)
	}
	if !c.Voice.Enabled || c.LanguageModel.Model != "env-cleaner" || c.Voice.TTSModel != "env-tts" || c.Voice.TTSVoice != "env-voice" {
		t.Fatalf("language/voice environment overrides missing: language=%+v voice=%+v", c.LanguageModel, c.Voice)
	}
}

func TestLegacyVoiceCleanupSettingsMigrateToSharedLanguageModel(t *testing.T) {
	o := options(t, nil)
	home := o.Env["TANDEM_HOME"]
	if err := SaveSettings(home, Settings{Voice: VoiceSettings{
		Enabled: true, LegacyCleanupEndpoint: "http://legacy/clean", LegacyCleanupAPIKey: "legacy-key", LegacyCleanupModel: "legacy-model",
		TTSEndpoint: "http://legacy/speech", TTSModel: "tts", TTSVoice: "voice",
	}}); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if c.LanguageModel.Endpoint != "http://legacy/clean" || c.LanguageModel.APIKey != "legacy-key" || c.LanguageModel.Model != "legacy-model" {
		t.Fatalf("legacy language model = %+v", c.LanguageModel)
	}
}

func TestOverlayExpansionProfilesAndPartialAgentMerge(t *testing.T) {
	o := options(t, map[string]string{"TANDEM_NODE_CMD": "/fixtures/bin/node"})
	config := `
defaults: {agent: custom, harness: custom-fast}
agents:
  claude:
    name: Renamed Claude
  custom:
    name: Custom Agent
    acp:
      command: "{node}"
      args: ["{runtimeRoot}/mock.mjs", "{tandemRoot}"]
      env: {HOME_PATH: "{home}"}
    history:
      parser: "{runtimeRoot}/history/custom.ts"
      args: ["--root", "{home}"]
      env: {CUSTOM_TOKEN: "secret"}
      resume: terminal
harnesses:
  custom-fast:
    agent: custom
    name: Custom Fast
    acpArgs: [--fast]
    terminalArgs: []
`
	if err := os.WriteFile(filepath.Join(o.Env["TANDEM_HOME"], "config.yml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if c.ACP.Default != "custom" || c.DefaultHarness != "custom-fast" {
		t.Fatalf("defaults = %q/%q", c.ACP.Default, c.DefaultHarness)
	}
	if c.Agents["claude"].ACP == nil || c.Agents["claude"].Terminal == nil || c.Agents["claude"].Name != "Renamed Claude" {
		t.Fatalf("partial overlay did not inherit: %+v", c.Agents["claude"])
	}
	custom := c.Agents["custom"].ACP
	if custom.Cmd != "/fixtures/bin/node" || custom.Args[0] != "/fixtures/tandem/runtime/mock.mjs" || custom.Env["HOME_PATH"] != c.Home {
		t.Fatalf("placeholders not expanded: %+v", custom)
	}
	history := c.Agents["custom"].History
	if history == nil || history.Parser != "/fixtures/tandem/runtime/history/custom.ts" ||
		!history.Enabled || history.Resume != "terminal" ||
		!reflect.DeepEqual(history.Args, []string{"--root", c.Home}) ||
		history.Env["CUSTOM_TOKEN"] != "secret" {
		t.Fatalf("history config not resolved: %+v", history)
	}
	if got := Redacted(c).Agents["custom"].History.Env["CUSTOM_TOKEN"]; got != "[REDACTED]" {
		t.Fatalf("history environment not redacted: %q", got)
	}
	if !reflect.DeepEqual(c.Harnesses["custom-fast"].ACPArgs, []string{"--fast"}) {
		t.Fatalf("harness not loaded: %+v", c.Harnesses)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	tests := []struct{ name, body, contains string }{
		{"malformed YAML", "agents: [", "invalid"},
		{"missing command", "agents:\n  broken:\n    acp:\n      args: []\n", "agents.broken.acp.command must be a non-empty string"},
		{"bad argument list", "agents:\n  broken:\n    terminal:\n      command: node\n      startArgs: nope\n", "agents.broken.terminal.startArgs must be an array of strings"},
		{"bad environment", "agents:\n  broken:\n    acp:\n      command: node\n      env: {COUNT: 3}\n", "agents.broken.acp.env must be a mapping of string values"},
		{"relative history parser", "agents:\n  broken:\n    terminal: {command: x}\n    history: {parser: relative.ts}\n", "history.parser must resolve to an absolute path"},
		{"bad history resume", "agents:\n  broken:\n    terminal: {command: x}\n    history: {parser: /tmp/x.ts, resume: magic}\n", "history.resume must be auto, acp, or terminal"},
		{"bad history enabled", "agents:\n  broken:\n    terminal: {command: x}\n    history: {parser: /tmp/x.ts, enabled: yes-please}\n", "history.enabled must be a boolean"},
		{"unknown harness agent", "harnesses:\n  bad: {agent: missing}\n", "harnesses.bad references unknown agent: missing"},
		{"unknown default harness", "defaults: {harness: absent}\n", "defaults.harness references unknown harness: absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := options(t, nil)
			if err := os.WriteFile(filepath.Join(o.Env["TANDEM_HOME"], "config.yml"), []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadWithOptions(o)
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("error = %v, want containing %q", err, tt.contains)
			}
		})
	}
	for _, env := range []map[string]string{{"TANDEM_PORT": "nope"}, {"STEEL_SESSION_OPTIONS": "[]"}} {
		o := options(t, env)
		if _, err := LoadWithOptions(o); err == nil {
			t.Fatalf("LoadWithOptions(%v) succeeded", env)
		}
	}
}

func TestExecutableResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable mode test")
	}
	bin := t.TempDir()
	executable := filepath.Join(bin, "agent-cli")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := options(t, map[string]string{"PATH": bin, "TANDEM_NODE_CMD": "agent-cli"})
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Agents["claude"].ACP.Cmd; got != executable {
		t.Fatalf("resolved command = %q, want %q", got, executable)
	}
}

func TestEnsureTokenCreationReuseAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token")
	token, err := EnsureToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64", len(token))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := EnsureToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if again != token {
		t.Fatalf("token was not reused")
	}
	info, _ = os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("reused token mode = %o", info.Mode().Perm())
	}
}

func TestDebugJSONIsDeterministicAndRedacted(t *testing.T) {
	o := options(t, map[string]string{"STEEL_API_KEY": "steel-secret", "STEEL_SESSION_OPTIONS": `{"nested":{"apiKey":"option-secret"}}`, "TANDEM_ACP_CMD": `["mock"]`})
	if err := os.WriteFile(filepath.Join(o.Env["TANDEM_HOME"], "config.yml"), []byte("agents:\n  custom:\n    acp:\n      command: custom\n      env: {PASSWORD: hidden}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	one, err := DebugJSON(c)
	if err != nil {
		t.Fatal(err)
	}
	two, _ := DebugJSON(c)
	if string(one) != string(two) {
		t.Fatal("debug JSON is not deterministic")
	}
	if strings.Contains(string(one), "steel-secret") || strings.Contains(string(one), "option-secret") || strings.Contains(string(one), "hidden") || !strings.Contains(string(one), "[REDACTED]") {
		t.Fatalf("debug JSON did not redact secrets: %s", one)
	}
	var decoded Config
	if err := json.Unmarshal(one, &decoded); err != nil {
		t.Fatalf("invalid debug JSON: %v", err)
	}
}

func TestCatalogMatchesPhaseZeroGolden(t *testing.T) {
	fixturePath := filepath.Join("testdata", "config-examples.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var examples struct {
		Overlay json.RawMessage `json:"overlay"`
	}
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	o := options(t, map[string]string{"TANDEM_NODE_CMD": "/fixtures/bin/node"})
	if err := os.WriteFile(filepath.Join(o.Env["TANDEM_HOME"], "config.yml"), examples.Overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	// Normalize the one intentionally runtime-specific placeholder.
	custom := c.Agents["custom"]
	custom.ACP.Env["FIXTURE_HOME"] = "{home}"
	c.Agents["custom"] = custom
	actual, err := json.MarshalIndent(catalogGolden{c.ACP.Default, c.DefaultHarness, c.Agents, c.Harnesses}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')
	wantPath := filepath.Join("testdata", "config-normalized.json")
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, want) {
		t.Fatalf("normalized catalog differs from golden\nactual:\n%s\nwant:\n%s", actual, want)
	}
}

func TestCheckCompatibilityAt(t *testing.T) {
	err := CheckCompatibilityAt(Settings{ConfigVersion: 0}, 1)
	var compatibilityErr *CompatibilityError
	if !errors.As(err, &compatibilityErr) {
		t.Fatalf("CheckCompatibilityAt error = %v, want CompatibilityError", err)
	}
	if compatibilityErr.Have != 0 || compatibilityErr.Need != 1 {
		t.Fatalf("CompatibilityError = %+v", compatibilityErr)
	}
	if err := CheckCompatibilityAt(Settings{ConfigVersion: 1}, 1); err != nil {
		t.Fatalf("current config rejected: %v", err)
	}
}

// A recorded system-node path can go stale (fnm's per-shell shim directory is
// deleted when that shell exits). Spawns must fall back to PATH, not fail.
func TestStaleSystemNodePathFallsBackToPath(t *testing.T) {
	bin := t.TempDir()
	nodePath := filepath.Join(bin, "node")
	if err := os.WriteFile(nodePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := options(t, map[string]string{"PATH": bin})
	settings := "version: 1\nsettings:\n  node:\n    mode: system\n    command: /run/user/1000/fnm_multishells/gone/bin/node\n"
	if err := os.WriteFile(filepath.Join(o.Env["TANDEM_HOME"], "config.yml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	if c.Node.Command != nodePath {
		t.Fatalf("stale node path should fall back to PATH node, got %q", c.Node.Command)
	}
}

func TestStableExecutablePathResolvesEphemeralShims(t *testing.T) {
	real := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := StableExecutablePath(real); got != real {
		t.Fatalf("durable path should be left alone, got %q", got)
	}
	shim := filepath.Join(t.TempDir(), "node")
	if err := os.Symlink(real, shim); err != nil {
		t.Fatal(err)
	}
	// t.TempDir() lives under /tmp, which counts as ephemeral.
	if got := StableExecutablePath(shim); got != real {
		t.Fatalf("ephemeral shim should resolve to %q, got %q", real, got)
	}
}

func TestMCPServersMergeGlobalAndProjectOverrides(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	if err := os.WriteFile(ConfigFilePath(home), []byte("mcpServers:\n  global:\n    command: global-command\n  overridden:\n    command: global-version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := ProjectMCPConfigPath(project)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("mcpServers:\n  local:\n    command: local-command\n  overridden:\n    command: project-version\n    args: [--fast]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	servers, err := LoadMCPServers(home, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 3 || servers["global"].Command != "global-command" || servers["local"].Command != "local-command" || servers["overridden"].Command != "project-version" || !reflect.DeepEqual(servers["overridden"].Args, []string{"--fast"}) {
		t.Fatalf("resolved MCP servers = %#v", servers)
	}
}

func TestAddMCPServerPreservesGlobalConfiguration(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(ConfigFilePath(home), []byte("settings:\n  port: 7718\nagents:\n  example: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := AddMCPServer(home, "", "files", MCPServer{Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"}}, false)
	if err != nil || path != ConfigFilePath(home) {
		t.Fatalf("AddMCPServer = %q, %v", path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "settings:") || !strings.Contains(string(b), "agents:") {
		t.Fatalf("unrelated config was lost: %s", b)
	}
	servers, err := LoadMCPServers(home, "")
	if err != nil || servers["files"].Command != "npx" {
		t.Fatalf("stored MCP server = %#v, %v", servers, err)
	}
}
