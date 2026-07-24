// Package setup implements Tandem's interactive first-run wizard: an
// operator-facing prompt sequence, reachable only from a real terminal (bare
// `tandem` with stdin a tty), that writes the persisted `settings:` block to
// $TANDEM_HOME/config.yml and optionally installs the systemd --user service.
package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/aiguy110/tandem/internal/config"
)

// Run drives the interactive setup wizard: it reads answers from in, prints
// prompts and status to out, and persists the resulting settings under the
// resolved TANDEM_HOME.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	home, err := resolveHome()
	if err != nil {
		return fmt.Errorf("resolve TANDEM_HOME: %w", err)
	}

	existing, err := config.LoadSettings(home)
	if err != nil {
		return fmt.Errorf("load existing settings: %w", err)
	}

	r := bufio.NewReader(in)
	fmt.Fprintln(out, "Tandem setup")
	fmt.Fprintln(out, "------------")
	fmt.Fprintf(out, "Configuring %s. Press enter to accept the bracketed default.\n\n", config.ConfigFilePath(home))

	settings, err := promptSettings(r, out, existing)
	if err != nil {
		return err
	}

	if err := config.SaveSettings(home, settings); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	fmt.Fprintf(out, "\nSettings written to %s\n", config.ConfigFilePath(home))

	if runtime.GOOS == "linux" {
		if _, lookErr := exec.LookPath("systemctl"); lookErr == nil {
			if err := offerServiceInstall(r, out, home, settings); err != nil {
				fmt.Fprintf(out, "warning: systemd service setup failed: %v\n", err)
			}
		}
	}

	return nil
}

func resolveHome() (string, error) {
	if h := os.Getenv("TANDEM_HOME"); h != "" {
		return h, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".tandem"), nil
}

// ask prints prompt with the bracketed default, reads one line from r, and
// returns the trimmed answer or def if the line was empty. On read error
// (including io.EOF) it returns def and the error so callers can decide
// whether to abort or treat it as "accept defaults".
func ask(r *bufio.Reader, out io.Writer, prompt, def string) (string, error) {
	fmt.Fprintf(out, "%s [%s]: ", prompt, def)
	line, err := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && !errors.Is(err, io.EOF) {
		return def, err
	}
	if line == "" {
		if err != nil {
			return def, err
		}
		return def, nil
	}
	return line, nil
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if homeDir, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return homeDir
			}
			return filepath.Join(homeDir, path[2:])
		}
	}
	return path
}

// promptSettings walks the operator through each setting in order. Reaching
// EOF on stdin (e.g. piped/non-interactive input running out) is treated as
// "accept the default for everything remaining": ask() already substitutes
// the default value on EOF, so each step here just needs to stop looping and
// move on rather than re-prompting.
func promptSettings(r *bufio.Reader, out io.Writer, existing config.Settings) (config.Settings, error) {
	var settings config.Settings

	// 1. Project roots.
	rootsDefault := strings.Join(existing.ProjectRoots, ", ")
	if rootsDefault == "" {
		rootsDefault = expandHome("~/Projects")
	}
	rootsAnswer, err := ask(r, out, "Project roots (comma-separated)", rootsDefault)
	if err != nil && !errors.Is(err, io.EOF) {
		return settings, fmt.Errorf("read project roots: %w", err)
	}
	settings.ProjectRoots = splitRoots(rootsAnswer)
	atEOF := errors.Is(err, io.EOF)

	// 2. Bind address.
	bindDefault := existing.Bind
	if bindDefault == "" {
		bindDefault = "127.0.0.1"
	}
	settings.Bind = bindDefault
	if !atEOF {
		bindAnswer, err := ask(r, out, "Bind address", bindDefault)
		if err != nil && !errors.Is(err, io.EOF) {
			return settings, fmt.Errorf("read bind address: %w", err)
		}
		settings.Bind = bindAnswer
		atEOF = errors.Is(err, io.EOF)
	}

	// 3. Port.
	portDefault := existing.Port
	if portDefault == 0 {
		portDefault = 7717
	}
	settings.Port = portDefault
	for !atEOF {
		portAnswer, err := ask(r, out, "Port", strconv.Itoa(portDefault))
		if err != nil && !errors.Is(err, io.EOF) {
			return settings, fmt.Errorf("read port: %w", err)
		}
		atEOF = errors.Is(err, io.EOF)
		port, convErr := strconv.Atoi(portAnswer)
		if convErr != nil {
			fmt.Fprintf(out, "  %q is not a valid port number, try again.\n", portAnswer)
			if atEOF {
				break
			}
			continue
		}
		settings.Port = port
		break
	}

	// 4. Browser driver.
	driverDefault := existing.BrowserDriver
	if driverDefault == "" {
		driverDefault = "local"
	}
	settings.BrowserDriver = driverDefault
	for !atEOF {
		driverAnswer, err := ask(r, out, "Browser driver (local/steel)", driverDefault)
		if err != nil && !errors.Is(err, io.EOF) {
			return settings, fmt.Errorf("read browser driver: %w", err)
		}
		atEOF = errors.Is(err, io.EOF)
		if driverAnswer != "local" && driverAnswer != "steel" {
			fmt.Fprintf(out, "  %q must be \"local\" or \"steel\", try again.\n", driverAnswer)
			if atEOF {
				break
			}
			continue
		}
		settings.BrowserDriver = driverAnswer
		break
	}

	// 5. Node runtime.
	node, err := promptNode(r, out, existing.Node, atEOF)
	if err != nil && !errors.Is(err, io.EOF) {
		return settings, fmt.Errorf("read node runtime: %w", err)
	}
	settings.Node = node

	return settings, nil
}

