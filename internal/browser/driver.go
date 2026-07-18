// Package browser provides browser lifecycle drivers and the agent-facing CDP broker.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	names := []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"}
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

type LocalConfig struct {
	UserDataRoot  string
	Executable    string
	LaunchTimeout time.Duration
}

type localHandle struct {
	cmd             *exec.Cmd
	cdpURL, profile string
}
type LocalDriver struct {
	cfg     LocalConfig
	mu      sync.Mutex
	handles map[string]*localHandle
	seeds   map[string]string // agentID -> snapshot dir to copy in before first launch
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
	args := []string{"--headless=new", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
		fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check", "--window-size=1280,800", "about:blank"}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		return ProvisionResult{}, fmt.Errorf("launch chromium: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
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
					d.handles[id] = &localHandle{cmd: cmd, cdpURL: cdp, profile: profile}
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
	// Remove the known profile path even when there is no live handle. An
	// explicit fresh restart after a daemon/process crash must not silently
	// inherit the previous Chromium user-data directory.
	return os.RemoveAll(d.ProfileDir(id))
}

// SessionStore persists externalized (Steel) browser sessions so they can be
// re-attached after a daemon restart. It is satisfied structurally by
// *store.Store, so the browser package does not import store.
type SessionStore interface {
	SaveBrowserSession(agentID, sessionID, profileID, cdpURL string) error
	DeleteBrowserSession(agentID string) error
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
func (d *SteelDriver) Adopt(agentID, sessionID, profileID, cdpURL string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions[agentID] = steelSession{id: sessionID, cdpURL: cdpURL}
	if profileID != "" {
		d.profiles[agentID] = profileID
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
