// Package daemon owns the native Tandem server lifecycle.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/automation"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/historyimport"
	"github.com/aiguy110/tandem/internal/homebase"
	"github.com/aiguy110/tandem/internal/httpserver"
	"github.com/aiguy110/tandem/internal/languagemodel"
	"github.com/aiguy110/tandem/internal/registry"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/voice"
	"github.com/aiguy110/tandem/internal/wsserver"
)

// Run loads runtime configuration and serves until SIGINT or SIGTERM.
func Run(stdout io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if _, statErr := os.Stat(config.ConfigFilePath(cfg.Home)); os.IsNotExist(statErr) {
		fmt.Fprintf(stdout, "tandem: no configuration found at %s; using defaults. Run 'tandem setup' in a terminal to configure Tandem.\n", config.ConfigFilePath(cfg.Home))
	}
	return Serve(ctx, cfg, stdout)
}

// Serve runs the authenticated HTTP and UI surface until ctx is canceled. The
// WebSocket protocol is intentionally not installed until the network phases.
func Serve(ctx context.Context, cfg config.Config, stdout io.Writer) error {
	token, err := config.EnsureToken(cfg.TokenPath)
	if err != nil {
		return fmt.Errorf("ensure bearer token: %w", err)
	}
	// Scaffold/refresh the per-installation home base. Non-fatal: the daemon
	// must serve even if this hits a snag.
	if err := homebase.Ensure(ctx, cfg, stdout); err != nil {
		fmt.Fprintf(stdout, "tandem: home base setup warning: %v\n", err)
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	assetStore, err := assets.Open(cfg.AssetsDir, db)
	if err != nil {
		return err
	}
	var voiceRenderer voice.Renderer
	if cfg.Voice.Enabled {
		languageModel, languageErr := languagemodel.New(cfg.LanguageModel)
		if languageErr != nil {
			return fmt.Errorf("configure language model for voice rendering: %w", languageErr)
		}
		voiceRenderer, err = voice.New(cfg.Voice, languageModel)
		if err != nil {
			return fmt.Errorf("configure voice rendering: %w", err)
		}
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	displayHost := cfg.Host
	if displayHost == "0.0.0.0" || displayHost == "::" {
		displayHost = "127.0.0.1"
	}
	origin := "http://" + net.JoinHostPort(displayHost, strconv.Itoa(port))
	bootstrapURL := origin + "/#t=" + token

	var agents *registry.Registry
	var broker *browser.Broker
	var takeovers *browser.Takeovers
	var factory agentadapter.Factory
	driver, driverErr := browser.NewDriver(browser.DriverConfig{Driver: cfg.Browser.Driver, UserDataRoot: cfg.Browser.UserDataRoot, ChromiumExecutable: cfg.Browser.ChromiumExecutable, SteelBaseURL: cfg.Browser.SteelBaseURL, SteelAPIKey: cfg.Browser.SteelAPIKey, SteelSessionOptions: cfg.Browser.SteelSessionOptions, SessionStore: db})
	if driverErr != nil {
		return driverErr
	}
	broker = browser.NewBroker(driver, browser.BrokerConfig{OnRelease: func(id string) {
		if takeovers != nil {
			takeovers.Release(id)
		}
	}})
	if err = broker.Start(); err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = broker.Stop(stopCtx)
	}()
	takeovers = browser.NewTakeovers(browser.TakeoverOptions{
		Token:       token,
		AgentExists: func(id string) bool { return agents != nil && agents.Get(id) != nil },
		OnRequest: func(id, reqID, reason string) {
			pushAgentEvent(agents, id, map[string]any{"kind": "takeover_request", "reqId": reqID, "reason": reason})
			pushAgentEvent(agents, id, map[string]any{"kind": "status", "status": "blocked"})
		},
		OnResolved: func(id, reqID string) {
			// Persist the resolution as well as broadcasting browser_state. A
			// transcript snapshot must be able to distinguish an old, completed
			// takeover from one that is still waiting for the human.
			pushAgentEvent(agents, id, map[string]any{"kind": "takeover_resolved", "reqId": reqID})
			pushAgentEvent(agents, id, map[string]any{"kind": "status", "status": "working"})
		},
	})
	exe, exeErr := os.Executable()
	if exeErr != nil {
		return exeErr
	}
	wiring := browser.MCPWiring{Broker: broker, NodeRuntime: cfg.Browser.NodeRuntime, PlaywrightCLI: cfg.Browser.PlaywrightMCPCLI, TandemExecutable: exe, ControlURL: origin, Token: token, BrowserEnabled: cfg.Browser.MCPEnabled}
	factory = registry.DefaultFactory{Assets: assetStore, Config: cfg, MCPServers: func(id, cwd string) []browser.MCPServer { return browser.BuildMCPServers(wiring, id, cwd) }}
	agents, err = registry.New(registry.Options{Store: db, Config: cfg, Assets: assetStore, Factory: factory, Browser: broker})
	if err != nil {
		return err
	}
	if err := agents.RestoreAll(ctx); err != nil {
		return fmt.Errorf("restore agents: %w", err)
	}
	automationService := &automation.Service{
		Store: db, Agents: agents, Token: token,
		Runner: automation.Runner{NodeCommand: cfg.Node.Command},
	}
	automationService.CaptureBrowser = func(captureCtx context.Context, runID string) (string, string, func(), error) {
		if !broker.IsProvisioned(runID) {
			return "", "", func() {}, nil
		}
		dest := filepath.Join(cfg.Browser.SnapshotRoot, "automation-transfer", runID)
		_ = os.RemoveAll(dest)
		kind, ref, captureErr := broker.CaptureSnapshot(captureCtx, runID, dest)
		if captureErr != nil {
			return "", "", func() {}, captureErr
		}
		cleanup := func() {}
		if kind == "local" {
			cleanup = func() { _ = os.RemoveAll(dest) }
		}
		return kind, ref, cleanup, nil
	}
	automationService.Tools = func(runCtx context.Context, runID, repoRoot string, requested []string, snapshotRef string) (automation.ToolSession, error) {
		if snapshotRef != "" {
			snapshots, listErr := db.ListBrowserSnapshots()
			if listErr != nil {
				return nil, listErr
			}
			var selected *store.BrowserSnapshot
			for i := range snapshots {
				if snapshots[i].ID == snapshotRef || snapshots[i].Name == snapshotRef {
					selected = &snapshots[i]
					break
				}
			}
			if selected == nil {
				return nil, fmt.Errorf("no such browser snapshot: %s", snapshotRef)
			}
			if selected.Kind != "" && selected.Kind != broker.DriverKind() {
				return nil, fmt.Errorf("browser snapshot %q is for the %s driver, not %s", selected.Name, selected.Kind, broker.DriverKind())
			}
			broker.SeedSnapshot(runID, selected.Kind, selected.Ref)
		}
		servers := browser.BuildMCPServers(wiring, runID, repoRoot)
		return automation.StartMCPToolSession(runCtx, servers, repoRoot, requested, os.Stderr, func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = broker.Teardown(cleanupCtx, runID)
		})
	}
	automationScheduler := &automation.Scheduler{Service: automationService}
	automationScheduler.Start(ctx)
	var historyLifecycle *historyimport.Lifecycle
	for _, definition := range cfg.Agents {
		if definition.History == nil || !definition.History.Enabled {
			continue
		}
		historyRunner, runnerErr := historyimport.New(historyimport.Options{
			Store: db, Node: cfg.Node.Command, RuntimeRoot: cfg.RuntimeRoot,
		})
		if runnerErr != nil {
			return fmt.Errorf("configure history importer: %w", runnerErr)
		}
		historyLifecycle, err = historyimport.NewLifecycle(historyimport.LifecycleOptions{
			Store: db, Importer: historyRunner, Agents: cfg.Agents,
			EnsureRuntime: func(scanCtx context.Context) error {
				return runtimeinstall.EnsureHistory(scanCtx, cfg, stdout)
			},
		})
		if err != nil {
			return err
		}
		historyLifecycle.Start(ctx)
		defer historyLifecycle.Close()
		break
	}
	reattachBrowsers(ctx, db, driver, broker, agents, stdout)
	defer func() {
		disposeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = agents.DisposeAll(disposeCtx)
	}()
	httpHandler := httpserver.New(httpserver.Options{
		Token: token, BootstrapURL: bootstrapURL, UIDir: cfg.UIDir, Assets: assetStore,
		Voice: voiceRenderer,
		MessageText: func(agentID string, seq int64) (string, error) {
			return transcriptMessageText(db, agentID, seq)
		},
		AgentExists: func(id string) bool {
			agent, lookupErr := db.Agent(id)
			return lookupErr == nil && agent != nil
		},
	})
	deferred := newDeferredShutdown(ctx, token, agents)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/automation/run", "/internal/automation/evaluate", "/internal/automation/preapprove":
			automationService.ServeHTTP(w, r)
			return
		case "/internal/shutdown-after-turns":
			deferred.ServeHTTP(w, r)
			return
		case "/internal/browser/takeover":
			if takeovers != nil {
				takeovers.ServeHTTP(w, r)
				return
			}
		case "/internal/browser/devnav":
			if broker != nil && os.Getenv("TANDEM_DEV_BROWSER") == "1" {
				if r.Method != http.MethodPost {
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
				if r.URL.Query().Get("token") != token && r.Header.Get("Authorization") != "Bearer "+token {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				id := r.URL.Query().Get("agentId")
				if agents.Get(id) == nil {
					http.Error(w, "no such agent", http.StatusNotFound)
					return
				}
				var body struct {
					URL string `json:"url"`
				}
				if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body) != nil {
					http.Error(w, "invalid body", http.StatusBadRequest)
					return
				}
				if body.URL == "" {
					body.URL = "about:blank"
				}
				if err := broker.DevNavigate(r.Context(), id, body.URL); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
				return
			}
		}
		httpHandler.ServeHTTP(w, r)
	})
	handler := wsserver.New(wsserver.Options{Token: token, Registry: agents, Fallback: fallback, Browser: broker, History: historyLifecycle, Automation: db})
	defer handler.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	fmt.Fprintf(stdout, "tandem · http on %s:%d · home %s\n", cfg.Host, port, cfg.Home)
	fmt.Fprintln(stdout, "websocket: authenticated subscriptions and replay enabled")
	fmt.Fprintf(stdout, "bootstrap: %s\n", bootstrapURL)
	fmt.Fprintf(stdout, "TANDEM_READY port=%d token=%s\n", port, token)

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	case <-deferred.ready:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	err = <-serveErr
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func transcriptMessageText(db *store.Store, agentID string, seq int64) (string, error) {
	after := seq - 2
	if after < 0 {
		after = 0
	}
	rows, err := db.RangeEvents(agentID, after)
	if err != nil {
		return "", err
	}
	start := 0
	if len(rows) > 0 && rows[0].Seq == seq-1 {
		if rows[0].Kind == "message_chunk" {
			return "", httpserver.ErrMessageNotFound
		}
		start = 1
	}
	if start >= len(rows) || rows[start].Seq != seq || rows[start].Kind != "message_chunk" {
		return "", httpserver.ErrMessageNotFound
	}
	var text strings.Builder
	for _, row := range rows[start:] {
		if row.Kind != "message_chunk" {
			break
		}
		var event struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(row.Payload), &event); err != nil {
			return "", err
		}
		text.WriteString(event.Text)
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", httpserver.ErrMessageNotFound
	}
	return text.String(), nil
}

