// Package config resolves Tandem's environment, runtime paths, agent catalog,
// profiles, and bearer token.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tandem "github.com/aiguy110/tandem"
	"gopkg.in/yaml.v3"
)

type Launch struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env,omitempty"`
}

type ResumeLaunch struct {
	Cmd       string            `json:"cmd"`
	Args      []string          `json:"args"`
	StartArgs []string          `json:"startArgs,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

type Agent struct {
	Name     string        `json:"name"`
	ACP      *Launch       `json:"acp,omitempty"`
	Terminal *ResumeLaunch `json:"terminal,omitempty"`
}

// Harness is a named launch variant of an agent (extra ACP/terminal args). It is
// the config-file "how to launch" concept; distinct from a Profile, which is the
// daemon-owned, user-facing bundle of harness + model/effort/permissions + snapshot.
type Harness struct {
	Agent        string   `json:"agent"`
	Name         string   `json:"name"`
	ACPArgs      []string `json:"acpArgs"`
	TerminalArgs []string `json:"terminalArgs"`
}

type ACPConfig struct {
	Default  string            `json:"default"`
	Agents   map[string]Launch `json:"agents"`
	Override *Launch           `json:"override,omitempty"`
}

type BrowserConfig struct {
	Driver              string         `json:"driver"`
	UserDataRoot        string         `json:"userDataRoot"`
	SnapshotRoot        string         `json:"snapshotRoot"`
	ChromiumExecutable  string         `json:"chromiumExecutable,omitempty"`
	SteelBaseURL        string         `json:"steelBaseUrl,omitempty"`
	SteelAPIKey         string         `json:"steelApiKey,omitempty"`
	SteelSessionOptions map[string]any `json:"steelSessionOptions"`
	MCPEnabled          bool           `json:"mcpEnabled"`
	NodeRuntime         string         `json:"nodeRuntime"`
	PlaywrightMCPCLI    string         `json:"playwrightMcpCli"`
}

type Config struct {
	Home           string                  `json:"home"`
	RuntimeRoot    string                  `json:"runtimeRoot"`
	ManagedRuntime bool                    `json:"managedRuntime"`
	DBPath         string                  `json:"dbPath"`
	TokenPath      string                  `json:"tokenPath"`
	WorktreesDir   string                  `json:"worktreesDir"`
	AssetsDir      string                  `json:"assetsDir"`
	Host           string                  `json:"host"`
	Port           int                     `json:"port"`
	UIDir          string                  `json:"uiDir,omitempty"`
	ProjectRoots   []string                `json:"projectRoots"`
	DirScanDepth   int                     `json:"dirScanDepth"`
	ACP            ACPConfig               `json:"acp"`
	ResumeCLI      map[string]ResumeLaunch `json:"resumeCli"`
	Agents         map[string]Agent        `json:"agents"`
	Harnesses      map[string]Harness      `json:"harnesses"`
	DefaultHarness string                  `json:"defaultHarness,omitempty"`
	Browser        BrowserConfig           `json:"browser"`
}

// Options makes environment and platform-dependent lookup deterministic in tests.
type Options struct {
	Env         map[string]string
	HomeDir     string
	RuntimeRoot string
	TandemRoot  string
}

func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return Config{}, err
	}
	root := filepath.Dir(exe)
	managedRuntime := true
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		if _, statErr := os.Stat(filepath.Join(cwd, "config.yml.example")); statErr == nil {
			root = cwd
			managedRuntime = false
		}
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if managedRuntime {
		runtimeRoot = filepath.Join(value(environ(), "TANDEM_HOME", filepath.Join(home, ".tandem")), "runtime")
	}
	cfg, err := LoadWithOptions(Options{Env: environ(), HomeDir: home, RuntimeRoot: runtimeRoot, TandemRoot: root})
	if err != nil {
		return Config{}, err
	}
	cfg.ManagedRuntime = managedRuntime
	return cfg, nil
}

func environ() map[string]string {
	m := make(map[string]string)
	for _, entry := range os.Environ() {
		if k, v, ok := strings.Cut(entry, "="); ok {
			m[k] = v
		}
	}
	return m
}

func LoadWithOptions(o Options) (Config, error) {
	env := o.Env
	if env == nil {
		env = map[string]string{}
	}
	home := value(env, "TANDEM_HOME", filepath.Join(o.HomeDir, ".tandem"))
	if err := os.MkdirAll(home, 0o755); err != nil {
		return Config{}, fmt.Errorf("create TANDEM_HOME: %w", err)
	}
	port, err := envInt(env, "TANDEM_PORT", 7717)
	if err != nil {
		return Config{}, err
	}
	depth, err := envInt(env, "TANDEM_DIR_SCAN_DEPTH", 1)
	if err != nil {
		return Config{}, err
	}

	cat, err := loadCatalog(o, home)
	if err != nil {
		return Config{}, err
	}
	roots := filepath.SplitList(value(env, "TANDEM_PROJECT_ROOTS", filepath.Join(o.HomeDir, "Projects")))
	roots = nonempty(roots)
	override, err := optionalLaunch(env["TANDEM_ACP_CMD"])
	if err != nil {
		return Config{}, fmt.Errorf("TANDEM_ACP_CMD: %w", err)
	}
	driver := "local"
	if env["TANDEM_BROWSER_DRIVER"] == "steel" {
		driver = "steel"
	}
	steelOptions, err := objectJSON(env["STEEL_SESSION_OPTIONS"])
	if err != nil {
		return Config{}, err
	}

	nodeRuntime := resolveExecutable(value(env, "TANDEM_NODE_CMD", "node"), o)
	acpAgents := make(map[string]Launch)
	resume := make(map[string]ResumeLaunch)
	for id, agent := range cat.agents {
		if agent.ACP != nil {
			acpAgents[id] = *agent.ACP
		}
		if agent.Terminal != nil {
			resume[id] = *agent.Terminal
		}
	}
	return Config{
		Home: home, RuntimeRoot: o.RuntimeRoot,
		DBPath: filepath.Join(home, "tandem.db"), TokenPath: filepath.Join(home, "token"),
		WorktreesDir: filepath.Join(home, "worktrees"), AssetsDir: filepath.Join(home, "assets"),
		Host: value(env, "TANDEM_BIND", "127.0.0.1"), Port: port, UIDir: env["TANDEM_UI_DIR"],
		ProjectRoots: roots, DirScanDepth: depth,
		ACP:       ACPConfig{Default: cat.defaultAgent, Agents: acpAgents, Override: override},
		ResumeCLI: resume, Agents: cat.agents, Harnesses: cat.harnesses, DefaultHarness: cat.defaultHarness,
		Browser: BrowserConfig{Driver: driver, UserDataRoot: filepath.Join(home, "browser-profiles"), SnapshotRoot: filepath.Join(home, "browser-snapshots"), ChromiumExecutable: env["TANDEM_CHROMIUM_EXECUTABLE"], SteelBaseURL: env["STEEL_BASE_URL"], SteelAPIKey: env["STEEL_API_KEY"], SteelSessionOptions: steelOptions, MCPEnabled: env["TANDEM_BROWSER_MCP"] != "off", NodeRuntime: nodeRuntime, PlaywrightMCPCLI: filepath.Join(o.RuntimeRoot, "node_modules", "@playwright", "mcp", "cli.js")},
	}, nil
}

type fileConfig struct {
	Defaults  map[string]any `yaml:"defaults"`
	Agents    map[string]any `yaml:"agents"`
	Harnesses map[string]any `yaml:"harnesses"`
}

type catalog struct {
	agents                       map[string]Agent
	harnesses                    map[string]Harness
	defaultAgent, defaultHarness string
}

func loadCatalog(o Options, home string) (catalog, error) {
	shipped, err := parseFile(tandem.DefaultCatalog, "shipped agent catalog")
	if err != nil {
		return catalog{}, err
	}
	user := fileConfig{}
	file := filepath.Join(home, "config.yml")
	if b, readErr := os.ReadFile(file); readErr == nil {
		user, err = parseFile(b, file)
		if err != nil {
			return catalog{}, err
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return catalog{}, readErr
	}

	subs := map[string]string{"node": resolveExecutable(value(o.Env, "TANDEM_NODE_CMD", "node"), o), "runtimeRoot": o.RuntimeRoot, "tandemRoot": o.TandemRoot, "home": home}
	agents := make(map[string]Agent)
	if err := mergeAgents(agents, shipped.Agents, subs, o); err != nil {
		return catalog{}, err
	}
	if err := mergeAgents(agents, user.Agents, subs, o); err != nil {
		return catalog{}, err
	}
	for id, definition := range agents {
		envID := strings.Map(func(r rune) rune {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				return r
			}
			return '_'
		}, strings.ToUpper(id))
		if raw := o.Env["TANDEM_ACP_CMD_"+envID]; raw != "" {
			launch, e := parseLaunch(raw)
			if e != nil {
				return catalog{}, e
			}
			definition.ACP = &launch
		}
		if raw := o.Env["TANDEM_RESUME_CMD_"+envID]; raw != "" {
			launch, e := parseLaunch(raw)
			if e != nil {
				return catalog{}, e
			}
			terminal := ResumeLaunch{}
			if definition.Terminal != nil {
				terminal = *definition.Terminal
			}
			terminal.Cmd, terminal.Args = resolveExecutable(launch.Cmd, o), launch.Args
			definition.Terminal = &terminal
		}
		agents[id] = definition
	}
	harnessesRaw := cloneMap(shipped.Harnesses)
	for k, v := range user.Harnesses {
		harnessesRaw[k] = v
	}
	harnesses := make(map[string]Harness)
	for id, raw := range harnessesRaw {
		m, ok := stringAnyMap(raw)
		if !ok {
			return catalog{}, fmt.Errorf("harnesses.%s must be a mapping", id)
		}
		agent, ok := m["agent"].(string)
		if !ok || agent == "" {
			return catalog{}, fmt.Errorf("harnesses.%s.agent must be a non-empty string", id)
		}
		if _, ok := agents[agent]; !ok {
			return catalog{}, fmt.Errorf("harnesses.%s references unknown agent: %s", id, agent)
		}
		acpArgs, e := stringArray(m["acpArgs"], "harnesses."+id+".acpArgs")
		if e != nil {
			return catalog{}, e
		}
		terminalArgs, e := stringArray(m["terminalArgs"], "harnesses."+id+".terminalArgs")
		if e != nil {
			return catalog{}, e
		}
		harnesses[id] = Harness{Agent: agent, Name: stringValue(m["name"], id), ACPArgs: acpArgs, TerminalArgs: terminalArgs}
	}
	defaultAgent := defaultValue(user.Defaults, shipped.Defaults, "agent")
	if defaultAgent == "" {
		return catalog{}, errors.New("shipped agent catalog must configure defaults.agent")
	}
	if _, ok := agents[defaultAgent]; !ok {
		return catalog{}, fmt.Errorf("defaults.agent references unknown agent: %s", defaultAgent)
	}
	defaultHarness := defaultValue(user.Defaults, shipped.Defaults, "harness")
	if defaultHarness != "" {
		if _, ok := harnesses[defaultHarness]; !ok {
			return catalog{}, fmt.Errorf("defaults.harness references unknown harness: %s", defaultHarness)
		}
	}
	return catalog{agents, harnesses, defaultAgent, defaultHarness}, nil
}

func parseFile(data []byte, name string) (fileConfig, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fileConfig{}, fmt.Errorf("invalid %s: %w", name, err)
	}
	m, ok := stringAnyMap(raw)
	if !ok {
		return fileConfig{}, fmt.Errorf("invalid %s: root must be a mapping", name)
	}
	f := fileConfig{}
	if v, exists := m["defaults"]; exists {
		var yes bool
		f.Defaults, yes = stringAnyMap(v)
		if !yes {
			return f, fmt.Errorf("invalid %s: defaults must be a mapping", name)
		}
	}
	if v, exists := m["agents"]; exists {
		var yes bool
		f.Agents, yes = stringAnyMap(v)
		if !yes {
			return f, fmt.Errorf("invalid %s: agents must be a mapping", name)
		}
	}
	// Accept "harnesses" (current) and fall back to the legacy "profiles" key so
	// pre-rename user config.yml files keep working.
	harnessKey := "harnesses"
	if _, exists := m["harnesses"]; !exists {
		if _, legacy := m["profiles"]; legacy {
			harnessKey = "profiles"
		}
	}
	if v, exists := m[harnessKey]; exists {
		var yes bool
		f.Harnesses, yes = stringAnyMap(v)
		if !yes {
			return f, fmt.Errorf("invalid %s: %s must be a mapping", name, harnessKey)
		}
	}
	return f, nil
}

func mergeAgents(dst map[string]Agent, entries map[string]any, subs map[string]string, o Options) error {
	for id, raw := range entries {
		m, ok := stringAnyMap(raw)
		if !ok {
			return fmt.Errorf("agents.%s must be a mapping", id)
		}
		previous := dst[id]
		agent := previous
		agent.Name = stringValue(m["name"], previous.Name)
		if agent.Name == "" {
			agent.Name = id
		}
		if rawACP, exists := m["acp"]; exists {
			v, ok := stringAnyMap(rawACP)
			if !ok {
				return fmt.Errorf("agents.%s.acp must be a mapping", id)
			}
			cmd, e := requiredString(v["command"], "agents."+id+".acp.command")
			if e != nil {
				return e
			}
			args, e := stringArray(v["args"], "agents."+id+".acp.args")
			if e != nil {
				return e
			}
			env, e := stringMap(v["env"], "agents."+id+".acp.env")
			if e != nil {
				return e
			}
			l := Launch{Cmd: resolveExecutable(expand(cmd, subs), o), Args: expandAll(args, subs), Env: expandMap(env, subs)}
			agent.ACP = &l
		}
		if rawTerminal, exists := m["terminal"]; exists {
			v, ok := stringAnyMap(rawTerminal)
			if !ok {
				return fmt.Errorf("agents.%s.terminal must be a mapping", id)
			}
			cmd, e := requiredString(v["command"], "agents."+id+".terminal.command")
			if e != nil {
				return e
			}
			start, e := stringArray(v["startArgs"], "agents."+id+".terminal.startArgs")
			if e != nil {
				return e
			}
			resume, e := stringArray(v["resumeArgs"], "agents."+id+".terminal.resumeArgs")
			if e != nil {
				return e
			}
			env, e := stringMap(v["env"], "agents."+id+".terminal.env")
			if e != nil {
				return e
			}
			l := ResumeLaunch{Cmd: resolveExecutable(expand(cmd, subs), o), Args: expandAll(resume, subs), StartArgs: expandAll(start, subs), Env: expandMap(env, subs)}
			agent.Terminal = &l
		}
		if agent.ACP == nil && agent.Terminal == nil {
			return fmt.Errorf("agents.%s must configure acp or terminal", id)
		}
		dst[id] = agent
	}
	return nil
}

func EnsureToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return EnsureToken(path)
	}
	if err != nil {
		return "", err
	}
	if _, err = f.WriteString(token); err == nil {
		err = f.Chmod(0o600)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return token, err
}

func Redacted(c Config) Config {
	out := c
	if out.Browser.SteelAPIKey != "" {
		out.Browser.SteelAPIKey = "[REDACTED]"
	}
	redactLaunch := func(l Launch) Launch { l.Env = redactMap(l.Env); return l }
	redactResume := func(l ResumeLaunch) ResumeLaunch { l.Env = redactMap(l.Env); return l }
	out.ACP.Agents = make(map[string]Launch, len(c.ACP.Agents))
	for k, v := range c.ACP.Agents {
		out.ACP.Agents[k] = redactLaunch(v)
	}
	if c.ACP.Override != nil {
		v := redactLaunch(*c.ACP.Override)
		out.ACP.Override = &v
	}
	out.ResumeCLI = make(map[string]ResumeLaunch, len(c.ResumeCLI))
	for k, v := range c.ResumeCLI {
		out.ResumeCLI[k] = redactResume(v)
	}
	out.Agents = make(map[string]Agent, len(c.Agents))
	for k, a := range c.Agents {
		if a.ACP != nil {
			v := redactLaunch(*a.ACP)
			a.ACP = &v
		}
		if a.Terminal != nil {
			v := redactResume(*a.Terminal)
			a.Terminal = &v
		}
		out.Agents[k] = a
	}
	out.Browser.SteelSessionOptions = redactObject(c.Browser.SteelSessionOptions)
	return out
}

func DebugJSON(c Config) ([]byte, error) { return json.MarshalIndent(Redacted(c), "", "  ") }

func redactMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = "[REDACTED]"
	}
	return out
}
func redactObject(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "apikey") || strings.Contains(lower, "api_key") {
			out[k] = "[REDACTED]"
			continue
		}
		switch nested := v.(type) {
		case map[string]any:
			out[k] = redactObject(nested)
		case []any:
			items := make([]any, len(nested))
			for i, item := range nested {
				if object, ok := item.(map[string]any); ok {
					items[i] = redactObject(object)
				} else {
					items[i] = item
				}
			}
			out[k] = items
		default:
			out[k] = v
		}
	}
	return out
}
func value(m map[string]string, key, fallback string) string {
	if m[key] != "" {
		return m[key]
	}
	return fallback
}
func envInt(m map[string]string, key string, fallback int) (int, error) {
	raw := m[key]
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %q", key, raw)
	}
	return v, nil
}
func nonempty(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range in {
		out[k] = v
	}
	return out
}
func stringAnyMap(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
func stringValue(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}
func defaultValue(primary, fallback map[string]any, key string) string {
	if v, ok := primary[key].(string); ok {
		return v
	}
	if v, ok := fallback[key].(string); ok {
		return v
	}
	return ""
}
func requiredString(v any, key string) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return s, nil
}
func stringArray(v any, key string) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	out := make([]string, len(raw))
	for i, x := range raw {
		var yes bool
		out[i], yes = x.(string)
		if !yes {
			return nil, fmt.Errorf("%s must be an array of strings", key)
		}
	}
	return out, nil
}
func stringMap(v any, key string) (map[string]string, error) {
	if v == nil {
		return map[string]string{}, nil
	}
	raw, ok := stringAnyMap(v)
	if !ok {
		return nil, fmt.Errorf("%s must be a mapping of string values", key)
	}
	out := make(map[string]string, len(raw))
	for k, x := range raw {
		s, yes := x.(string)
		if !yes {
			return nil, fmt.Errorf("%s must be a mapping of string values", key)
		}
		out[k] = s
	}
	return out, nil
}
func expand(s string, subs map[string]string) string {
	for k, v := range subs {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
func expandAll(in []string, subs map[string]string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = expand(v, subs)
	}
	return out
}
func expandMap(in map[string]string, subs map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = expand(v, subs)
	}
	return out
}
func resolveExecutable(cmd string, o Options) string {
	if strings.ContainsRune(cmd, filepath.Separator) {
		return cmd
	}
	dirs := append(filepath.SplitList(o.Env["PATH"]), filepath.Join(o.HomeDir, ".local", "bin"), filepath.Join(o.HomeDir, "bin"))
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		p := filepath.Join(d, cmd)
		if info, e := os.Stat(p); e == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return cmd
}
func parseLaunch(raw string) (Launch, error) {
	var parts []any
	if json.Unmarshal([]byte(raw), &parts) == nil && len(parts) > 0 {
		out := Launch{Cmd: fmt.Sprint(parts[0]), Args: make([]string, len(parts)-1)}
		for i, v := range parts[1:] {
			out.Args[i] = fmt.Sprint(v)
		}
		return out, nil
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return Launch{}, errors.New("launch command is empty")
	}
	return Launch{Cmd: fields[0], Args: fields[1:]}, nil
}
func optionalLaunch(raw string) (*Launch, error) {
	if raw == "" {
		return nil, nil
	}
	v, e := parseLaunch(raw)
	return &v, e
}
func objectJSON(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("STEEL_SESSION_OPTIONS must be valid JSON: %w", err)
	}
	if v == nil {
		return nil, errors.New("STEEL_SESSION_OPTIONS must be a JSON object")
	}
	return v, nil
}
