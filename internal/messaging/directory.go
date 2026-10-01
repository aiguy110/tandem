package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/workspace"
)

type dirCacheEntry struct {
	at      time.Time
	entries []DirectoryEntry
}

func (s *Service) invalidateDirectory() {
	s.dirMu.Lock()
	s.dirCache = map[string]dirCacheEntry{}
	s.dirMu.Unlock()
}

// LocalDirectory lists this host's open, listed agents. canMessage reports
// whether requester (when given) holds a non-paused link to each.
func (s *Service) LocalDirectory(requester *Address, query string) []DirectoryEntry {
	self := s.selfID()
	settings, err := s.st.AllAgentMsgSettings()
	if err != nil {
		slog.Warn("agent directory settings lookup failed", "error", err)
		settings = nil
	}
	entries := []DirectoryEntry{}
	names := s.hostNames()
	for _, sess := range s.opts.Sessions.List() {
		cfg, hasCfg := settings[sess.ID]
		if hasCfg && !cfg.Listed {
			continue
		}
		if requester != nil && requester.Host == self && requester.Agent == sess.ID {
			continue
		}
		name := sess.DisplayName()
		card := cfg.Card
		if card == "" {
			card = name
		}
		addr := Address{Host: self, Agent: sess.ID, Name: name}
		entry := DirectoryEntry{
			Ref: refFor(names, addr), Address: addr, HostName: s.opts.LocalName,
			Agent: sess.Spec.Agent, Repo: repoOf(sess), CWD: s.opts.Sessions.CWD(sess.ID), Card: card, Status: string(sess.Status()),
		}
		if requester != nil {
			if link, err := s.st.AgentMsgLink(requester.Host, requester.Agent, sess.ID); err == nil && link != nil && !link.Paused {
				entry.CanMessage = true
			}
		}
		if matchesQuery(entry, query) {
			entries = append(entries, entry)
		}
	}
	sortEntries(entries)
	return entries
}

func repoOf(sess *session.Session) string {
	ws := sess.Spec.Workspace
	switch {
	case ws.Kind == workspace.KindExisting && ws.CWD != "":
		return filepath.Base(ws.CWD)
	case ws.Repo != "":
		return filepath.Base(ws.Repo)
	}
	return ""
}

func matchesQuery(e DirectoryEntry, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, field := range []string{e.Address.Name, e.Card, e.Repo, e.CWD, e.Agent, e.HostName, e.Address.Host, e.Address.Agent} {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

func sortEntries(entries []DirectoryEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Address.Host != entries[j].Address.Host {
			return entries[i].Address.Host < entries[j].Address.Host
		}
		return entries[i].Address.Name < entries[j].Address.Name
	})
}

// Directory is the agent-facing directory: this host's listed agents plus
// those of every reachable host that grants this host the message level,
// cached for 30 seconds.
func (s *Service) Directory(ctx context.Context, sessionID, query string) ([]DirectoryEntry, error) {
	sess, err := s.liveSession(sessionID)
	if err != nil {
		return nil, err
	}
	requester := s.addressOf(sess)
	entries := s.LocalDirectory(&requester, "")
	entries = append(entries, s.remoteDirectories(ctx, requester)...)
	// References are viewer-relative: a remote host formatted its own, so
	// every entry is re-rendered with the names this host uses.
	names := s.hostNames()
	out := entries[:0]
	for _, e := range entries {
		e.Ref = refFor(names, e.Address)
		if matchesQuery(e, query) {
			out = append(out, e)
		}
	}
	sortEntries(out)
	return out, nil
}

// remoteDirectories queries every federation host that grants message access.
func (s *Service) remoteDirectories(ctx context.Context, requester Address) []DirectoryEntry {
	if s.opts.Federation == nil {
		return nil
	}
	var names map[string]string
	if n, ok := s.opts.Federation.(federationHostNames); ok {
		names = n.HostNames()
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}
	var all []DirectoryEntry
	for _, h := range s.opts.Federation.Hosts() {
		if h.Local || h.NodeID == "" || h.NodeID == s.selfID() || seen[h.NodeID] {
			continue
		}
		level, err := federation.ParseLevel(h.Access)
		if err != nil || level < federation.LevelMessage || h.Status != "connected" {
			continue
		}
		seen[h.NodeID] = true
		hostName := h.Name
		if n := names[h.ID]; n != "" {
			hostName = n
		}
		wg.Add(1)
		go func(h federation.Host, hostName string) {
			defer wg.Done()
			entries := s.fetchDirectory(ctx, h.NodeID, hostName, requester)
			mu.Lock()
			all = append(all, entries...)
			mu.Unlock()
		}(h, hostName)
	}
	wg.Wait()
	return all
}

