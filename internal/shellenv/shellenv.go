// Package shellenv imports the operator's login-shell environment into the
// daemon at startup.
//
// A daemon started by systemd, a desktop launcher, or another supervisor gets a
// bare environment: whatever PATH entries and exports the operator's shell rc
// files add are missing, so agents spawned by Tandem cannot find tools that
// work in the operator's own terminal. Resolve runs $SHELL once as a login,
// interactive shell (the same approach VS Code uses), captures the environment
// it ends up with, and Apply merges it into this process so every child process
// inherits it through os.Environ().
package shellenv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// GuardVar is set to "1" while the shell is resolving so rc files can skip
	// slow or session-hijacking setup: [[ -n $TANDEM_RESOLVING_ENVIRONMENT ]] || exec tmux
	GuardVar = "TANDEM_RESOLVING_ENVIRONMENT"
	// vscodeGuardVar is set as well so rc files already guarded for VS Code's
	// identical resolution step behave the same way for Tandem.
	vscodeGuardVar = "VSCODE_RESOLVING_ENVIRONMENT"
	// MarkerVar carries the random output delimiter to the dump subprocess.
	MarkerVar = "TANDEM_SHELL_ENV_MARKER"
	// DumpArg is the hidden tandem subcommand that prints its environment.
	DumpArg = "__shell-env-dump"

	DefaultTimeout = 10 * time.Second
)

// Options controls one resolution.
type Options struct {
	// Shell is the login shell to run. Empty means $SHELL, then /etc/passwd.
	Shell string
	// DumpCommand is the argv the shell runs to print its environment. Empty
	// means this executable with DumpArg.
	DumpCommand []string
	// Env is the environment the shell starts from. Nil means os.Environ().
	Env     []string
	Timeout time.Duration
}

// Resolve runs the login shell and returns the environment it produced.
func Resolve(ctx context.Context, opts Options) (map[string]string, error) {
	shell := opts.Shell
	if shell == "" {
		shell = loginShell()
	}
	if shell == "" {
		return nil, errors.New("no login shell found in $SHELL or /etc/passwd")
	}
	dump := opts.DumpCommand
	if len(dump) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate tandem executable: %w", err)
		}
		dump = []string{exe, DumpArg}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	base := opts.Env
	if base == nil {
		base = os.Environ()
	}
	marker, err := newMarker()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, append(shellArgs(shell), shellCommand(shell, dump))...)
	cmd.Env = append(append([]string(nil), base...), GuardVar+"=1", vscodeGuardVar+"=1", MarkerVar+"="+marker)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Own session: no controlling terminal for interactive-shell job control to
	// grab, and one process group to kill on timeout.
	configureDetached(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	// Background programs started by rc files (ssh-agent, gpg-agent) can hold
	// our pipes open long after the shell exits; don't wait on them.
	cmd.WaitDelay = time.Second

	runErr := cmd.Run()
	env, parseErr := parseDump(stdout.Bytes(), marker)
	if parseErr == nil {
		// ErrWaitDelay and a non-zero exit from a noisy rc are fine once the
		// dump itself came through intact.
		if errors.Is(runErr, exec.ErrWaitDelay) {
			slog.Info("login shell exited but a program it started kept its output open; stopped waiting", "shell", shell, "wait_delay", cmd.WaitDelay)
		} else if runErr != nil {
			slog.Debug("login shell exited with an error after reporting its environment", "shell", shell, "error", runErr)
		}
		return env, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s did not finish within %s (guard slow rc code with $%s): %s", shell, timeout, GuardVar, tail(stderr.String()))
	}
	if runErr != nil {
		return nil, fmt.Errorf("%s: %w: %s", shell, runErr, tail(stderr.String()))
	}
	return nil, fmt.Errorf("%s: %w: %s", shell, parseErr, tail(stderr.String()))
}

// Dump implements DumpArg: write the environment as JSON between markers.
func Dump(stdout *os.File) error {
	marker := os.Getenv(MarkerVar)
	if marker == "" {
		return errors.New(MarkerVar + " is not set")
	}
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		// The marker must not appear inside the payload it delimits.
		if k, v, ok := strings.Cut(entry, "="); ok && k != MarkerVar {
			env[k] = v
		}
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s%s%s", marker, data, marker)
	return err
}

// Result describes what Merge changed.
type Result struct {
	// Set are variables newly added from the shell.
	Set map[string]string
	// PathAdded are PATH entries the shell contributed.
	PathAdded []string
	// Path is the merged PATH ("" when unchanged).
	Path string
}

// Merge computes what to import from the shell into current. Variables the
// daemon already has win, so explicit service configuration (systemd
// Environment=, TANDEM_*) is never overridden. PATH is the exception: the
// shell's entries come first, followed by any daemon entries it lacks.
func Merge(current, shell map[string]string) Result {
	res := Result{Set: map[string]string{}}
	for k, v := range shell {
		if k == "PATH" || skip(k) {
			continue
		}
		if _, ok := current[k]; ok {
			continue
		}
		res.Set[k] = v
	}
	shellPath := splitPath(shell["PATH"])
	if len(shellPath) == 0 {
		return res
	}
	have := map[string]bool{}
	for _, dir := range splitPath(current["PATH"]) {
		have[dir] = true
	}
	merged := make([]string, 0, len(shellPath))
	seen := map[string]bool{}
	for _, dir := range shellPath {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		merged = append(merged, dir)
		if !have[dir] {
			res.PathAdded = append(res.PathAdded, dir)
		}
	}
	for _, dir := range splitPath(current["PATH"]) {
		if !seen[dir] {
			seen[dir] = true
			merged = append(merged, dir)
		}
	}
	if joined := strings.Join(merged, string(os.PathListSeparator)); joined != current["PATH"] {
		res.Path = joined
	}
	return res
}

