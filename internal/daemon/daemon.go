// Package daemon owns the native Tandem server lifecycle.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/httpserver"
	"github.com/aiguy110/tandem/internal/store"
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
	bootstrapURL := "http://" + net.JoinHostPort(displayHost, strconv.Itoa(port)) + "/#t=" + token
	handler := httpserver.New(httpserver.Options{
		Token: token, BootstrapURL: bootstrapURL, UIDir: cfg.UIDir, Assets: assetStore,
		AgentExists: func(id string) bool {
			agent, lookupErr := db.Agent(id)
			return lookupErr == nil && agent != nil
		},
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	fmt.Fprintf(stdout, "tandem daemon · http on %s:%d · home %s\n", cfg.Host, port, cfg.Home)
	fmt.Fprintln(stdout, "websocket: unavailable during native migration phase 14")
	fmt.Fprintf(stdout, "bootstrap: %s\n", bootstrapURL)
	fmt.Fprintf(stdout, "TANDEM_READY port=%d token=%s\n", port, token)

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-serveErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
