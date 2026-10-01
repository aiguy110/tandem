package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestLoopbackDisconnectOnlyFailsWaitersFromOldConnection(t *testing.T) {
	oldConn, newConn := &websocket.Conn{}, &websocket.Conn{}
	oldReply, newReply := make(chan json.RawMessage), make(chan json.RawMessage)
	l := &LoopbackLocal{
		conn: newConn,
		waiters: map[string]loopbackWaiter{
			"old": {conn: oldConn, reply: oldReply},
			"new": {conn: newConn, reply: newReply},
		},
	}

	if got := l.failWaiters(oldConn); got != 1 {
		t.Fatalf("failed waiters = %d, want 1", got)
	}
	if l.conn != newConn {
		t.Fatal("old reader cleared replacement connection")
	}
	if _, ok := <-oldReply; ok {
		t.Fatal("old connection waiter was not closed")
	}
	select {
	case <-newReply:
		t.Fatal("old reader closed replacement connection waiter")
	default:
	}
	if _, ok := l.waiters["new"]; !ok {
		t.Fatal("replacement connection waiter was removed")
	}
}

func TestLoopbackSpawnProgressDoesNotAnswerCommand(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var m struct {
			CorrID json.RawMessage `json:"corrId"`
		}
		if err := conn.ReadJSON(&m); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"t": "spawn_progress", "phase": "Checking…", "corrId": m.CorrID})
		_ = conn.WriteJSON(map[string]any{"t": "ack", "sessionId": "s-1", "corrId": m.CorrID})
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()
	l, err := NewLoopbackLocal(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := l.Execute(ctx, json.RawMessage(`{"t":"spawn_agent"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		T         string `json:"t"`
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatal(err)
	}
	if got.T != "ack" || got.SessionID != "s-1" {
		t.Fatalf("reply = %s, want the spawn ack", reply)
	}
}

// A command executed by the loopback (not relayed onward) still tells
// wsserver who asked: the origin in the call's context is injected as the
// private origin field, unless the payload already carries one.
func TestLoopbackCarriesCommandOriginToBrowserProtocol(t *testing.T) {
	upgrader := websocket.Upgrader{}
	got := make(chan map[string]any, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var m map[string]any
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			got <- m
			_ = conn.WriteJSON(map[string]any{"t": "ack", "corrId": m["corrId"]})
		}
	}))
	defer srv.Close()
	l, err := NewLoopbackLocal(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.Execute(WithOrigin(ctx, "host-a"), json.RawMessage(`{"t":"agent_message_deliver"}`)); err != nil {
		t.Fatal(err)
	}
	if m := <-got; m[OriginField] != "host-a" {
		t.Fatalf("command = %#v, want origin host-a", m)
	}
	if _, err := l.Execute(ctx, json.RawMessage(`{"t":"list_agents"}`)); err != nil {
		t.Fatal(err)
	}
	if m := <-got; m[OriginField] != nil {
		t.Fatalf("a command with no origin must not gain one: %#v", m)
	}
}