func (s *Service) fetchDirectory(ctx context.Context, hostID, hostName string, requester Address) []DirectoryEntry {
	key := hostID + "|" + requester.String()
	s.dirMu.Lock()
	cached, ok := s.dirCache[key]
	s.dirMu.Unlock()
	if ok && s.now().Sub(cached.at) < directoryCacheTTL {
		return cached.entries
	}
	raw, denied, err := s.callFederation(ctx, hostID, map[string]any{"t": "agent_directory", "requester": requester})
	if err == nil && denied != nil {
		err = fmt.Errorf("%s", denied.Message)
	}
	var resp struct {
		T       string           `json:"t"`
		Entries []DirectoryEntry `json:"entries"`
		Error   string           `json:"error"`
	}
	if err == nil {
		if jerr := json.Unmarshal(raw, &resp); jerr != nil {
			err = jerr
		} else if resp.T != "agent_directory" {
			err = fmt.Errorf("unexpected reply %q: %s", resp.T, resp.Error)
		}
	}
	if err != nil {
		slog.Warn("agent directory fetch failed", "host_id", hostID, "error", err, "stale_cache", ok)
		if ok {
			return cached.entries
		}
		return nil
	}
	for i := range resp.Entries {
		// The host is authoritative for its own address, but the display
		// name is the viewer's choice.
		if resp.Entries[i].HostName == "" || hostName != "" {
			resp.Entries[i].HostName = hostName
		}
	}
	s.dirMu.Lock()
	s.dirCache[key] = dirCacheEntry{at: s.now(), entries: resp.Entries}
	s.dirMu.Unlock()
	slog.Debug("agent directory fetched", "host_id", hostID, "entries", len(resp.Entries))
	return resp.Entries
}

// resolve turns an agent-supplied recipient string into an Address.
//
// The canonical form is "@agent:<host>/<agent>" (see Ref; the "@" is optional
// and matching is case-insensitive): <host> is a reachable host's display name
// or node ID, <agent> an agent's display name or session ID on that host. The
// older "<host>~<agent>", "@name", "name" and "name@<host-id-or-name>" forms
// are still accepted.
func (s *Service) resolve(ctx context.Context, sess *session.Session, to string) (Address, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return Address{}, &Rejection{Code: ErrInvalid, Message: "to is required"}
	}
	if hostSel, agentSel, ok := parseRef(to); ok {
		return s.resolveRef(ctx, sess, to, hostSel, agentSel)
	}
	if addr, ok := ParseAddress(to); ok {
		return addr, nil
	}
	name, hostSel, _ := strings.Cut(strings.TrimPrefix(to, "@"), "@")
	name, hostSel = strings.TrimSpace(name), strings.TrimSpace(hostSel)
	if name == "" {
		return Address{}, &Rejection{Code: ErrInvalid, Message: "to must be an agent reference like @agent:<host>/<agent> (see messages_directory)"}
	}
	entries, err := s.Directory(ctx, sess.ID, "")
	if err != nil {
		return Address{}, err
	}
	var matches []DirectoryEntry
	for _, e := range entries {
		if !strings.EqualFold(e.Address.Name, name) {
			continue
		}
		if hostSel != "" && e.Address.Host != hostSel && !strings.EqualFold(e.HostName, hostSel) {
			continue
		}
		matches = append(matches, e)
	}
	switch len(matches) {
	case 0:
		return Address{}, &Rejection{Code: ErrInvalid, Message: fmt.Sprintf("no listed agent named %q; use messages_directory to find agents", to)}
	case 1:
		return matches[0].Address, nil
	}
	return Address{}, &Rejection{Code: ErrInvalid, Message: fmt.Sprintf("%q is ambiguous; use one of: %s", to, candidateRefs(s.hostNames(), matches))}
}

// resolveRef resolves "@agent:<host>/<agent>".
func (s *Service) resolveRef(ctx context.Context, sess *session.Session, raw, hostSel, agentSel string) (Address, error) {
	if hostSel == "" || agentSel == "" {
		return Address{}, &Rejection{Code: ErrInvalid, Message: fmt.Sprintf("%q is not an agent reference; use @agent:<host>/<agent> from messages_directory", raw)}
	}
	names := s.hostNames()
	hostID, err := matchHost(names, hostSel)
	if err != nil && s.opts.LocalName != "" && strings.EqualFold(hostSel, s.opts.LocalName) {
		hostID, err = s.selfID(), nil
	}
	if err != nil {
		return Address{}, err
	}
	entries, err := s.Directory(ctx, sess.ID, "")
	if err != nil {
		return Address{}, err
	}
	var byID, byName []DirectoryEntry
	for _, e := range entries {
		if e.Address.Host != hostID {
			continue
		}
		if strings.EqualFold(e.Address.Agent, agentSel) {
			byID = append(byID, e)
		} else if strings.EqualFold(e.Address.Name, agentSel) {
			byName = append(byName, e)
		}
	}
	matches := byID
	if len(matches) == 0 {
		matches = byName
	}
	switch len(matches) {
	case 0:
		return Address{}, &Rejection{Code: ErrInvalid, Message: fmt.Sprintf("no listed agent %q on host %q; use messages_directory to find agents", agentSel, hostSel)}
	case 1:
		return matches[0].Address, nil
	}
	return Address{}, &Rejection{Code: ErrInvalid, Message: fmt.Sprintf("%q is ambiguous; use one of: %s", raw, candidateRefs(names, matches))}
}

// SetCard sets the one-line purpose shown for an agent in the directory.
func (s *Service) SetCard(sessionID, card string) error {
	if _, err := s.liveSession(sessionID); err != nil {
		return err
	}
	card = strings.TrimSpace(card)
	if len(card) > maxCardLen {
		card = truncate(card, maxCardLen)
	}
	if err := s.st.SetAgentMsgCard(sessionID, card); err != nil {
		return err
	}
	s.invalidateDirectory()
	s.linksChanged(sessionID)
	return nil
}
