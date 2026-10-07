// Package browser provides browser lifecycle drivers and the agent-facing CDP broker.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ProvisionResult struct{ CDPURL string }

type Driver interface {
	Kind() string
	Provision(context.Context, string) (ProvisionResult, error)
	Teardown(context.Context, string) error
	IsProvisioned(string) bool
	PID(string) int
}

type DriverConfig struct {
	Driver              string
	UserDataRoot        string
	ChromiumExecutable  string
	SteelBaseURL        string
	SteelAPIKey         string
	SteelSessionOptions map[string]any
	HTTPClient          *http.Client
	// SessionStore, when set, persists Steel sessions for re-attach across a
	// daemon restart. Ignored by the local driver (its Chromium dies with us).
	SessionStore SessionStore
}

func NewDriver(cfg DriverConfig) (Driver, error) {
	switch cfg.Driver {
	case "", "local":
		return NewLocalDriver(LocalConfig{UserDataRoot: cfg.UserDataRoot, Executable: cfg.ChromiumExecutable}), nil
	case "steel":
		return NewSteelDriver(SteelConfig{BaseURL: cfg.SteelBaseURL, APIKey: cfg.SteelAPIKey, SessionOptions: cfg.SteelSessionOptions, Client: cfg.HTTPClient, Store: cfg.SessionStore})
	default:
		return nil, fmt.Errorf("unknown browser driver %q", cfg.Driver)
	}
}

// DiscoverChromium resolves a configured executable, then well-known browser names.
// It deliberately does not depend on Playwright or its private browser cache.
func DiscoverChromium(configured string) (string, error) {
	if configured != "" {
		p, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("configured chromium executable %q: %w", configured, err)
		}
		return p, nil
	}
	// Branded Chrome first: its UA brands, codecs, and fingerprint match what
	// sites see from real users, whereas Chromium is a mild automation tell.
	names := []string{"google-chrome-stable", "google-chrome", "chrome", "chromium", "chromium-browser"}
	if runtime.GOOS == "darwin" {
		names = append([]string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium"}, names...)
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("chromium executable not found; configure TANDEM_CHROMIUM_EXECUTABLE")
}

// localNetworkAccessArgs disables Chrome's Local Network Access checks for the
// daemon-owned browser. LNA gates public-origin subresource requests to loopback
// and private IPs behind a permission prompt; headless Chrome has no permission
// UI, so the prompt auto-denies and any page that talks to a local agent (device
// compliance/posture checks, dev servers, printer and IoT consoles) breaks with
// no way for the user to grant access from the shared-browser pane.
var localNetworkAccessArgs = []string{"--disable-features=LocalNetworkAccessChecks"}

// stealthArgs strip the launch-level signals bot detectors key on: the
// AutomationControlled blink feature (navigator.webdriver = true) and the
// "Chrome is being controlled by automated test software" infobar.
var stealthArgs = []string{"--disable-blink-features=AutomationControlled", "--disable-infobars", "--test-type"}

// localLaunchArgs builds the Chromium command line for a daemon-owned browser
// listening for CDP on port and using profile as its user-data-dir. When
// userAgent is non-empty it overrides the browser's default (used in headless
// mode to hide the "HeadlessChrome" product token).
func localLaunchArgs(port int, profile string, headless bool, userAgent string) []string {
	args := []string{"--disable-gpu", "--disable-dev-shm-usage",
		fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check", "--window-size=1280,800", "--window-position=0,0"}
	if headless {
		args = append([]string{"--headless=new"}, args...)
	} else {
		// Headful runs target the private Xvfb display; without this Chrome's
		// ozone auto-detection prefers Wayland and opens on the user's desktop.
		args = append(args, "--ozone-platform=x11")
	}
	// Chrome refuses to run its sandbox as root; elsewhere --no-sandbox only
	// weakens isolation and adds an "unsupported flag" banner detectors notice.
	if os.Geteuid() == 0 {
		args = append(args, "--no-sandbox")
	}
	if userAgent != "" {
		args = append(args, "--user-agent="+userAgent)
	}
	args = append(args, stealthArgs...)
	args = append(args, localNetworkAccessArgs...)
	return append(args, "about:blank")
}

// headfulUserAgent derives the UA a headful build of exe would send, so a
// headless fallback does not advertise "HeadlessChrome". It returns "" when
// the version cannot be determined.
func headfulUserAgent(exe string) string {
	out, err := exec.Command(exe, "--version").Output()
	if err != nil {
		return ""
	}
	var version string
	for _, f := range strings.Fields(string(out)) {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && strings.Contains(f, ".") {
			version = f
			break
		}
	}
	if version == "" {
		return ""
	}
	// Chrome reduces the UA to MAJOR.0.0.0 on every platform.
	major, _, _ := strings.Cut(version, ".")
	platform := "X11; Linux x86_64"
	if runtime.GOOS == "darwin" {
		platform = "Macintosh; Intel Mac OS X 10_15_7"
	}
	return "Mozilla/5.0 (" + platform + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 Safari/537.36"
}

// xvfbEnv returns environ pointed at the private Xvfb display. Any inherited
// desktop-session display variables are dropped so Chromium cannot attach to
// the user's real Wayland/X session and pop up a visible window.
func xvfbEnv(environ []string, display string) []string {
	env := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_SESSION_TYPE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "DISPLAY="+display, "XDG_SESSION_TYPE=x11")
}

// startXvfb launches a private virtual X server so Chrome can run headful
// (headless mode is the single biggest bot-detection signal) without opening
// windows on the user's desktop. It returns the server process and its
// DISPLAY value.
func startXvfb(ctx context.Context) (*exec.Cmd, string, error) {
	exe, err := exec.LookPath("Xvfb")
	if err != nil {
		return nil, "", err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	defer r.Close()
	cmd := exec.Command(exe, "-displayfd", "3", "-screen", "0", "1920x1080x24", "-nolisten", "tcp", "+extension", "RANDR")
	cmd.ExtraFiles = []*os.File{w}
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		return nil, "", fmt.Errorf("launch Xvfb: %w", err)
	}
	_ = w.Close()
	type result struct {
		display string
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		var b [16]byte
		n, err := r.Read(b[:])
		ch <- result{strings.TrimSpace(string(b[:n])), err}
	}()
	select {
	case res := <-ch:
		if res.display == "" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, "", fmt.Errorf("Xvfb did not report a display: %v", res.err)
		}
		go func() { _ = cmd.Wait() }()
		return cmd, ":" + res.display, nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", ctx.Err()
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", errors.New("Xvfb did not report a display within 10s")
	}
}

