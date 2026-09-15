// Package daemon owns the native Tandem server lifecycle.
package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/automation"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/buildinfo"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/historyimport"
	"github.com/aiguy110/tandem/internal/homebase"
	"github.com/aiguy110/tandem/internal/httpserver"
	"github.com/aiguy110/tandem/internal/languagemodel"
	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/registry"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/updater"
	"github.com/aiguy110/tandem/internal/voice"
	"github.com/aiguy110/tandem/internal/wsserver"
)

// RunOptions selects daemon runtime behavior without rewriting the user's
// configuration file. MasterURL makes this daemon an agent-host slave.
type RunOptions struct{ MasterURL string }

// Run loads runtime configuration and serves until SIGINT or SIGTERM.
func Run(stdout io.Writer) error { return RunWithOptions(stdout, RunOptions{}) }

// RunWithOptions is Run with ephemeral command-line options.
func RunWithOptions(stdout io.Writer, opts RunOptions) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if _, statErr := os.Stat(config.ConfigFilePath(cfg.Home)); os.IsNotExist(statErr) {
		fmt.Fprintf(stdout, "tandem: no configuration found at %s; using defaults. Run 'tandem setup' in a terminal to configure Tandem.\n", config.ConfigFilePath(cfg.Home))
	}
	return ServeWithOptions(ctx, cfg, stdout, opts)
}

// Serve runs the authenticated HTTP and UI surface until ctx is canceled. The
// WebSocket protocol is intentionally not installed until the network phases.
func Serve(ctx context.Context, cfg config.Config, stdout io.Writer) error {
	return ServeWithOptions(ctx, cfg, stdout, RunOptions{})
}

