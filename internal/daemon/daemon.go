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
	"strconv"
	"syscall"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/httpserver"
	"github.com/aiguy110/tandem/internal/registry"
	"github.com/aiguy110/tandem/internal/store"
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
	return Serve(ctx, cfg, stdout)
}

// Serve runs the authenticated HTTP and UI surface until ctx is canceled. The
// WebSocket protocol is intentionally not installed until the network phases.
func Serve(ctx context.Context, cfg config.Config, stdout io.Writer) error {
	token, err := config.EnsureToken(cfg.TokenPath)
	if err != nil {
		return fmt.Errorf("ensure bearer token: %w", err)
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
	if cfg.Browser.MCPEnabled {
		driver, driverErr := browser.NewDriver(browser.DriverConfig{Driver: cfg.Browser.Driver, UserDataRoot: cfg.Browser.UserDataRoot, ChromiumExecutable: cfg.Browser.ChromiumExecutable, SteelBaseURL: cfg.Browser.SteelBaseURL, SteelAPIKey: cfg.Browser.SteelAPIKey, SteelSessionOptions: cfg.Browser.SteelSessionOptions})
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
			OnResolved: func(id, _ string) { pushAgentEvent(agents, id, map[string]any{"kind": "status", "status": "working"}) },
		})
		exe, exeErr := os.Executable()
		if exeErr != nil {
			return exeErr
		}
		wiring := browser.MCPWiring{Broker: broker, NodeRuntime: cfg.Browser.NodeRuntime, PlaywrightCLI: cfg.Browser.PlaywrightMCPCLI, TandemExecutable: exe, ControlURL: origin, Token: token}
		factory = registry.DefaultFactory{Assets: assetStore, MCPServers: func(id string) []browser.MCPServer { return browser.BuildMCPServers(wiring, id) }}
	}
	agents, err = registry.New(registry.Options{Store: db, Config: cfg, Assets: assetStore, Factory: factory, Browser: broker})
	if err != nil {
		return err
	}
	if err := agents.RestoreAll(ctx); err != nil {
		return fmt.Errorf("restore agents: %w", err)
	}
	defer func() {
		disposeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = agents.DisposeAll(disposeCtx)
	}()
	httpHandler := httpserver.New(httpserver.Options{
		Token: token, BootstrapURL: bootstrapURL, UIDir: cfg.UIDir, Assets: assetStore,
		AgentExists: func(id string) bool {
			agent, lookupErr := db.Agent(id)
			return lookupErr == nil && agent != nil
		},
	})
	deferred := newDeferredShutdown(ctx, token, agents)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/shutdown-after-turns":
			deferred.ServeHTTP(w, r)
			return
		case "/internal/browser/takeover":
			if takeovers != nil {
				takeovers.ServeHTTP(w, r)
				return
			}
		}
		httpHandler.ServeHTTP(w, r)
	})
	handler := wsserver.New(wsserver.Options{Token: token, Registry: agents, Fallback: fallback})
	defer handler.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	fmt.Fprintf(stdout, "tandem daemon · http on %s:%d · home %s\n", cfg.Host, port, cfg.Home)
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
