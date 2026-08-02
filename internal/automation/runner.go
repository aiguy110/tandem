package automation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Report struct {
	WakeAgent    bool            `json:"wakeAgent"`
	AgentProfile string          `json:"agentProfile,omitempty"`
	Context      json.RawMessage `json:"context,omitempty"`
}

// EffectiveAgentProfile applies report-call override semantics to the
// manifest's default wake profile. It returns an empty string for a quiet
// report or when no profile was configured.
func (r Report) EffectiveAgentProfile(manifest Manifest) string {
	if !r.WakeAgent {
		return ""
	}
	if r.AgentProfile != "" {
		return r.AgentProfile
	}
	if manifest.Wake != nil {
		return manifest.Wake.AgentProfile
	}
	return ""
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	Report   *Report
}

// ToolHandler connects tandem:runtime's tools.call(name, arguments) to the
// daemon. Integrators should construct a handler containing only the MCP tools
// granted for this run; the runner deliberately makes no approval decisions.
type ToolHandler func(ctx context.Context, name string, arguments json.RawMessage) (any, error)

// Runner executes trusted TypeScript with ordinary host access. NodeCommand
// and NodeArgs are configurable for managed runtimes and tests. A nil Env
// inherits the daemon environment.
type Runner struct {
	NodeCommand string
	NodeArgs    []string
	Env         []string
	ToolHandler ToolHandler
}

func (r Runner) Run(ctx context.Context, repoRoot, scriptPath string, args []string) (Result, error) {
	full, _, err := ResolveScriptPath(repoRoot, scriptPath)
	if err != nil {
		return Result{}, err
	}
	return r.runFile(ctx, repoRoot, full, args)
}

// Evaluate runs ephemeral TypeScript as though it were a top-level file in the
// repository, preserving normal Node package resolution and host access.
func (r Runner) Evaluate(ctx context.Context, repoRoot string, source []byte, args []string) (Result, error) {
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("resolve repository root: %w", err)
	}
	f, err := os.CreateTemp(realRoot, ".tandem-evaluate-*.ts")
	if err != nil {
		return Result{}, fmt.Errorf("create evaluation script: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(source); err != nil {
		f.Close()
		return Result{}, fmt.Errorf("write evaluation script: %w", err)
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("close evaluation script: %w", err)
	}
	return r.runFile(ctx, realRoot, name, args)
}

func (r Runner) runFile(ctx context.Context, repoRoot, script string, scriptArgs []string) (Result, error) {
	tempDir, err := os.MkdirTemp("", "tandem-automation-runtime-*")
	if err != nil {
		return Result{}, fmt.Errorf("create automation runtime: %w", err)
	}
	defer os.RemoveAll(tempDir)
	markerBytes := make([]byte, 16)
	if _, err := rand.Read(markerBytes); err != nil {
		return Result{}, fmt.Errorf("create report marker: %w", err)
	}
	marker := "__TANDEM_REPORT_" + hex.EncodeToString(markerBytes) + "__"
	toolURL, toolToken, closeTools, err := startToolBridge(ctx, r.ToolHandler)
	if err != nil {
		return Result{}, err
	}
	defer closeTools()
	runtimePath := filepath.Join(tempDir, "runtime.mjs")
	loaderPath := filepath.Join(tempDir, "loader.mjs")
	if err := os.WriteFile(runtimePath, []byte(runtimeModule(marker, toolURL, toolToken)), 0o600); err != nil {
		return Result{}, fmt.Errorf("write automation runtime: %w", err)
	}
	if err := os.WriteFile(loaderPath, []byte(loaderModule(runtimePath)), 0o600); err != nil {
		return Result{}, fmt.Errorf("write automation loader: %w", err)
	}
	command := r.NodeCommand
	if command == "" {
		command = "node"
	}
	argv := append([]string{}, r.NodeArgs...)
	argv = append(argv, "--experimental-transform-types", "--no-warnings", "--experimental-loader", loaderPath, script)
	argv = append(argv, scriptArgs...)
	cmd := exec.CommandContext(ctx, command, argv...)
	cmd.Dir = repoRoot
	if r.Env != nil {
		cmd.Env = r.Env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	err = cmd.Run()
	result := Result{Duration: time.Since(started), ExitCode: 0, Stderr: stderr.String()}
	result.Stdout, result.Report = parseReports(stdout.String(), marker)
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, nil
	}
	return result, fmt.Errorf("start automation script: %w", err)
}

