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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

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

	// 4. Shared browser.
	browserSettings, browserEOF, err := promptBrowser(r, out, existing, atEOF)
	if err != nil {
		return settings, err
	}
	settings.BrowserDriver = browserSettings.BrowserDriver
	settings.SteelBaseURL = browserSettings.SteelBaseURL
	settings.SteelAPIKey = browserSettings.SteelAPIKey
	atEOF = browserEOF

	// 5. Node runtime.
	node, err := promptNode(r, out, existing.Node, atEOF)
	if err != nil && !errors.Is(err, io.EOF) {
		return settings, fmt.Errorf("read node runtime: %w", err)
	}
	settings.Node = node

	return settings, nil
}

func promptBrowser(r *bufio.Reader, out io.Writer, existing config.Settings, atEOF bool) (config.Settings, bool, error) {
	result := config.Settings{BrowserDriver: "local"}
	chromium, _ := discoverChromium()

	fmt.Fprintln(out, "\nShared browser:")
	if chromium != "" {
		fmt.Fprintf(out, "  1) Local Chrome/Chromium (recommended; detected %s)\n", chromium)
	} else {
		fmt.Fprintln(out, "  1) Local Chrome/Chromium (none detected; install Chrome/Chromium before using browser tools)")
	}
	fmt.Fprintln(out, "  2) Steel browser service (local Docker container, remote server, or Steel Cloud)")

	def := "1"
	if existing.BrowserDriver == "steel" {
		def = "2"
	}
	if atEOF {
		if def == "2" && existing.SteelBaseURL != "" {
			result.BrowserDriver = "steel"
			result.SteelBaseURL = existing.SteelBaseURL
			result.SteelAPIKey = existing.SteelAPIKey
		}
		printBrowserSummary(out, result, chromium)
		return result, true, nil
	}

	for {
		answer, err := ask(r, out, "Choose", def)
		if err != nil && !errors.Is(err, io.EOF) {
			return result, false, fmt.Errorf("read browser choice: %w", err)
		}
		eof := errors.Is(err, io.EOF)
		switch answer {
		case "1", "local":
			printBrowserSummary(out, result, chromium)
			return result, eof, nil
		case "2", "steel":
			steel, steelEOF, err := promptSteel(r, out, existing, eof)
			if err != nil {
				return result, false, err
			}
			if steel.BrowserDriver == "local" {
				printBrowserSummary(out, steel, chromium)
			}
			return steel, steelEOF, nil
		default:
			fmt.Fprintf(out, "  %q is not a valid choice, enter 1 or 2.\n", answer)
			if eof {
				printBrowserSummary(out, result, chromium)
				return result, true, nil
			}
		}
	}
}

func promptSteel(r *bufio.Reader, out io.Writer, existing config.Settings, atEOF bool) (config.Settings, bool, error) {
	result := config.Settings{BrowserDriver: "local"}
	dockerPath, dockerErr := exec.LookPath("docker")
	dockerReady := false
	if dockerErr == nil {
		check := exec.Command(dockerPath, "info", "--format", "{{.ServerVersion}}")
		check.Stdout = io.Discard
		check.Stderr = io.Discard
		dockerReady = check.Run() == nil
	}

	fmt.Fprintln(out, "\nSteel service:")
	if dockerReady {
		fmt.Fprintln(out, "  1) Start and manage a local Steel container with Docker (Docker is ready)")
	} else if dockerErr == nil {
		fmt.Fprintln(out, "  1) Start and manage a local Steel container with Docker (Docker is installed, but its daemon is unavailable)")
	} else {
		fmt.Fprintln(out, "  1) Start and manage a local Steel container with Docker (Docker is not installed)")
	}
	fmt.Fprintln(out, "  2) Connect to an existing Steel service")
	fmt.Fprintln(out, "  3) Go back and use local Chrome/Chromium")

	if atEOF {
		return result, true, nil
	}
	for {
		answer, err := ask(r, out, "Choose", "2")
		if err != nil && !errors.Is(err, io.EOF) {
			return result, false, fmt.Errorf("read Steel provider: %w", err)
		}
		eof := errors.Is(err, io.EOF)
		switch answer {
		case "1":
			if !dockerReady {
				fmt.Fprintln(out, "  Docker is not ready. Start/install Docker, connect to an existing Steel service, or choose local Chrome/Chromium.")
				if eof {
					return result, true, nil
				}
				continue
			}
			fmt.Fprintln(out, "Starting Tandem's Steel container as `tandem-steel`...")
			if err := startSteelContainer(dockerPath); err != nil {
				fmt.Fprintf(out, "  Could not start Steel: %v\n", err)
				if eof {
					return result, true, nil
				}
				continue
			}
			baseURL := "http://localhost:3000"
			if err := waitForSteel(baseURL, "", 20*time.Second); err != nil {
				fmt.Fprintf(out, "  Steel container started, but its API is not ready: %v\n", err)
				if eof {
					return result, true, nil
				}
				continue
			}
			result.BrowserDriver = "steel"
			result.SteelBaseURL = baseURL
			printBrowserSummary(out, result, "")
			return result, eof, nil
		case "2":
			steel, urlEOF, err := promptExistingSteel(r, out, existing, eof)
			return steel, urlEOF, err
		case "3":
			return result, eof, nil
		default:
			fmt.Fprintf(out, "  %q is not a valid choice, enter 1, 2, or 3.\n", answer)
			if eof {
				return result, true, nil
			}
		}
	}
}

