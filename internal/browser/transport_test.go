package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Complete the handshake, then deliberately stop reading from the peer.
func stalledTransport(t *testing.T) *websocket.Conn {
	t.Helper()
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-stop
	}))
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); close(stop); server.Close() })
	return conn
}

func TestCDPContentionHonorsContext(t *testing.T) {
	shared := NewSharedBrowser("unused")
	if err := shared.connectMu.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer shared.connectMu.unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := shared.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connect lock error = %v", err)
	}
	conn := stalledTransport(t)
	if err := shared.writeMu.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer shared.writeMu.unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := shared.callOn(ctx2, conn, "", "Input.dispatchMouseEvent", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write lock error = %v", err)
	}
	if len(shared.pending) != 0 {
		t.Fatal("timed-out call leaked a pending response")
	}
}

func TestReleaseStalledWriteDoesNotHoldBrowserState(t *testing.T) {
	conn := stalledTransport(t)
	broker := NewBroker(nil, BrokerConfig{})
	state := broker.record("stalled")
	state.owner = ControlUser
	link := &proxyLink{agent: conn, upstream: conn, held: []heldFrame{{kind: websocket.TextMessage, data: make([]byte, 16<<20)}}}
	state.links[link] = struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- broker.ReleaseContext(ctx, "stalled") }()
	// Wait for Release to detach the batch and reach the stalled write.
	deadline := time.Now().Add(time.Second)
	for {
		state.mu.Lock()
		detached := len(link.held) == 0
		state.mu.Unlock()
		if detached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("release did not start")
		}
		time.Sleep(time.Millisecond)
	}
	owner := make(chan ControlOwner, 1)
	go func() { owner <- broker.Owner("stalled") }()
	select {
	case got := <-owner:
		if got != ControlUser {
			t.Fatalf("ownership changed before draining: %s", got)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("stalled release held the browser state lock")
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("stalled write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("release did not respect its deadline")
	}
}