// ServeWithOptions is Serve with ephemeral command-line options.
func ServeWithOptions(ctx context.Context, cfg config.Config, stdout io.Writer, runOpts RunOptions) error {
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
			// Returning the wheel can race with an interrupt. Do not resurrect a
			// cancelled turn as "working" just because its browser takeover was
			// released after the prompt ended.
			if agents != nil {
				if s := agents.Get(id); s != nil {
					pushAgentEvent(agents, id, map[string]any{"kind": "status", "status": takeoverResolvedStatus(s.ActiveTurn(), s.Status())})
				}
			}
		},
	})
	exe, exeErr := os.Executable()
	if exeErr != nil {
		return exeErr
	}
	wiring := browser.MCPWiring{Broker: broker, NodeRuntime: cfg.Browser.NodeRuntime, PlaywrightCLI: cfg.Browser.PlaywrightMCPCLI, TandemExecutable: exe, ControlURL: origin, Token: token, BrowserEnabled: cfg.Browser.MCPEnabled}
	factory = registry.DefaultFactory{Assets: assetStore, Config: cfg, MCPServers: func(id, cwd string) ([]browser.MCPServer, error) {
		return configuredMCPServers(wiring, cfg.Home, id, cwd)
	}}
	audioCache := newMessageAudioCache(ctx, db, voiceRenderer)
	agents, err = registry.New(registry.Options{Store: db, Config: cfg, Assets: assetStore, Factory: factory, Browser: broker,
		OnSession: audioCache.watch,
		OnAudioPreference: func(s *session.Session, enabled bool) {
			if enabled {
				audioCache.prepare(s)
			}
		},
		OnAudioFocus: audioCache.setFocus,
	})
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
		servers, serversErr := configuredMCPServers(wiring, cfg.Home, runID, repoRoot)
		if serversErr != nil {
			return nil, serversErr
		}
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
	deferred := newDeferredShutdown(ctx, token, agents)
	notificationCenter := notifications.New()
	loopback, err := federation.NewLoopbackLocal(origin, token)
	if err != nil {
		return fmt.Errorf("configure federation loopback: %w", err)
	}
	federationName, hostnameErr := os.Hostname()
	if hostnameErr != nil || strings.TrimSpace(federationName) == "" {
		federationName = displayHost
	}
	federationService, err := federation.New(federation.Options{Store: db, Notifications: notificationCenter, MasterURL: runOpts.MasterURL, ProxyURL: cfg.MasterProxy, Name: federationName, Endpoint: origin, Local: loopback, BuildVersion: buildinfo.Version})
	if err != nil {
		return err
	}
	httpHandler := httpserver.New(httpserver.Options{
		// A federated agent's images live on the host that owns it; the
		// federated store fetches them over the tunnel on a local miss.
		Token: token, Version: buildinfo.Version, BootstrapURL: bootstrapURL, UIDir: cfg.UIDir, Assets: federatedAssetStore{local: assetStore, federation: federationService},
		Uploads: federatedUploadStore{local: agents, federation: federationService},
		Voice:   voiceRenderer,
		MessageText: func(sessionID string, seq int64) (string, error) {
			return transcriptMessageText(db, sessionID, seq)
		},
		// A federated agent's transcript lives on the host that owns it, so
		// its clip is rendered there and carried back over the tunnel.
		RenderMessageAudio: func(renderCtx context.Context, sessionID string, seq int64) (voice.Audio, error) {
			if hostID, localID, ok := wsserver.SplitRemoteSessionID(sessionID); ok {
				return remoteMessageAudio(renderCtx, federationService, hostID, localID, seq)
			}
			return audioCache.render(renderCtx, sessionID, seq)
		},
		AgentExists: func(id string) bool {
			if hostID, _, ok := wsserver.SplitRemoteSessionID(id); ok {
				// Only the owning host can confirm the agent; accept any ID
				// naming a host we know and let the remote call 404 instead.
				for _, host := range federationService.Hosts() {
					if host.ID == hostID {
						return true
					}
				}
				return false
			}
			agent, lookupErr := db.Session(id)
			return lookupErr == nil && agent != nil
		},
	})
	updateService := updater.NewService(updater.ServiceOptions{
		Updater: updater.Options{CurrentVersion: buildinfo.Version, Log: stdout},
		Home:    cfg.Home, Center: notificationCenter, Agents: agents, Log: stdout,
		Restart: func() { deferred.Request() },
	})
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/federation/") {
			federationService.ServeHTTP(w, r)
			return
		}
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
				id := r.URL.Query().Get("sessionId")
				if id == "" {
					id = r.URL.Query().Get("agentId")
				}
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
	notificationAction := func(actionCtx context.Context, id, action string) (string, error) {
		if sessionID, handled, actionErr := federationService.HandleNotificationAction(actionCtx, id, action); handled {
			return sessionID, actionErr
		}
		return updateService.HandleAction(actionCtx, id, action)
	}
	handler := wsserver.New(wsserver.Options{Token: token, Registry: agents, Fallback: fallback, Browser: broker, History: historyLifecycle, Automation: db, Notifications: notificationCenter, NotificationAction: notificationAction, Federation: federationService, AudioReadySeqs: audioCache.readySeqs, AudioReady: audioCache.readyClips, RenderMessageAudio: audioCache.render, Asset: assetStore.Get, PutAsset: assetStore.Put, SaveUpload: agents.Save, HasUploadDirectory: agents.HasConfiguredDirectory})
	defer handler.Close()
	updateService.Start(ctx)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	if runOpts.MasterURL != "" {
		go func() {
			if federationErr := federationService.RunSlave(ctx); federationErr != nil && ctx.Err() == nil {
				fmt.Fprintf(stdout, "tandem: federation slave stopped: %v\n", federationErr)
			}
		}()
	}

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

