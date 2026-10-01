package messaging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

// Sessions is the live-agent registry the service reads.
type Sessions interface {
	Get(id string) *session.Session
	List() []*session.Session
	// CWD is a live agent's working directory.
	CWD(id string) string
}

// Federation is the part of the federation service messaging uses. It may be
// nil, in which case this is a single-host daemon.
type Federation interface {
	// SelfID is this daemon's stable host ID.
	SelfID() string
	Hosts() []federation.Host
	Call(ctx context.Context, hostID string, payload json.RawMessage) (json.RawMessage, error)
}

type federationEvents interface {
	Subscribe(func(hostID string, payload json.RawMessage)) func()
}

type federationHostNames interface{ HostNames() map[string]string }

// Options configures a Service.
type Options struct {
	Store    *store.Store
	Sessions Sessions
	// Federation is nil on a standalone daemon; SelfID then names this host.
	Federation Federation
	SelfID     string
	// LocalName is this host's display name in directory entries.
	LocalName string
	Now       func() time.Time
	// CallTimeout bounds one federation call (default 20s).
	CallTimeout time.Duration
	// TickInterval is how often the maintenance loop looks for due timeouts
	// (default 1s).
	TickInterval time.Duration
	// PullInterval is how often hosts with outstanding requests are polled for
	// responses they could not push (default 5s).
	PullInterval time.Duration
	// LinkRequestWait is how long messages_request_link blocks for the human's
	// decision before answering "pending" (default DefaultLinkRequestWait).
	LinkRequestWait time.Duration
	// OnSummaryChange is called (asynchronously) when waitingOn/openAsks of
	// some agent changed, so browsers should be sent fresh summaries.
	OnSummaryChange func()
	// OnLinksChanged is called when a session's links, listing or card
	// changed.
	OnLinksChanged func(sessionID string)
	// OnStateChanged is called when the host-wide kill switch changed.
	OnStateChanged func()
	// Token authenticates the agent-facing HTTP bridge (ServeHTTP).
	Token string
}

// Service routes agent messages for one daemon.
type Service struct {
	opts Options
	st   *store.Store

	ctx    context.Context
	cancel context.CancelFunc

	paused atomic.Bool

	bg       sync.WaitGroup
	kick     chan struct{}
	outboxMu sync.Mutex
	pull     *pullState

	linkWaitMu sync.Mutex
	linkWaits  map[string]map[*linkWaiter]struct{}

	lockMu   sync.Mutex
	sessLock map[string]*sync.Mutex

	watchMu sync.Mutex
	watched map[string]watchedSession

	dirMu    sync.Mutex
	dirCache map[string]dirCacheEntry

	sumMu   sync.RWMutex
	waiting map[string][]WaitingOn
	asks    map[string]int

	lastOutbox atomic.Int64
	lastPrune  atomic.Int64
}

type watchedSession struct {
	sess  *session.Session
	unsub func()
}

// New builds a service. Call Start to begin background work.
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Sessions == nil {
		return nil, errors.New("messaging: store and sessions are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 20 * time.Second
	}
	if o.PullInterval <= 0 {
		o.PullInterval = pullInterval
	}
	if o.LinkRequestWait <= 0 {
		o.LinkRequestWait = DefaultLinkRequestWait
	}
	if o.TickInterval <= 0 {
		o.TickInterval = time.Second
	}
	s := &Service{
		opts: o, st: o.Store, kick: make(chan struct{}, 1),
		sessLock: map[string]*sync.Mutex{}, watched: map[string]watchedSession{},
		dirCache: map[string]dirCacheEntry{}, pull: newPullState(), linkWaits: map[string]map[*linkWaiter]struct{}{}, waiting: map[string][]WaitingOn{}, asks: map[string]int{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	paused, err := s.st.AgentMsgMeta(metaPausedKey)
	if err != nil {
		return nil, fmt.Errorf("messaging: read paused flag: %w", err)
	}
	s.paused.Store(paused == "1")
	if err := s.refreshSummary(); err != nil {
		return nil, err
	}
	slog.Info("agent messaging initialised", "paused", s.paused.Load())
	return s, nil
}

func (s *Service) now() time.Time { return s.opts.Now() }

// Start launches the maintenance loop (timeouts, outbox retry), subscribes to
// federation reconnects and re-raises link approvals that were pending when
// the daemon last stopped. It returns immediately; ctx ends the service.
func (s *Service) Start(ctx context.Context) {
	s.cancel()
	s.ctx, s.cancel = context.WithCancel(ctx)
	if events, ok := s.opts.Federation.(federationEvents); ok && s.opts.Federation != nil {
		unsub := events.Subscribe(func(hostID string, payload json.RawMessage) {
			var env struct {
				T string `json:"t"`
			}
			if json.Unmarshal(payload, &env) == nil && env.T == "federation_hosts_changed" {
				slog.Debug("agent messaging: federation topology changed, scheduling outbox flush and pull", "host_id", hostID)
				s.Kick()
				s.kickPull(true)
			}
		})
		go func() { <-s.ctx.Done(); unsub() }()
	}
	s.resumeLinkApprovals()
	s.bg.Add(2)
	go func() {
		defer s.bg.Done()
		s.loop()
	}()
	go func() {
		defer s.bg.Done()
		s.pullLoop()
	}()
	slog.Info("agent messaging started", "tick", s.opts.TickInterval)
}

// Close stops background work and waits for it to finish.
func (s *Service) Close() {
	s.cancel()
	s.Wait()
}

// Wait blocks until background work started so far has finished. Tests use it
// to observe asynchronous sends.
func (s *Service) Wait() { s.bg.Wait() }

// Kick asks the loop to retry the outbox soon (for example after a host
// reconnects).
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Service) loop() {
	t := time.NewTicker(s.opts.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.RunDue(s.ctx, false)
		case <-s.kick:
			s.RunDue(s.ctx, true)
		}
	}
}

