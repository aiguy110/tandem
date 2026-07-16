package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/config"
)

type fakeIdleRegistry struct {
	active int
	gate   chan struct{}
	waits  atomic.Int32
}

func (f *fakeIdleRegistry) ActiveTurnCount() int { return f.active }
func (f *fakeIdleRegistry) WaitForIdle(ctx context.Context) bool {
	f.waits.Add(1)
	select {
	case <-f.gate:
		return true
	case <-ctx.Done():
		return false
	}
}

func TestDeferredShutdownAuthRepeatAndSingleWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agents := &fakeIdleRegistry{active: 2, gate: make(chan struct{})}
	h := newDeferredShutdown(ctx, "secret", agents)

	request := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, target, nil))
		return w
	}
	if w := request("/internal/shutdown-after-turns?token=wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", w.Code)
	}
	if w := request("/internal/shutdown-after-turns?token=secret"); w.Code != http.StatusOK || w.Body.String() != "2 0\n" || w.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("first response status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	if w := request("/internal/shutdown-after-turns?token=secret"); w.Code != http.StatusOK || w.Body.String() != "2 1\n" {
		t.Fatalf("repeat response status=%d body=%q", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for agents.waits.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if agents.waits.Load() != 1 {
		t.Fatalf("waiters=%d, want 1", agents.waits.Load())
	}
	close(agents.gate)
	select {
	case <-h.ready:
	case <-time.After(time.Second):
		t.Fatal("idle registry did not request shutdown")
	}
}

func TestServeShutdownAfterTurnsEndpointStopsDaemon(t *testing.T) {
	home := t.TempDir()
	port := availablePort(t)
	cfg := config.Config{
		Home: home, DBPath: filepath.Join(home, "tandem.db"), TokenPath: filepath.Join(home, "token"),
		AssetsDir: filepath.Join(home, "assets"), Host: "127.0.0.1", Port: port,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, io.Discard) }()

	var token []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		token, _ = os.ReadFile(cfg.TokenPath)
		if len(token) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(token) == 0 {
		t.Fatal("daemon did not create token")
	}

	client := &http.Client{Timeout: time.Second}
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port) + "/internal/shutdown-after-turns?token=" + string(token)
	var response *http.Response
	var err error
	for time.Now().Before(deadline) {
		response, err = client.Post(endpoint, "text/plain", nil)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("post deferred shutdown: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "0 0" {
		t.Fatalf("shutdown response status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve deferred shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after idle shutdown request")
	}
}