// Apply resolves the shell environment and merges it into this process. It is
// controlled by TANDEM_SHELL_ENV (off disables it; force resolves even when
// started from a terminal) and TANDEM_SHELL_ENV_TIMEOUT. Failures are logged
// and leave the environment untouched.
func Apply(ctx context.Context, startedFromTerminal bool) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("TANDEM_SHELL_ENV")))
	switch {
	case mode == "off" || mode == "0" || mode == "false":
		slog.Info("shell environment import disabled", "setting", "TANDEM_SHELL_ENV="+mode)
		return
	case startedFromTerminal && mode != "force":
		slog.Debug("shell environment import skipped: started from a terminal, environment already inherited")
		return
	case os.Getenv(GuardVar) != "":
		// We are ourselves running inside a resolving shell; never recurse.
		return
	}
	timeout := DefaultTimeout
	if raw := strings.TrimSpace(os.Getenv("TANDEM_SHELL_ENV_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			timeout = d
		} else {
			slog.Warn("invalid TANDEM_SHELL_ENV_TIMEOUT; using default", "value", raw, "default", DefaultTimeout)
		}
	}
	shell := loginShell()
	if shell == "" {
		slog.Info("shell environment import skipped: no login shell in $SHELL or /etc/passwd")
		return
	}
	start := time.Now()
	env, err := Resolve(ctx, Options{Shell: shell, Timeout: timeout})
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		slog.Warn("shell environment import failed; agents get the daemon's own environment", "shell", shell, "duration", elapsed, "error", err)
		return
	}
	current := map[string]string{}
	for _, entry := range os.Environ() {
		if k, v, ok := strings.Cut(entry, "="); ok {
			current[k] = v
		}
	}
	res := Merge(current, env)
	names := make([]string, 0, len(res.Set))
	for k, v := range res.Set {
		if err := os.Setenv(k, v); err != nil {
			slog.Warn("shell environment import: set variable failed", "name", k, "error", err)
			continue
		}
		names = append(names, k)
	}
	if res.Path != "" {
		if err := os.Setenv("PATH", res.Path); err != nil {
			slog.Warn("shell environment import: set PATH failed", "error", err)
		}
	}
	// Names only: values can hold credentials.
	slog.Info("imported login shell environment", "shell", shell, "duration", elapsed, "vars_added", names, "path_added", res.PathAdded)
	if elapsed > 3*time.Second {
		slog.Warn("login shell startup is slow and delays daemon startup; guard slow rc code with $"+GuardVar, "shell", shell, "duration", elapsed)
	}
}

// skip reports shell-session bookkeeping and Tandem's own control variables,
// which must not leak from the resolving shell into agents.
func skip(name string) bool {
	switch name {
	case "PWD", "OLDPWD", "SHLVL", "_", "TERM", "COLUMNS", "LINES", "PS1", "PS2", "PS4",
		GuardVar, vscodeGuardVar, MarkerVar:
		return true
	}
	return strings.HasPrefix(name, "TANDEM_")
}

func shellArgs(shell string) []string {
	switch strings.TrimSuffix(filepath.Base(shell), ".exe") {
	case "csh", "tcsh":
		// csh only treats argv[0]-prefixed "-" as login; -l can't combine with -c.
		return []string{"-i", "-c"}
	case "pwsh", "powershell":
		return []string{"-Login", "-Command"}
	default: // sh, bash, zsh, ksh, dash, fish, nu
		return []string{"-i", "-l", "-c"}
	}
}

func shellCommand(shell string, argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	cmd := strings.Join(quoted, " ")
	switch strings.TrimSuffix(filepath.Base(shell), ".exe") {
	case "pwsh", "powershell":
		return "& " + cmd
	}
	return cmd
}

func loginShell() string {
	if s := strings.TrimSpace(os.Getenv("SHELL")); s != "" {
		return s
	}
	return passwdShell()
}

func parseDump(out []byte, marker string) (map[string]string, error) {
	m := []byte(marker)
	start := bytes.Index(out, m)
	if start < 0 {
		return nil, errors.New("environment dump missing from shell output")
	}
	rest := out[start+len(m):]
	end := bytes.Index(rest, m)
	if end < 0 {
		return nil, errors.New("environment dump truncated")
	}
	var env map[string]string
	if err := json.Unmarshal(rest[:end], &env); err != nil {
		return nil, fmt.Errorf("decode environment dump: %w", err)
	}
	return env, nil
}

func splitPath(p string) []string {
	var out []string
	for _, dir := range filepath.SplitList(p) {
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

func newMarker() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate marker: %w", err)
	}
	return "--tandem-env-" + hex.EncodeToString(b) + "--", nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = "…" + s[len(s)-500:]
	}
	if s == "" {
		return "(no stderr)"
	}
	return s
}
