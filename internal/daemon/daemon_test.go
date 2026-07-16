package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/config"
)

func TestServeLoadsEmbeddedUIAndStopsCleanly(t *testing.T) {
	home := t.TempDir()
	port := availablePort(t)
	cfg := config.Config{
		Home: home, DBPath: filepath.Join(home, "tandem.db"), TokenPath: filepath.Join(home, "token"),
		AssetsDir: filepath.Join(home, "assets"), Host: "127.0.0.1", Port: port,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, io.Discard) }()

	client := &http.Client{Timeout: time.Second}
	var response *http.Response
	var err error
	url := fmt.Sprintf("http://127.0.0.1:%d/agents/api-1", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get(url)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		<-done
		t.Fatalf("load embedded UI: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `<div id="root"></div>`) || response.Header.Get("Cache-Control") == "" {
		t.Fatalf("UI response status=%d headers=%v body=%q err=%v", response.StatusCode, response.Header, body, readErr)
	}
	if mode := fileMode(t, cfg.TokenPath); mode.Perm() != 0o600 {
		t.Fatalf("token mode=%o, want 600", mode.Perm())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func availablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func fileMode(t *testing.T, name string) os.FileMode {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}
