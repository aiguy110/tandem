package wsserver

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPeerNetworkClassification(t *testing.T) {
	cases := []struct {
		name      string
		remote    string
		forwarded string
		want      string
	}{
		{"tailnet peer", "100.101.102.103:51234", "", "tailscale"},
		{"tailnet v6 peer", "[fd7a:115c:a1e0::1234]:443", "", "tailscale"},
		{"direct loopback", "127.0.0.1:5000", "", "loopback"},
		{"lan", "192.168.1.20:4000", "", "lan"},
		{"public", "8.8.8.8:4000", "", "other"},
		{"tailscale serve proxy", "127.0.0.1:5000", "100.80.1.2", "proxied:tailscale"},
		{"proxy chain uses first hop", "127.0.0.1:5000", "100.80.1.2, 10.0.0.1", "proxied:tailscale"},
		{"unparseable", "garbage", "", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := newPeerInfo(r).network; got != tc.want {
				t.Fatalf("network = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientDiagIsLoggedAndConnectionSurvives(t *testing.T) {
	var buf syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, _, _, _, url := setupWSOptions(t, 0, nil, nil)
	c := dial(t, url)
	send(t, c, map[string]any{"t": "client_diag", "diag": map[string]any{
		"kind": "slow_command", "command": "close_agent", "corrId": "c7", "latencyMs": 61000, "rttMs": 4200,
	}})
	send(t, c, map[string]any{"t": "ping", "corrId": "after-diag"})
	if got := recv(t, c); got["t"] != "pong" || got["corrId"] != "after-diag" {
		t.Fatalf("client_diag produced a reply or broke the connection: %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(buf.String(), "client reported connection diagnostic") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logged := buf.String()
	for _, want := range []string{"client reported connection diagnostic", "kind=slow_command", "command=close_agent", "latency_ms=61000", "rtt_ms=4200"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log missing %q:\n%s", want, logged)
		}
	}
}

// syncBuffer is a log sink safe to read while connection goroutines write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestKeyCommandReceiptIsLogged(t *testing.T) {
	var buf syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, _, _, _, url := setupWSOptions(t, 0, nil, nil)
	c := dial(t, url)
	sent := time.Now().Add(-1500 * time.Millisecond).UnixMilli()
	send(t, c, map[string]any{"t": "get_close_preview", "sessionId": "nope", "corrId": "p1", "clientTs": sent})
	recv(t, c)
	logged := buf.String()
	if !strings.Contains(logged, `msg="websocket command received"`) || !strings.Contains(logged, "type=get_close_preview") || !strings.Contains(logged, "corr_id=p1") {
		t.Fatalf("receipt not logged:\n%s", logged)
	}
	if !strings.Contains(logged, "client_sent_skew_ms=1") {
		t.Fatalf("skew from clientTs not logged:\n%s", logged)
	}
}
