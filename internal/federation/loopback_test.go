package federation

import (
	"encoding/json"
	"testing"

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