func parseReports(stdout, marker string) (string, *Report) {
	lines := strings.SplitAfter(stdout, "\n")
	kept := make([]string, 0, len(lines))
	var last *Report
	for _, line := range lines {
		payload := strings.TrimSuffix(strings.TrimPrefix(line, marker), "\n")
		payload = strings.TrimSuffix(payload, "\r")
		if strings.HasPrefix(line, marker) {
			var report Report
			if json.Unmarshal([]byte(payload), &report) == nil {
				last = &report
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, ""), last
}

func runtimeModule(marker, toolURL, toolToken string) string {
	markerJSON, _ := json.Marshal(marker)
	urlJSON, _ := json.Marshal(toolURL)
	tokenJSON, _ := json.Marshal(toolToken)
	return fmt.Sprintf(`const marker = %s;
const toolURL = %s;
const toolToken = %s;
export function report(value) {
  if (value === null || typeof value !== "object" || typeof value.wakeAgent !== "boolean") {
    throw new TypeError("report() requires an object with a boolean wakeAgent field");
  }
  if (value.agentProfile !== undefined && (typeof value.agentProfile !== "string" || value.agentProfile.trim() === "")) {
    throw new TypeError("report() agentProfile must be a non-empty string");
  }
  process.stdout.write(marker + JSON.stringify(value) + "\n");
}
async function call(name, args = {}) {
  if (!toolURL) throw new Error("Tandem tools are not available for this run");
  if (typeof name !== "string" || name.trim() === "") throw new TypeError("tool name must be a non-empty string");
  const response = await fetch(toolURL, {
    method: "POST",
    headers: {"authorization": "Bearer " + toolToken, "content-type": "application/json"},
    body: JSON.stringify({name, arguments: args}),
  });
  const body = await response.json();
  if (!response.ok) throw new Error(body.error || "Tandem tool call failed");
  return body.result;
}
function toolNamespace(parts = []) {
  return new Proxy(function () {}, {
    get(_target, property) {
      if (parts.length === 0 && property === "call") return call;
      if (property === "then") return undefined;
      return toolNamespace([...parts, String(property)]);
    },
    apply(_target, _thisArg, values) {
      return call(parts.join("."), values[0] ?? {});
    },
  });
}
export const tools = toolNamespace();
`, markerJSON, urlJSON, tokenJSON)
}

func startToolBridge(ctx context.Context, handler ToolHandler) (url, token string, close func(), err error) {
	if handler == nil {
		return "", "", func() {}, nil
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", nil, fmt.Errorf("create tool bridge token: %w", err)
	}
	token = hex.EncodeToString(tokenBytes)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, fmt.Errorf("listen for automation tool calls: %w", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized Tandem tool call"})
			return
		}
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<20))
		if err := dec.Decode(&call); err != nil || strings.TrimSpace(call.Name) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid Tandem tool call"})
			return
		}
		result, err := handler(req.Context(), call.Name, call.Arguments)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
	})}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), token, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}, nil
}

func loaderModule(runtimePath string) string {
	runtimeURL := "file://" + filepath.ToSlash(runtimePath)
	if filepath.VolumeName(runtimePath) != "" {
		runtimeURL = "file:///" + filepath.ToSlash(runtimePath)
	}
	urlJSON, _ := json.Marshal(runtimeURL)
	return fmt.Sprintf(`const runtimeURL = %s;
export async function resolve(specifier, context, nextResolve) {
  if (specifier === "tandem:runtime") return { url: runtimeURL, shortCircuit: true };
  return nextResolve(specifier, context);
}
`, urlJSON)
}