func splitRoots(answer string) []string {
	parts := strings.Split(answer, ",")
	roots := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		roots = append(roots, expandHome(p))
	}
	return roots
}

// promptNode prompts for the Node runtime choice. If atEOF is already true
// (a prior prompt hit EOF on stdin), it skips straight to the default choice
// rather than attempting another read.
func promptNode(r *bufio.Reader, out io.Writer, existing config.NodeSettings, atEOF bool) (config.NodeSettings, error) {
	nodePath, lookErr := exec.LookPath("node")
	detected := lookErr == nil
	var version string
	if detected {
		if v, err := exec.Command(nodePath, "--version").Output(); err == nil {
			version = strings.TrimSpace(string(v))
		}
	}

	fmt.Fprintln(out, "Node runtime:")
	if detected {
		if version != "" {
			fmt.Fprintf(out, "  1) Use the detected Node at %s (%s)\n", nodePath, version)
		} else {
			fmt.Fprintf(out, "  1) Use the detected Node at %s\n", nodePath)
		}
	} else {
		fmt.Fprintln(out, "  1) Use `node` from PATH at runtime (none detected right now)")
	}
	fmt.Fprintf(out, "  2) Let Tandem download & manage Node (v%s)\n", config.DefaultManagedNodeVersion)

	def := "1"
	if !detected {
		def = "2"
	}
	if existing.Mode == "managed" {
		def = "2"
	} else if existing.Mode == "system" {
		def = "1"
	}

	choice := func(pick string) config.NodeSettings {
		if pick == "2" {
			return config.NodeSettings{Mode: "managed", Version: config.DefaultManagedNodeVersion}
		}
		cmd := nodePath
		if cmd == "" {
			cmd = "node"
		}
		return config.NodeSettings{Mode: "system", Command: cmd}
	}

	if atEOF {
		return choice(def), nil
	}

	for {
		answer, err := ask(r, out, "Choose", def)
		if err != nil && !errors.Is(err, io.EOF) {
			return config.NodeSettings{}, err
		}
		switch answer {
		case "1", "2":
			return choice(answer), nil
		default:
			fmt.Fprintf(out, "  %q is not a valid choice, enter 1 or 2.\n", answer)
			if errors.Is(err, io.EOF) {
				return choice(def), nil
			}
		}
	}
}

func offerServiceInstall(r *bufio.Reader, out io.Writer, home string, settings config.Settings) error {
	answer, err := ask(r, out, "Install and enable the tandem systemd --user service now? [y/N]", "N")
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		return nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	nodeBinDir := ""
	switch settings.Node.Mode {
	case "managed":
		nodeBinDir = filepath.Join(home, "node", "bin")
	default:
		if cmd := settings.Node.Command; cmd != "" && cmd != "node" {
			if abs, lookErr := exec.LookPath(cmd); lookErr == nil {
				nodeBinDir = filepath.Dir(abs)
			} else if filepath.IsAbs(cmd) {
				nodeBinDir = filepath.Dir(cmd)
			}
		} else if abs, lookErr := exec.LookPath("node"); lookErr == nil {
			nodeBinDir = filepath.Dir(abs)
		}
	}

	unit := unitFile(exePath, home, nodeBinDir)

	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("resolve user config dir: %w", err)
	}
	unitDir := filepath.Join(configDir, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", unitDir, err)
	}
	unitPath := filepath.Join(unitDir, "tandem.service")
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}
	fmt.Fprintf(out, "Wrote %s\n", unitPath)

	if err := runSystemctl(out, "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if err := runSystemctl(out, "--user", "enable", "--now", "tandem.service"); err != nil {
		return fmt.Errorf("enable --now: %w", err)
	}

	fmt.Fprintln(out, "\nThe tandem service is installed and running.")
	fmt.Fprintln(out, "  Check status:  systemctl --user status tandem.service")
	fmt.Fprintln(out, "  Follow logs:   journalctl --user -u tandem -f")
	fmt.Fprintln(out, "  To keep it running without an active login session, run:")
	fmt.Fprintln(out, "    loginctl enable-linger $USER")
	return nil
}

func runSystemctl(out io.Writer, args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// unitFile renders the tandem.service systemd unit for a Tandem executable at
// exePath, running with workdir as its working directory (used as TANDEM_HOME
// so relative state lands in the operator's chosen home), and with
// nodeBinDir (if non-empty) prepended to PATH so ACP bridges can find Node.
func unitFile(exePath, workdir, nodeBinDir string) string {
	path := "/usr/local/bin:/usr/bin:/bin"
	if nodeBinDir != "" {
		path = nodeBinDir + ":" + path
	}
	return fmt.Sprintf(`[Unit]
Description=Tandem agent orchestration daemon
After=network.target

[Service]
Type=simple
ExecStart=%s daemon
WorkingDirectory=%s
Environment=TANDEM_HOME=%s
Environment=PATH=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exePath, workdir, workdir, path)
}