// configuredMCPServers combines Tandem's built-ins with the live global and
// project-local server configuration. This is intentionally evaluated per
// session creation, so changes made by `tandem mcp add` are available to new
// agents immediately without restarting the daemon.
func configuredMCPServers(wiring browser.MCPWiring, home, sessionID, cwd string) ([]browser.MCPServer, error) {
	servers := browser.BuildMCPServers(wiring, sessionID, cwd)
	used := make(map[string]bool, len(servers))
	for _, server := range servers {
		used[server.Name] = true
	}
	configured, err := config.LoadMCPServers(home, cwd)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if used[name] {
			return nil, fmt.Errorf("mcp server %q conflicts with a Tandem-provided server", name)
		}
		definition := configured[name]
		keys := make([]string, 0, len(definition.Env))
		for key := range definition.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		env := make([]browser.MCPEnvVariable, 0, len(keys))
		for _, key := range keys {
			env = append(env, browser.MCPEnvVariable{Name: key, Value: definition.Env[key]})
		}
		headerKeys := make([]string, 0, len(definition.Headers))
		for key := range definition.Headers {
			headerKeys = append(headerKeys, key)
		}
		sort.Strings(headerKeys)
		headers := make([]browser.MCPEnvVariable, 0, len(headerKeys))
		for _, key := range headerKeys {
			headers = append(headers, browser.MCPEnvVariable{Name: key, Value: definition.Headers[key]})
		}
		servers = append(servers, browser.MCPServer{Name: name, Type: definition.Type, Command: definition.Command, Args: definition.Args, Env: env, URL: definition.URL, Headers: headers})
		used[name] = true
	}
	return servers, nil
}