// reattachBrowsers re-connects to externalized (Steel) browser sessions that
// outlived a daemon restart, so their agents' Browser tabs light up with the
// live page instead of showing "no browser yet". A session the Steel server has
// since reclaimed fails to re-provision and is forgotten (its row removed); a
// row whose agent no longer exists is dropped too.
func reattachBrowsers(ctx context.Context, db *store.Store, driver browser.Driver, broker *browser.Broker, agents *registry.Registry, stdout io.Writer) {
	adopter, ok := driver.(*browser.SteelDriver)
	if !ok || broker == nil {
		return
	}
	sessions, err := db.ListBrowserSessions()
	if err != nil {
		fmt.Fprintf(stdout, "browser re-attach: list sessions: %v\n", err)
		return
	}
	for _, ps := range sessions {
		if agents.Get(ps.AgentID) == nil {
			_ = db.DeleteBrowserSession(ps.AgentID)
			continue
		}
		adopter.Adopt(ps.AgentID, ps.SessionID, ps.ProfileID, ps.CDPURL)
		attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := broker.EnsureProvisioned(attachCtx, ps.AgentID)
		cancel()
		if err != nil {
			// EnsureProvisioned already tore the dead session down (which forgets
			// the persisted row); just note it.
			fmt.Fprintf(stdout, "browser re-attach failed for %s: %v\n", ps.AgentID, err)
		}
	}
}

func pushAgentEvent(agents *registry.Registry, id string, value any) {
	if agents == nil {
		return
	}
	s := agents.Get(id)
	if s == nil {
		return
	}
	payload, err := json.Marshal(value)
	kind := ""
	if object, ok := value.(map[string]any); ok {
		kind, _ = object["kind"].(string)
	}
	if err == nil {
		s.PushEvent(eventlog.Event{Kind: kind, Payload: payload})
	}
}