// RunDue performs one maintenance pass: it closes asks past their deadline,
// expires stale link requests, retries the outbox (when forceOutbox or the
// retry interval has elapsed) and prunes old rows.
func (s *Service) RunDue(ctx context.Context, forceOutbox bool) {
	now := s.now()
	s.expireRequests(ctx, now)
	s.expireLinkRequests(now)
	last := time.UnixMilli(s.lastOutbox.Load())
	if forceOutbox || now.Sub(last) >= outboxRetryInterval {
		s.lastOutbox.Store(ms(now))
		s.flushOutbox(ctx, forceOutbox)
	}
	if now.Sub(time.UnixMilli(s.lastPrune.Load())) >= time.Hour {
		s.lastPrune.Store(ms(now))
		s.prune(now)
	}
}

func (s *Service) prune(now time.Time) {
	cutoff := ms(now.Add(-pruneAfter))
	for name, fn := range map[string]func(int64) error{
		"inbound": s.st.PruneAgentMsgInbound, "outbox": s.st.PruneAgentMsgOutbox,
		"requests": s.st.PruneAgentMsgRequests, "obligations": s.st.PruneAgentMsgObligations,
		"threads": s.st.PruneAgentMsgThreads,
	} {
		if err := fn(cutoff); err != nil {
			slog.Warn("agent messaging prune failed", "table", name, "error", err)
		}
	}
}

// selfID is this host's stable ID ("" when unknown).
func (s *Service) selfID() string {
	if s.opts.Federation != nil {
		if id := s.opts.Federation.SelfID(); id != "" {
			return id
		}
	}
	return s.opts.SelfID
}

// SelfID is this host's stable ID, the one agent addresses use.
func (s *Service) SelfID() string { return s.selfID() }

func (s *Service) lockFor(sessionID string) *sync.Mutex {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	m := s.sessLock[sessionID]
	if m == nil {
		m = &sync.Mutex{}
		s.sessLock[sessionID] = m
	}
	return m
}

func newID(prefix string) string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}

// addressOf is the Address of a local session.
func (s *Service) addressOf(sess *session.Session) Address {
	return Address{Host: s.selfID(), Agent: sess.ID, Name: sess.DisplayName()}
}

func (s *Service) systemAddress() Address {
	return Address{Host: s.selfID(), Agent: systemAgentID, Name: systemAgentName}
}

// Paused reports the host kill switch.
func (s *Service) Paused() bool { return s.paused.Load() }

// State is the host-wide messaging state.
func (s *Service) State() State { return State{Paused: s.Paused(), HostID: s.selfID()} }

// SetPaused flips the kill switch. While paused the host rejects sends from
// and deliveries to its agents and holds its outbox without consuming retries.
func (s *Service) SetPaused(paused bool) error {
	v := "0"
	if paused {
		v = "1"
	}
	if err := s.st.SetAgentMsgMeta(metaPausedKey, v); err != nil {
		return err
	}
	changed := s.paused.Swap(paused) != paused
	slog.Warn("agent messaging kill switch set", "paused", paused, "changed", changed)
	if changed {
		if s.opts.OnStateChanged != nil {
			s.opts.OnStateChanged()
		}
		if !paused {
			s.Kick()
			s.kickPull(false)
		}
	}
	return nil
}

// SummaryState returns a session's open outbound asks and the number of
// inbound asks it has not answered.
func (s *Service) SummaryState(sessionID string) ([]WaitingOn, int) {
	s.sumMu.RLock()
	defer s.sumMu.RUnlock()
	return append([]WaitingOn(nil), s.waiting[sessionID]...), s.asks[sessionID]
}

// refreshSummary rebuilds the in-memory summary state from the store.
func (s *Service) refreshSummary() error {
	reqs, err := s.st.OpenAgentMsgRequests()
	if err != nil {
		return fmt.Errorf("messaging: load open requests: %w", err)
	}
	obs, err := s.st.OpenAgentMsgObligations("")
	if err != nil {
		return fmt.Errorf("messaging: load open obligations: %w", err)
	}
	waiting := map[string][]WaitingOn{}
	for _, r := range reqs {
		waiting[r.Session] = append(waiting[r.Session], WaitingOn{
			RequestID: r.RequestID, To: Address{Host: r.ToHost, Agent: r.ToAgent, Name: r.ToName},
			Since: rfc3339(r.CreatedAt), Deadline: rfc3339(r.Deadline),
		})
	}
	asks := map[string]int{}
	for _, o := range obs {
		asks[o.Session]++
	}
	s.sumMu.Lock()
	s.waiting, s.asks = waiting, asks
	s.sumMu.Unlock()
	return nil
}

// summaryChanged refreshes the cached summary state and tells the daemon to
// re-broadcast agent summaries.
func (s *Service) summaryChanged() {
	if err := s.refreshSummary(); err != nil {
		slog.Warn("agent messaging summary refresh failed", "error", err)
	}
	if s.opts.OnSummaryChange != nil {
		go s.opts.OnSummaryChange()
	}
}

func (s *Service) linksChanged(sessionID string) {
	if s.opts.OnLinksChanged != nil {
		s.opts.OnLinksChanged(sessionID)
	}
}