type LocalConfig struct {
	UserDataRoot  string
	Executable    string
	LaunchTimeout time.Duration
}

type localHandle struct {
	cmd             *exec.Cmd
	xvfb            *exec.Cmd
	cdpURL, profile string
}
type LocalDriver struct {
	cfg     LocalConfig
	mu      sync.Mutex
	handles map[string]*localHandle
	seeds   map[string]string // sessionID -> snapshot dir to copy in before first launch
}

func NewLocalDriver(cfg LocalConfig) *LocalDriver {
	if cfg.LaunchTimeout == 0 {
		cfg.LaunchTimeout = 20 * time.Second
	}
	return &LocalDriver{cfg: cfg, handles: make(map[string]*localHandle), seeds: make(map[string]string)}
}

// ProfileDir is the on-disk user-data-dir Chromium uses for an agent, whether or
// not it is currently provisioned. It is the source for snapshot capture.
func (d *LocalDriver) ProfileDir(id string) string {
	return filepath.Join(d.cfg.UserDataRoot, id)
}

// SeedProfile records a snapshot directory to copy into an agent's user-data-dir
// the first time its browser is provisioned. A later call before provisioning
// overrides an earlier one; passing "" clears the seed.
func (d *LocalDriver) SeedProfile(id, srcDir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if srcDir == "" {
		delete(d.seeds, id)
		return
	}
	d.seeds[id] = srcDir
}
func (*LocalDriver) Kind() string { return "local" }
func (d *LocalDriver) IsProvisioned(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handles[id] != nil
}
func (d *LocalDriver) PID(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h := d.handles[id]; h != nil && h.cmd.Process != nil {
		return h.cmd.Process.Pid
	}
	return 0
}

