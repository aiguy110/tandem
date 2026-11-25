package runtimeinstall

import (
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
)

// installLock is the mutex behind installMu, instrumented so a stuck holder is
// visible in the log: every spawn, harness restart and adapter-catalog read
// waits on it, and a holder that never returns silently wedges them all.
// Callers are identified by their calling function, so call sites stay plain
// Lock/Unlock pairs.
type installLock struct {
	mu sync.Mutex

	state   sync.Mutex // guards the fields below
	holder  string
	since   time.Time
	waiters int
	watch   bool
}

const (
	// installLockSlowWait and installLockSlowHold log a wait or hold that ran
	// this long once it ends.
	installLockSlowWait = time.Second
	installLockSlowHold = 10 * time.Second
	// installLockStuckAfter makes the watchdog report a holder still holding
	// after this long, then again every interval while it keeps holding.
	installLockStuckAfter = 2 * time.Minute
)

func (l *installLock) Lock() {
	caller := callerName(2)
	l.state.Lock()
	l.waiters++
	holder, since := l.holder, l.since
	l.state.Unlock()
	if holder != "" {
		slog.Debug("waiting for ACP runtime install lock", "caller", caller, "holder", holder, "held_ms", time.Since(since).Milliseconds())
	}
	started := time.Now()
	l.mu.Lock()
	waited := time.Since(started)
	l.state.Lock()
	l.waiters--
	l.holder, l.since = caller, time.Now()
	if !l.watch {
		l.watch = true
		go l.watchdog()
	}
	l.state.Unlock()
	if waited >= installLockSlowWait {
		slog.Warn("waited for ACP runtime install lock", "caller", caller, "previous_holder", holder, "waited_ms", waited.Milliseconds())
	}
}

func (l *installLock) Unlock() {
	l.state.Lock()
	holder, held := l.holder, time.Since(l.since)
	l.holder, l.since = "", time.Time{}
	l.state.Unlock()
	l.mu.Unlock()
	if held >= installLockSlowHold {
		slog.Warn("held ACP runtime install lock", "holder", holder, "held_ms", held.Milliseconds())
	}
}

// watchdog reports a holder that has not released the lock, with how many
// callers (spawns, restarts, catalog reads) are queued behind it.
func (l *installLock) watchdog() {
	ticker := time.NewTicker(installLockStuckAfter / 2)
	defer ticker.Stop()
	for range ticker.C {
		l.state.Lock()
		holder, since, waiters := l.holder, l.since, l.waiters
		l.state.Unlock()
		if holder != "" && time.Since(since) >= installLockStuckAfter {
			slog.Error("ACP runtime install lock held unusually long; spawns and harness restarts are blocked behind it",
				"holder", holder, "held_ms", time.Since(since).Milliseconds(), "waiters", waiters)
		}
	}
}

// callerName names the function skip frames above callerName's caller, without
// its module path.
func callerName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "unknown"
	}
	name := fn.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}
