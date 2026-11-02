package wsserver

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// peerInfo describes where a browser socket came from. It is logged once per
// connection so a slow-link report can be tied to the network path (a tailnet
// peer, a `tailscale serve` proxy, loopback) without guessing.
type peerInfo struct {
	remoteAddr        string
	network           string
	forwardedFor      string
	viaTailscaleServe bool
	userAgent         string
	host              string
}

var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

func newPeerInfo(r *http.Request) peerInfo {
	p := peerInfo{
		remoteAddr:        r.RemoteAddr,
		forwardedFor:      r.Header.Get("X-Forwarded-For"),
		viaTailscaleServe: r.Header.Get("Tailscale-User-Login") != "",
		userAgent:         r.UserAgent(),
		host:              r.Host,
	}
	p.network = classifyNetwork(r.RemoteAddr)
	if p.network == "loopback" && p.forwardedFor != "" {
		// A local reverse proxy (tailscale serve, nginx) hides the real peer;
		// its first forwarded hop is the client.
		first, _, _ := strings.Cut(p.forwardedFor, ",")
		p.network = "proxied:" + classifyNetwork(strings.TrimSpace(first))
	}
	return p
}

func classifyNetwork(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback():
		return "loopback"
	case tailscaleV4.Contains(ip), tailscaleV6.Contains(ip):
		return "tailscale"
	case ip.IsPrivate(), ip.IsLinkLocalUnicast():
		return "lan"
	default:
		return "other"
	}
}

// clientDiag is a latency observation the browser measured and reports back so
// it lands in the daemon log next to the server-side view of the same moment.
type clientDiag struct {
	Kind          string `json:"kind"`
	Command       string `json:"command,omitempty"`
	CorrID        string `json:"corrId,omitempty"`
	LatencyMS     int64  `json:"latencyMs,omitempty"`
	QueuedMS      int64  `json:"queuedMs,omitempty"`
	BufferedBytes int64  `json:"bufferedBytes,omitempty"`
	RTTMS         int64  `json:"rttMs,omitempty"`
	StaleMS       int64  `json:"staleMs,omitempty"`
	Visible       *bool  `json:"visible,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// commandsLoggedOnReceipt are the user-initiated actions whose arrival time is
// worth recording: when one feels stuck, comparing the browser's send time with
// this log line separates a late send from a late daemon.
var commandsLoggedOnReceipt = map[string]bool{
	"spawn_agent":       true,
	"close_agent":       true,
	"get_close_preview": true,
	"get_spawn_options": true,
	"fork_session":      true,
	"resume_session":    true,
}

// clientSkewMS is receipt time minus the browser's send stamp. It folds in any
// clock offset between the two machines, so read it relative to the baseline
// from other commands on the same connection rather than as an absolute.
func clientSkewMS(m clientMessage) any {
	if m.ClientTS <= 0 {
		return nil
	}
	return time.Now().UnixMilli() - m.ClientTS
}

func (c *connection) logCommandReceived(m clientMessage) {
	if !commandsLoggedOnReceipt[m.T] {
		return
	}
	slog.Info("websocket command received", "connection_id", c.id, "type", m.T, "session_id", m.SessionID, "corr_id", rawCorr(m.CorrID), "client_sent_skew_ms", clientSkewMS(m))
}

func (c *connection) logClientDiag(m clientMessage) {
	d := m.Diag
	if d == nil {
		return
	}
	detail := d.Detail
	if len(detail) > 200 {
		detail = detail[:200]
	}
	args := []any{
		"connection_id", c.id, "kind", d.Kind, "command", d.Command, "corr_id", d.CorrID,
		"latency_ms", d.LatencyMS, "queued_ms", d.QueuedMS, "buffered_bytes", d.BufferedBytes,
		"rtt_ms", d.RTTMS, "stale_ms", d.StaleMS, "client_sent_skew_ms", clientSkewMS(m),
		"remote_addr", c.peer.remoteAddr, "network", c.peer.network, "detail", detail,
	}
	if d.Visible != nil {
		args = append(args, "visible", *d.Visible)
	}
	slog.Warn("client reported connection diagnostic", args...)
}

func rawCorr(raw json.RawMessage) string {
	return strings.Trim(string(raw), `"`)
}
