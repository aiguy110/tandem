package browser

import (
	"context"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// transportMutex bounds contention as well as the network operation itself.
// A context timeout on a CDP response alone cannot interrupt Mutex.Lock or WriteMessage.
type transportMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *transportMutex) lock(ctx context.Context) error {
	m.once.Do(func() { m.token = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.token <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *transportMutex) unlock() { <-m.token }

func writeTransport(ctx context.Context, mu *transportMutex, conn *websocket.Conn, kind int, data []byte) error {
	if err := mu.lock(ctx); err != nil {
		return err
	}
	defer mu.unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return conn.WriteMessage(kind, data)
}
