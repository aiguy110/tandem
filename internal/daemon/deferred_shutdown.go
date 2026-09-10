package daemon

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync"
)

type idleRegistry interface {
	ActiveTurnCount() int
	WaitForIdle(context.Context) bool
}

// deferredShutdown implements the localhost control endpoint used by
// redeploy.sh: acknowledge immediately, then stop the daemon once every active
// turn has completed. Repeated requests observe, but do not re-arm, the waiter.
type deferredShutdown struct {
	token  string
	agents idleRegistry
	ctx    context.Context
	ready  chan struct{}

	mu        sync.Mutex
	requested bool
}

func newDeferredShutdown(ctx context.Context, token string, agents idleRegistry) *deferredShutdown {
	return &deferredShutdown{token: token, agents: agents, ctx: ctx, ready: make(chan struct{}, 1)}
}

func (d *deferredShutdown) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/internal/shutdown-after-turns" {
		http.NotFound(w, r)
		return
	}
	if !sameToken(r.URL.Query().Get("token"), d.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}

	_, already := d.Request()

	w.Header().Set("Content-Type", "text/plain")
	flag := 0
	if already {
		flag = 1
	}
	_, _ = fmt.Fprintf(w, "%d %d\n", d.agents.ActiveTurnCount(), flag)
}

// Request schedules a clean shutdown after all active turns finish. It is
// shared by the localhost deployment endpoint and the in-app update action.
func (d *deferredShutdown) Request() (active int, already bool) {
	d.mu.Lock()
	already = d.requested
	if !already {
		d.requested = true
		go d.wait()
	}
	d.mu.Unlock()
	return d.agents.ActiveTurnCount(), already
}

func (d *deferredShutdown) wait() {
	if !d.agents.WaitForIdle(d.ctx) {
		return
	}
	select {
	case d.ready <- struct{}{}:
	default:
	}
}

func sameToken(got, want string) bool {
	return want != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