func (d *LocalDriver) Provision(ctx context.Context, id string) (ProvisionResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h := d.handles[id]; h != nil {
		return ProvisionResult{CDPURL: h.cdpURL}, nil
	}
	exe, err := DiscoverChromium(d.cfg.Executable)
	if err != nil {
		return ProvisionResult{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ProvisionResult{}, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	profile := filepath.Join(d.cfg.UserDataRoot, id)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return ProvisionResult{}, err
	}
	// Seed from a captured snapshot on first launch (empty profile dir only, so a
	// re-provision after a crash never clobbers accumulated state).
	if seed := d.seeds[id]; seed != "" {
		if entries, _ := os.ReadDir(profile); len(entries) == 0 {
			if err := copyTree(seed, profile); err != nil {
				return ProvisionResult{}, fmt.Errorf("seed browser snapshot: %w", err)
			}
		}
	}
	xvfb, display, xerr := startXvfb(ctx)
	headless := xerr != nil
	var ua string
	if headless {
		ua = headfulUserAgent(exe)
		slog.Warn("browser: Xvfb unavailable, falling back to headless chromium", "session_id", id, "error", xerr)
	}
	killXvfb := func() {
		if xvfb != nil {
			_ = xvfb.Process.Kill()
		}
	}
	args := localLaunchArgs(port, profile, headless, ua)
	cmd := exec.Command(exe, args...)
	if display != "" {
		cmd.Env = xvfbEnv(os.Environ(), display)
	}
	if err := cmd.Start(); err != nil {
		killXvfb()
		return ProvisionResult{}, fmt.Errorf("launch chromium: %w", err)
	}
	slog.Info("browser: launched local chromium", "session_id", id, "pid", cmd.Process.Pid, "executable", exe, "headless", headless, "display", display, "cdp_port", port)
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		// The X server exists only for this browser; reap it with Chrome.
		killXvfb()
		done <- err
	}()
	cdp := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.NewTimer(d.cfg.LaunchTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			_ = os.RemoveAll(profile)
			return ProvisionResult{}, fmt.Errorf("chromium exited during launch: %w", err)
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = os.RemoveAll(profile)
			return ProvisionResult{}, ctx.Err()
		case <-deadline.C:
			_ = cmd.Process.Kill()
			_ = os.RemoveAll(profile)
			return ProvisionResult{}, fmt.Errorf("chromium CDP did not start for agent %s", id)
		case <-tick.C:
			r, err := http.Get(cdp + "/json/version")
			if err == nil {
				_ = r.Body.Close()
				if r.StatusCode >= 200 && r.StatusCode < 300 {
					d.handles[id] = &localHandle{cmd: cmd, xvfb: xvfb, cdpURL: cdp, profile: profile}
					return ProvisionResult{CDPURL: cdp}, nil
				}
			}
		}
	}
}

func (d *LocalDriver) Teardown(_ context.Context, id string) error {
	d.mu.Lock()
	h := d.handles[id]
	delete(d.handles, id)
	d.mu.Unlock()
	if h != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	if h != nil && h.xvfb != nil {
		_ = h.xvfb.Process.Kill()
	}
	// Remove the known profile path even when there is no live handle. An
	// explicit fresh restart after a daemon/process crash must not silently
	// inherit the previous Chromium user-data directory.
	return os.RemoveAll(d.ProfileDir(id))
}

// SessionStore persists externalized (Steel) browser sessions so they can be
// re-attached after a daemon restart. It is satisfied structurally by
// *store.Store, so the browser package does not import store.
type SessionStore interface {
	SaveBrowserSession(sessionID, driverSessionID, profileID, cdpURL string) error
	DeleteBrowserSession(sessionID string) error
}

type SteelConfig struct {
	BaseURL, APIKey string
	SessionOptions  map[string]any
	Client          *http.Client
	// Store, when set, durably records each provisioned session so a restart can
	// re-attach to it rather than orphaning it on the Steel server.
	Store SessionStore
}
type steelSession struct{ id, cdpURL string }
type SteelDriver struct {
	cfg      SteelConfig
	mu       sync.Mutex
	sessions map[string]steelSession
	profiles map[string]string
}