func transcriptMessageText(db *store.Store, sessionID string, seq int64) (string, error) {
	after := seq - 2
	if after < 0 {
		after = 0
	}
	rows, err := db.RangeEvents(sessionID, after)
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
		// Audio state changes are emitted asynchronously while an agent may
		// still be streaming its reply. They do not create a new transcript
		// row in the UI, so they must not split the text supplied to speech.
		if row.Kind == "audio_state" {
			continue
		}
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

// remoteMessageAudio renders one federated agent's clip on the host that owns
// its transcript. The tunnel carries browser-protocol JSON only, so the bytes
// come back base64-encoded and are decoded here for the ordinary audio route.
func remoteMessageAudio(ctx context.Context, svc *federation.Service, hostID, sessionID string, seq int64) (voice.Audio, error) {
	if svc == nil {
		return voice.Audio{}, errors.New("remote hosts are unavailable")
	}
	payload, err := json.Marshal(map[string]any{"t": "render_message_audio", "sessionId": sessionID, "seq": seq})
	if err != nil {
		return voice.Audio{}, err
	}
	response, err := svc.Call(ctx, hostID, payload)
	if err != nil {
		return voice.Audio{}, err
	}
	var envelope struct {
		Error    string `json:"error"`
		MIMEType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return voice.Audio{}, fmt.Errorf("remote host returned invalid audio response: %w", err)
	}
	if envelope.Error != "" {
		return voice.Audio{}, errors.New(envelope.Error)
	}
	data, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil {
		return voice.Audio{}, fmt.Errorf("remote host returned undecodable audio: %w", err)
	}
	if len(data) == 0 {
		return voice.Audio{}, errors.New("remote host returned no audio")
	}
	return voice.Audio{Data: data, MIMEType: envelope.MIMEType}, nil
}

// federatedAssetStore serves local assets directly and routes assets for a
// namespaced session to the daemon that owns it.
type federatedAssetStore struct {
	local      *assets.Store
	federation *federation.Service
}

type federatedUploadStore struct {
	local      *registry.Registry
	federation *federation.Service
}

func (s federatedUploadStore) Save(sessionID, name string, data []byte) (string, error) {
	hostID, localID, ok := wsserver.SplitRemoteSessionID(sessionID)
	if !ok {
		return s.local.Save(sessionID, name, data)
	}
	var envelope struct {
		Error string `json:"error"`
		Path  string `json:"path"`
	}
	err := remoteUploadCall(s.federation, hostID, map[string]any{
		"t": "save_upload", "sessionId": localID, "name": name,
		"bytesB64": base64.StdEncoding.EncodeToString(data),
	}, &envelope)
	if err != nil {
		slog.Warn("remote workspace upload failed", "host_id", hostID, "session_id", localID, "name", name, "bytes", len(data), "error", err)
		return "", err
	}
	return envelope.Path, nil
}

func (s federatedUploadStore) HasConfiguredDirectory(sessionID string) (bool, error) {
	hostID, localID, ok := wsserver.SplitRemoteSessionID(sessionID)
	if !ok {
		return s.local.HasConfiguredDirectory(sessionID)
	}
	var envelope struct {
		Error      string `json:"error"`
		Configured bool   `json:"configured"`
	}
	err := remoteUploadCall(s.federation, hostID, map[string]any{
		"t": "has_upload_directory", "sessionId": localID,
	}, &envelope)
	if err != nil {
		slog.Warn("remote upload settings lookup failed", "host_id", hostID, "session_id", localID, "error", err)
		return false, err
	}
	return envelope.Configured, nil
}

func remoteUploadCall(svc *federation.Service, hostID string, payload any, envelope any) error {
	if svc == nil {
		return errors.New("remote hosts are unavailable")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	response, err := svc.Call(ctx, hostID, raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(response, envelope); err != nil {
		return fmt.Errorf("remote host returned an invalid upload response: %w", err)
	}
	var status struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response, &status); err != nil {
		return err
	}
	if status.Error != "" {
		return errors.New(status.Error)
	}
	return nil
}

func remotePutAsset(_ context.Context, svc *federation.Service, hostID, sessionID string, data []byte, declaredMIME string) (assets.Stored, error) {
	var envelope struct {
		Error    string `json:"error"`
		AssetID  string `json:"assetId"`
		MIMEType string `json:"mimeType"`
		Size     int64  `json:"size"`
	}
	err := remoteUploadCall(svc, hostID, map[string]any{
		"t": "put_asset", "sessionId": sessionID, "mimeType": declaredMIME,
		"bytesB64": base64.StdEncoding.EncodeToString(data),
	}, &envelope)
	if err != nil {
		slog.Warn("remote asset upload failed", "host_id", hostID, "session_id", sessionID, "mime_type", declaredMIME, "bytes", len(data), "error", err)
		return assets.Stored{}, err
	}
	return assets.Stored{AssetID: envelope.AssetID, MIMEType: envelope.MIMEType, Size: envelope.Size}, nil
}

func (s federatedAssetStore) Put(sessionID string, data []byte, declaredMIME string) (assets.Stored, error) {
	hostID, localID, ok := wsserver.SplitRemoteSessionID(sessionID)
	if ok {
		return remotePutAsset(context.Background(), s.federation, hostID, localID, data, declaredMIME)
	}
	return s.local.Put(sessionID, data, declaredMIME)
}

func (s federatedAssetStore) Get(sessionID, assetID string) (assets.Stored, error) {
	stored, err := s.local.Get(sessionID, assetID)
	if err == nil {
		return stored, nil
	}
	hostID, localID, ok := wsserver.SplitRemoteSessionID(sessionID)
	if !ok {
		return assets.Stored{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stored, err = remoteAsset(ctx, s.federation, hostID, localID, assetID)
	if err != nil {
		slog.Warn("remote asset fetch failed", "host_id", hostID, "session_id", localID, "asset_id", assetID, "error", err)
		return assets.Stored{}, err
	}
	slog.Debug("remote asset fetched", "host_id", hostID, "session_id", localID, "asset_id", assetID, "bytes", stored.Size)
	return stored, nil
}

// remoteAsset fetches one stored image from the host that owns a federated
// session. The tunnel carries browser-protocol JSON only, so the bytes come
// back base64-encoded, as with remoteMessageAudio.
func remoteAsset(ctx context.Context, svc *federation.Service, hostID, sessionID, assetID string) (assets.Stored, error) {
	if svc == nil {
		return assets.Stored{}, errors.New("remote hosts are unavailable")
	}
	payload, err := json.Marshal(map[string]any{"t": "get_asset", "sessionId": sessionID, "assetId": assetID})
	if err != nil {
		return assets.Stored{}, err
	}
	response, err := svc.Call(ctx, hostID, payload)
	if err != nil {
		return assets.Stored{}, err
	}
	var envelope struct {
		Error    string `json:"error"`
		MIMEType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return assets.Stored{}, fmt.Errorf("remote host returned invalid asset response: %w", err)
	}
	if envelope.Error != "" {
		return assets.Stored{}, errors.New(envelope.Error)
	}
	data, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil {
		return assets.Stored{}, fmt.Errorf("remote host returned undecodable asset: %w", err)
	}
	if len(data) == 0 || envelope.MIMEType == "" {
		return assets.Stored{}, errors.New("remote host returned an empty asset")
	}
	return assets.Stored{AssetID: assetID, MIMEType: envelope.MIMEType, Size: int64(len(data)), Data: data}, nil
}

// messageAudioCache owns all provider work and retained clips. The browser
// never receives provider credentials or performs provider requests; it only
// asks Tandem for a message's already-cached (or manually requested) audio.
type messageAudioCache struct {
	ctx      context.Context
	db       *store.Store
	renderer voice.Renderer
	mu       sync.Mutex
	flights  map[string]chan struct{}
	focuses  map[string]map[string]struct{}
	prepared map[string]struct{}
}

func newMessageAudioCache(ctx context.Context, db *store.Store, renderer voice.Renderer) *messageAudioCache {
	return &messageAudioCache{
		ctx: ctx, db: db, renderer: renderer,
		flights: make(map[string]chan struct{}), focuses: make(map[string]map[string]struct{}), prepared: make(map[string]struct{}),
	}
}

func audioKey(sessionID string, seq int64) string {
	return sessionID + "/" + strconv.FormatInt(seq, 10)
}

func (c *messageAudioCache) render(ctx context.Context, sessionID string, seq int64) (voice.Audio, error) {
	if c.renderer == nil {
		return voice.Audio{}, errors.New("voice rendering is not configured; run tandem setup")
	}
	if cached, err := c.db.MessageAudio(sessionID, seq); err != nil {
		return voice.Audio{}, err
	} else if cached != nil {
		c.backfillDuration(sessionID, seq, cached)
		return voice.Audio{Data: cached.Data, MIMEType: cached.MIMEType}, nil
	}
	key := audioKey(sessionID, seq)
	c.mu.Lock()
	if done := c.flights[key]; done != nil {
		c.mu.Unlock()
		select {
		case <-done:
			return c.render(ctx, sessionID, seq)
		case <-ctx.Done():
			return voice.Audio{}, ctx.Err()
		}
	}
	done := make(chan struct{})
	c.flights[key] = done
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.flights, key); close(done); c.mu.Unlock() }()
	text, err := transcriptMessageText(c.db, sessionID, seq)
	if err != nil {
		return voice.Audio{}, err
	}
	audio, err := c.renderer.Render(ctx, text)
	if err != nil {
		return voice.Audio{}, err
	}
	durationMs, _ := voice.Duration(audio.MIMEType, audio.Data) // best effort; 0 means unknown
	if err := c.db.PutMessageAudio(store.MessageAudio{SessionID: sessionID, Seq: seq, MIMEType: audio.MIMEType, Data: audio.Data, DurationMs: durationMs}); err != nil {
		return voice.Audio{}, err
	}
	return audio, nil
}

// backfillDuration computes and persists duration for a cached clip that
// predates duration computation (DurationMs == 0). It is a best-effort,
// read-triggered upgrade: any failure to parse or persist just leaves the
// clip's duration unknown for this read, to be retried on a later read.
func (c *messageAudioCache) backfillDuration(sessionID string, seq int64, cached *store.MessageAudio) int64 {
	if cached.DurationMs > 0 {
		return cached.DurationMs
	}
	durationMs, ok := voice.Duration(cached.MIMEType, cached.Data)
	if !ok || durationMs <= 0 {
		return 0
	}
	if err := c.db.UpdateMessageAudioDuration(sessionID, seq, durationMs); err != nil {
		return 0
	}
	cached.DurationMs = durationMs
	return durationMs
}

func (c *messageAudioCache) watch(s *session.Session) {
	s.OnEvent(func(le eventlog.LoggedEvent) {
		// An assistant message is a contiguous run of message_chunk events. It
		// becomes safe to render as soon as the agent moves on to another output
		// block; waiting for status=idle makes speech lag behind tool work (and
		// sometimes an entire long-running turn). The trailing run still waits
		// for idle, because it may continue streaming.
		if messageBlockBoundary(le.Event.Kind) || (le.Event.Kind == "status" && s.Status() == session.Idle) {
			c.prepare(s)
		}
	})
	c.prepare(s)
}

func messageBlockBoundary(kind string) bool {
	switch kind {
	case "thought_chunk", "tool_call", "tool_call_update", "plan", "terminal_output", "permission_request", "error", "user_message":
		return true
	default:
		return false
	}
}

func (c *messageAudioCache) setFocus(s *session.Session, clientID string, focused bool) {
	c.mu.Lock()
	if focused {
		clients := c.focuses[s.ID]
		if clients == nil {
			clients = make(map[string]struct{})
			c.focuses[s.ID] = clients
		}
		clients[clientID] = struct{}{}
	} else if clients := c.focuses[s.ID]; clients != nil {
		delete(clients, clientID)
		if len(clients) == 0 {
			delete(c.focuses, s.ID)
		}
	}
	c.mu.Unlock()
	if focused {
		c.prepare(s)
	}
}

func (c *messageAudioCache) focused(sessionID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.focuses[sessionID]) > 0
}

func (c *messageAudioCache) readySeqs(sessionID string) []int64 {
	seqs, err := c.db.MessageAudioSeqs(sessionID)
	if err != nil {
		return nil
	}
	return seqs
}

// readyClips is readySeqs plus each clip's known duration, for the WS
// snapshot's audioReady field. It does not itself trigger backfill (that
// happens lazily off the render/HTTP audio-serving path, which already has
// the bytes in hand); rows not yet read through render carry durationMs=0.
func (c *messageAudioCache) readyClips(sessionID string) []store.MessageAudioClip {
	clips, err := c.db.MessageAudioClips(sessionID)
	if err != nil {
		return nil
	}
	return clips
}

// claimPreparation makes automatic rendering and its ready notification a
// once-per-process operation for each message. Turn completion, focus changes,
// reconnects, and multiple browser clients can all request preparation at the
// same time; render's flight map deduplicates provider work, but without this
// claim every caller would still emit a separate ready event and autoplay the
// same clip again.
func (c *messageAudioCache) claimPreparation(sessionID string, seq int64) bool {
	key := audioKey(sessionID, seq)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.prepared[key]; exists {
		return false
	}
	c.prepared[key] = struct{}{}
	return true
}

func (c *messageAudioCache) releasePreparation(sessionID string, seq int64) {
	c.mu.Lock()
	delete(c.prepared, audioKey(sessionID, seq))
	c.mu.Unlock()
}

func (c *messageAudioCache) prepare(s *session.Session) {
	if c.renderer == nil {
		return
	}
	enabled, enabledAfterSeq, err := c.db.SessionAudioEnabled(s.ID)
	if err != nil || !enabled {
		return
	}
	seqs, err := turnMessageSeqs(s, c.focused(s.ID), enabledAfterSeq)
	if err != nil || len(seqs) == 0 {
		return
	}
	pending := make([]int64, 0, len(seqs))
	for _, seq := range seqs {
		if c.claimPreparation(s.ID, seq) {
			pending = append(pending, seq)
		}
	}
	if len(pending) == 0 {
		return
	}
	go func(seqs []int64) {
		for _, seq := range seqs {
			if cached, err := c.db.MessageAudio(s.ID, seq); err == nil && cached != nil {
				emitAudioState(s, "ready", seq, "", c.backfillDuration(s.ID, seq, cached))
				continue
			}
			emitAudioState(s, "rendering", seq, "", 0)
			if _, err := c.render(c.ctx, s.ID, seq); err != nil {
				c.releasePreparation(s.ID, seq)
				emitAudioState(s, "error", seq, err.Error(), 0)
				continue
			}
			durationMs := int64(0)
			if cached, err := c.db.MessageAudio(s.ID, seq); err == nil && cached != nil {
				durationMs = cached.DurationMs
			}
			emitAudioState(s, "ready", seq, "", durationMs)
		}
	}(pending)
}

func turnMessageSeqs(s *session.Session, all bool, enabledAfterSeq int64) ([]int64, error) {
	history, err := s.Log.FullHistory()
	if err != nil {
		return nil, err
	}
	return completedMessageSeqs(history, all, enabledAfterSeq, s.Status() == session.Idle), nil
}

// completedMessageSeqs returns the first sequence number of each finished
// contiguous message_chunk block. A trailing block is only finished when the
// session is idle; otherwise it is still receiving streamed text.
func completedMessageSeqs(history []eventlog.LoggedEvent, all bool, enabledAfterSeq int64, includeTrailing bool) []int64 {
	start := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Event.Kind == "user_message" {
			start = i + 1
			break
		}
	}
	var seqs []int64
	for i := start; i < len(history); i++ {
		if history[i].Seq <= enabledAfterSeq || history[i].Event.Kind != "message_chunk" || (i > start && messageBlockContinuation(history[i-1].Event.Kind)) {
			continue
		}
		// A following event closes this block. If there isn't one, the caller
		// must explicitly know the agent is idle before we speak it.
		end := i + 1
		for end < len(history) && messageBlockContinuation(history[end].Event.Kind) {
			end++
		}
		if end == len(history) && !includeTrailing {
			continue
		}
		seqs = append(seqs, history[i].Seq)
	}
	if !all && len(seqs) > 1 {
		return seqs[len(seqs)-1:]
	}
	return seqs
}