func promptExistingSteel(r *bufio.Reader, out io.Writer, existing config.Settings, atEOF bool) (config.Settings, bool, error) {
	result := config.Settings{BrowserDriver: "local"}
	if atEOF {
		return result, true, nil
	}
	urlDefault := existing.SteelBaseURL
	if urlDefault == "" {
		urlDefault = "http://localhost:3000"
	}
	for {
		baseURL, err := ask(r, out, "Steel base URL", urlDefault)
		if err != nil && !errors.Is(err, io.EOF) {
			return result, false, fmt.Errorf("read Steel base URL: %w", err)
		}
		eof := errors.Is(err, io.EOF)
		parsed, parseErr := url.ParseRequestURI(baseURL)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			fmt.Fprintf(out, "  %q is not a valid http(s) URL, try again.\n", baseURL)
			if eof {
				return result, true, nil
			}
			continue
		}
		apiKey, keyErr := promptSteelAPIKey(r, out, existing.SteelAPIKey)
		if keyErr != nil && !errors.Is(keyErr, io.EOF) {
			return result, false, fmt.Errorf("read Steel API key: %w", keyErr)
		}
		eof = eof || errors.Is(keyErr, io.EOF)
		fmt.Fprintf(out, "Checking %s...\n", strings.TrimRight(baseURL, "/"))
		if checkErr := waitForSteel(baseURL, apiKey, 5*time.Second); checkErr != nil {
			fmt.Fprintf(out, "  Could not reach Steel: %v\n", checkErr)
			if eof {
				return result, true, nil
			}
			retry, retryErr := ask(r, out, "Retry URL, save anyway, or use local? (retry/save/local)", "retry")
			if retryErr != nil && !errors.Is(retryErr, io.EOF) {
				return result, false, retryErr
			}
			switch strings.ToLower(retry) {
			case "save":
			case "local":
				return result, errors.Is(retryErr, io.EOF), nil
			default:
				continue
			}
		}
		result.BrowserDriver = "steel"
		result.SteelBaseURL = strings.TrimRight(baseURL, "/")
		result.SteelAPIKey = apiKey
		printBrowserSummary(out, result, "")
		return result, eof, nil
	}
}

func promptSteelAPIKey(r *bufio.Reader, out io.Writer, existing string) (string, error) {
	if existing != "" {
		keep, err := ask(r, out, "Keep the existing Steel API key? (y/N)", "y")
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if strings.EqualFold(keep, "y") || strings.EqualFold(keep, "yes") {
			return existing, err
		}
	}
	return ask(r, out, "Steel API key (optional; input is visible, config is owner-only)", "")
}

func discoverChromium() (string, error) {
	names := []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"}
	if runtime.GOOS == "darwin" {
		names = append([]string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}, names...)
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Chrome/Chromium not found")
}

func startSteelContainer(dockerPath string) error {
	inspect := exec.Command(dockerPath, "inspect", "tandem-steel")
	inspect.Stdout = io.Discard
	inspect.Stderr = io.Discard
	if inspect.Run() == nil {
		update := exec.Command(dockerPath, "update", "--restart", "unless-stopped", "tandem-steel")
		update.Stdout = io.Discard
		update.Stderr = io.Discard
		if err := update.Run(); err != nil {
			return fmt.Errorf("set tandem-steel restart policy: %w", err)
		}
		out, err := exec.Command(dockerPath, "start", "tandem-steel").CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker start tandem-steel: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	args := []string{"run", "-d", "--name", "tandem-steel", "--restart", "unless-stopped", "--shm-size=2g",
		"-p", "3000:3000", "-p", "9223:9223", "-e", "CHROME_HEADLESS=false", "-e", "DISPLAY=:10",
		"--entrypoint", "/bin/sh", "ghcr.io/steel-dev/steel-browser:latest",
		"-c", "Xvfb :10 -screen 0 1920x1080x24 -nolisten tcp & exec /app/api/entrypoint.sh"}
	out, err := exec.Command(dockerPath, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run tandem-steel: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func waitForSteel(baseURL, apiKey string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for {
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/", nil)
		if err != nil {
			return err
		}
		if apiKey != "" {
			req.Header.Set("steel-api-key", apiKey)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < http.StatusInternalServerError {
				return nil
			}
			lastErr = fmt.Errorf("server returned %s", resp.Status)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func printBrowserSummary(out io.Writer, settings config.Settings, chromium string) {
	if settings.BrowserDriver == "steel" {
		auth := "no API key"
		if settings.SteelAPIKey != "" {
			auth = "API key configured"
		}
		fmt.Fprintf(out, "Browser readiness: Steel at %s (%s).\n", settings.SteelBaseURL, auth)
		return
	}
	if chromium != "" {
		fmt.Fprintf(out, "Browser readiness: local Chrome/Chromium at %s.\n", chromium)
	} else {
		fmt.Fprintln(out, "Browser readiness: local selected; Chrome/Chromium must be installed before browser tools are used.")
	}
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
