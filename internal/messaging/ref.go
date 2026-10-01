package messaging

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RefPrefix starts every canonical agent reference.
const RefPrefix = "@agent:"

// refToken is what a host display name or an agent display name must look like
// to be used verbatim in a reference; anything else falls back to the stable ID.
var refToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// hostNameTable maps a host's stable node ID to the display name THIS host
// shows for it (the browser host list's name, including operator renames).
// The local host maps to its own name. A host without a name maps to "".
type hostNameTable map[string]string

// hostNames builds the table for this host and every host in the federation
// view. It reads the federation host list once, so callers formatting many
// references should build it once and use refFor.
func (s *Service) hostNames() hostNameTable {
	self := s.selfID()
	t := hostNameTable{}
	var names map[string]string
	if n, ok := s.opts.Federation.(federationHostNames); ok {
		names = n.HostNames()
	}
	if self != "" {
		// The browser lists the local host under the route ID "local", named
		// "This host" unless renamed; that default is not a ref token, so the
		// UI and this table both fall back to the node ID. LocalName stays a
		// resolvable alias (see resolveRef).
		t[self] = names["local"]
	}
	if s.opts.Federation == nil {
		return t
	}
	for _, h := range s.opts.Federation.Hosts() {
		if h.Local || h.NodeID == "" || h.NodeID == self {
			continue
		}
		name := firstNonEmpty(names[h.ID], h.Name)
		if _, seen := t[h.NodeID]; seen && h.Status != "connected" {
			continue
		}
		t[h.NodeID] = name
	}
	return t
}

// refFor formats the canonical reference using a prebuilt name table.
func refFor(t hostNameTable, a Address) string {
	host := a.Host
	if name := t[a.Host]; refToken.MatchString(name) {
		host = name
	}
	agent := a.Agent
	if refToken.MatchString(a.Name) {
		agent = a.Name
	}
	return RefPrefix + cleanLabel(host) + "/" + cleanLabel(agent)
}

// Ref is the canonical reference to an agent: "@agent:<host>/<agent>".
//
// <host> is the display name this host uses for the agent's host (the name the
// browser host list shows, including operator renames; the local host's own
// name for itself) when it matches ^[A-Za-z0-9][A-Za-z0-9._-]*$, otherwise the
// host's stable node ID. <agent> is the agent's display name when it matches
// the same pattern, otherwise its session ID. Names are viewer-relative, so a
// reference is only meaningful to the host that formatted it; every agent-facing
// tool and prompt is rendered by the host the agent runs on.
func (s *Service) Ref(a Address) string { return refFor(s.hostNames(), a) }

// parseRef parses "@agent:<host>/<agent>" (the leading "@" optional,
// case-insensitive prefix). ok is false when raw is not in that form.
func parseRef(raw string) (host, agent string, ok bool) {
	raw = strings.TrimSpace(raw)
	rest := strings.TrimPrefix(raw, "@")
	if len(rest) < len("agent:") || !strings.EqualFold(rest[:len("agent:")], "agent:") {
		return "", "", false
	}
	host, agent, _ = strings.Cut(rest[len("agent:"):], "/")
	return strings.TrimSpace(host), strings.TrimSpace(agent), true
}

// matchHost finds the node ID a reference's <host> names: an exact (case-
// insensitive) node ID, else the single host whose display name matches.
func matchHost(t hostNameTable, sel string) (string, error) {
	for id := range t {
		if strings.EqualFold(id, sel) {
			return id, nil
		}
	}
	var ids []string
	for id, name := range t {
		if name != "" && strings.EqualFold(name, sel) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	switch len(ids) {
	case 0:
		return "", &Rejection{Code: ErrInvalid, Message: fmt.Sprintf("no reachable host %q; use messages_directory to find agents", sel)}
	case 1:
		return ids[0], nil
	}
	return "", &Rejection{Code: ErrInvalid, Message: fmt.Sprintf("host name %q is ambiguous; use @agent:<host-id>/<agent> with one of these host IDs: %s", sel, strings.Join(ids, ", "))}
}

// candidateRefs lists references for entries a caller must choose between.
// Where two share a name the session ID is used so every reference is unique.
func candidateRefs(t hostNameTable, entries []DirectoryEntry) string {
	count := map[string]int{}
	for _, e := range entries {
		count[strings.ToLower(refFor(t, e.Address))]++
	}
	refs := make([]string, len(entries))
	for i, e := range entries {
		a := e.Address
		if count[strings.ToLower(refFor(t, a))] > 1 {
			a.Name = ""
		}
		refs[i] = refFor(t, a)
	}
	return strings.Join(refs, ", ")
}