// messageBlockContinuation identifies events that are transparent inside a
// streamed assistant message. In particular, audio_state can arrive from an
// earlier clip while the agent is still writing the current reply.
func messageBlockContinuation(kind string) bool {
	return kind == "message_chunk" || kind == "audio_state"
}

func emitAudioState(s *session.Session, state string, seq int64, message string, durationMs int64) {
	fields := map[string]any{"kind": "audio_state", "state": state, "seq": seq, "message": message}
	if durationMs > 0 {
		fields["durationMs"] = durationMs
	}
	payload, _ := json.Marshal(fields)
	s.PushEvent(eventlog.Event{Kind: "audio_state", Payload: payload})
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
		if agents.Get(ps.SessionID) == nil {
			_ = db.DeleteBrowserSession(ps.SessionID)
			continue
		}
		adopter.Adopt(ps.SessionID, ps.DriverSessionID, ps.ProfileID, ps.CDPURL)
		attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := broker.EnsureProvisioned(attachCtx, ps.SessionID)
		cancel()
		if err != nil {
			// EnsureProvisioned already tore the dead session down (which forgets
			// the persisted row); just note it.
			fmt.Fprintf(stdout, "browser re-attach failed for %s: %v\n", ps.SessionID, err)
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

// takeoverResolvedStatus restores the turn state after the user returns a
// browser takeover. A takeover itself marks a session blocked, but it does not
// keep a cancelled prompt alive. Preserve an error, otherwise only report
// working while the session still has an active prompt.
func takeoverResolvedStatus(active bool, current session.Status) session.Status {
	if active {
		return session.Working
	}
	if current == session.Error {
		return session.Error
	}
	return session.Idle
}