func NewSteelDriver(cfg SteelConfig) (*SteelDriver, error) {
	if _, err := url.ParseRequestURI(cfg.BaseURL); err != nil || cfg.BaseURL == "" {
		return nil, errors.New("steel base URL is required")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 20 * time.Second}
	}
	return &SteelDriver{cfg: cfg, sessions: make(map[string]steelSession), profiles: make(map[string]string)}, nil
}
func (*SteelDriver) Kind() string   { return "steel" }
func (*SteelDriver) PID(string) int { return 0 }
func (d *SteelDriver) IsProvisioned(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.sessions[id]
	return ok
}
func (d *SteelDriver) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(d.cfg.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if d.cfg.APIKey != "" {
		req.Header.Set("steel-api-key", d.cfg.APIKey)
	}
	return d.cfg.Client.Do(req)
}
func (d *SteelDriver) Provision(ctx context.Context, id string) (ProvisionResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sessions[id]; ok {
		return ProvisionResult{CDPURL: s.cdpURL}, nil
	}
	opts := map[string]any{"dimensions": map[string]any{"width": 1280, "height": 800}, "persistProfile": true}
	for k, v := range d.cfg.SessionOptions {
		opts[k] = v
	}
	if p := d.profiles[id]; p != "" {
		opts["profileId"] = p
	}
	r, err := d.request(ctx, http.MethodPost, "/v1/sessions", opts)
	if err != nil {
		return ProvisionResult{}, err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return ProvisionResult{}, fmt.Errorf("steel create session: %s", r.Status)
	}
	var out struct{ ID, WebsocketURL, CDPURL, ProfileID string }
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
		return ProvisionResult{}, err
	}
	if out.ID == "" {
		return ProvisionResult{}, errors.New("steel create session: missing id")
	}
	raw := out.CDPURL
	if raw == "" {
		raw = out.WebsocketURL
	}
	if raw == "" {
		raw = strings.TrimRight(d.cfg.BaseURL, "/") + "/v1/sessions/" + url.PathEscape(out.ID)
	}
	raw = normalizeSteelURL(raw, d.cfg.BaseURL)
	d.sessions[id] = steelSession{id: out.ID, cdpURL: raw}
	if out.ProfileID != "" {
		d.profiles[id] = out.ProfileID
	}
	if d.cfg.Store != nil {
		_ = d.cfg.Store.SaveBrowserSession(id, out.ID, out.ProfileID, raw)
	}
	return ProvisionResult{CDPURL: raw}, nil
}

// Adopt seeds an agent's session/profile handles from persisted state so the
// next Provision re-attaches to the existing Steel session (preserving its
// pages) instead of creating a fresh one.
func (d *SteelDriver) Adopt(sessionID, driverSessionID, profileID, cdpURL string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions[sessionID] = steelSession{id: driverSessionID, cdpURL: cdpURL}
	if profileID != "" {
		d.profiles[sessionID] = profileID
	}
}

// ProfileID returns the Steel-side persisted profile id for an agent, if any.
// It is the reference recorded when capturing a snapshot on the Steel driver.
func (d *SteelDriver) ProfileID(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.profiles[id]
}

// SeedProfile makes the agent's next session start from an existing Steel
// profile id (the snapshot ref), rather than a fresh profile.
func (d *SteelDriver) SeedProfile(id, profileID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if profileID == "" {
		delete(d.profiles, id)
		return
	}
	d.profiles[id] = profileID
}

func normalizeSteelURL(raw, base string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	b, err := url.Parse(base)
	if err != nil {
		return raw
	}
	switch u.Hostname() {
	case "0.0.0.0", "::", "127.0.0.1", "localhost":
		host := b.Hostname()
		if p := u.Port(); p != "" {
			host = net.JoinHostPort(host, p)
		}
		u.Host = host
	}
	return u.String()
}
func (d *SteelDriver) Teardown(ctx context.Context, id string) error {
	d.mu.Lock()
	s, ok := d.sessions[id]
	delete(d.sessions, id)
	d.mu.Unlock()
	// The session is ending for good (agent closed, or re-attach found it gone),
	// so drop the persisted handle whether or not we still hold it in memory.
	if d.cfg.Store != nil {
		_ = d.cfg.Store.DeleteBrowserSession(id)
	}
	if !ok {
		return nil
	}
	r, err := d.request(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(s.id)+"/release", nil)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return fmt.Errorf("steel release session: %s", r.Status)
	}
	return nil
}
