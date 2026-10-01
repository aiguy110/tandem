// Package messaging implements agent-to-agent messaging (docs/agent-messaging.md):
// links, envelopes, delivery, the outbox, asks/replies, reminders, timeouts,
// the directory and the host kill switch.
package messaging

import (
	"strings"
	"time"
)

// Envelope kinds.
const (
	KindSend    = "send"
	KindAsk     = "ask"
	KindReply   = "reply"
	KindDecline = "decline"
	KindSystem  = "system"
)

// System events (Envelope.System.Event).
const (
	EventLinkApproved  = "link_approved"
	EventLinkDenied    = "link_denied"
	EventReplyReminder = "reply_reminder"
	EventNoReply       = "no_reply"
	EventTimeout       = "timeout"
	EventRecipientGone = "recipient_gone"
	EventUndeliverable = "undeliverable"
)

// Limits and defaults.
const (
	DefaultBudgetPerHour = 60
	DefaultMaxHops       = 20
	DefaultAskTimeoutSec = 1800
	MaxAskTimeoutSec     = 86400
	MaxBodyBytes         = 64 << 10
	maxCardLen           = 200
	maxReasonLen         = 500
	maxPendingLinkReqs   = 20
	systemAgentID        = "tandem"
	systemAgentName      = "tandem"
	metaPausedKey        = "paused"
)

const (
	outboxRetryInterval = 15 * time.Second
	outboxMaxAge        = 24 * time.Hour
	directoryCacheTTL   = 30 * time.Second
	linkRequestMaxAge   = 24 * time.Hour
	// DefaultLinkRequestWait is how long messages_request_link blocks for the
	// human's decision before answering "pending".
	DefaultLinkRequestWait = 10 * time.Minute
	pruneAfter             = 7 * 24 * time.Hour
)

// Link delivery modes.
const (
	DeliverySteer = "steer"
	DeliveryQueue = "queue"
)

// Link sources.
const (
	SourceUser     = "user"
	SourceApproval = "approval"
)

// Delivery result statuses and error codes. A Result carries either one of
// the statuses or an Error code.
const (
	StatusSteered = "steered"
	StatusQueued  = "queued"
	StatusStarted = "started"
	StatusPending = "pending"
	// StatusDelivered: a link outcome was handed to the tool call blocked on it.
	StatusDelivered = "delivered"
	StatusRejected  = "rejected"

	ErrNoLink               = "no_link"
	ErrLinkPaused           = "link_paused"
	ErrHopLimit             = "hop_limit"
	ErrBudgetExceeded       = "budget_exceeded"
	ErrMessagingPaused      = "messaging_paused"
	ErrRecipientGone        = "recipient_gone"
	ErrRecipientUnavailable = "recipient_unavailable"
	ErrUnknownRequest       = "unknown_request"
	ErrHostUnreachable      = "host_unreachable"
	ErrAccessDenied         = "access_denied"
	// ErrInvalid reports a malformed request (bad arguments, oversized body).
	ErrInvalid = "invalid_request"
)

// Address names an agent: the stable federation node ID of its host plus its
// Tandem session ID. Name is informational.
type Address struct {
	Host  string `json:"host"`
	Agent string `json:"agent"`
	Name  string `json:"name,omitempty"`
}

// String is the canonical "<host>~<agent>" form.
func (a Address) String() string { return a.Host + "~" + a.Agent }

// Same reports whether a and b name the same agent (ignoring Name).
func (a Address) Same(b Address) bool { return a.Host == b.Host && a.Agent == b.Agent }

// ParseAddress parses "<host>~<agent>".
func ParseAddress(raw string) (Address, bool) {
	host, agent, ok := strings.Cut(strings.TrimSpace(raw), "~")
	if !ok || host == "" || agent == "" {
		return Address{}, false
	}
	return Address{Host: host, Agent: agent}, true
}

// SystemInfo marks a kind=system envelope (or a decline made by the daemon).
type SystemInfo struct {
	Event string `json:"event"`
}

// Envelope is the unit of agent messaging.
type Envelope struct {
	ID         string      `json:"id"`
	ThreadID   string      `json:"threadId"`
	Kind       string      `json:"kind"`
	RequestID  string      `json:"requestId,omitempty"`
	From       Address     `json:"from"`
	To         Address     `json:"to"`
	Body       string      `json:"body"`
	Hop        int         `json:"hop"`
	SentAt     string      `json:"sentAt"`
	TimeoutSec int         `json:"timeoutSec,omitempty"`
	System     *SystemInfo `json:"system,omitempty"`
}

// Result is the outcome of delivering one envelope. Status is one of
// steered|queued|started (accepted) or pending (sender side, held in the
// outbox); on a rejection Status is "rejected" and Error holds the code.
type Result struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Message is a human-readable explanation of Error; it is not part of the
	// wire contract and may be empty.
	Message string `json:"message,omitempty"`
}

// OK reports whether the envelope was accepted (or is safely held).
func (r Result) OK() bool { return r.Error == "" }

func rejected(id, code, message string) Result {
	return Result{ID: id, Status: StatusRejected, Error: code, Message: message}
}

// Error implements error for a rejected Result so tool layers can return it.
type Rejection struct {
	Code    string
	Message string
}

func (e *Rejection) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Link is the wire form of a stored link.
type Link struct {
	ID            string  `json:"id"`
	From          Address `json:"from"`
	To            string  `json:"to"`
	Delivery      string  `json:"delivery"`
	BudgetPerHour int     `json:"budgetPerHour"`
	MaxHops       int     `json:"maxHops"`
	Paused        bool    `json:"paused"`
	Source        string  `json:"source"`
	CreatedAt     string  `json:"createdAt"`
	UsedLastHour  int     `json:"usedLastHour"`
}

// LinkInput is what a human sets on a link (set_agent_link).
type LinkInput struct {
	From          Address `json:"from"`
	Delivery      string  `json:"delivery"`
	BudgetPerHour int     `json:"budgetPerHour"`
	MaxHops       int     `json:"maxHops"`
	Paused        bool    `json:"paused"`
}

// LinksView is the reply to list_agent_links.
type LinksView struct {
	SessionID string `json:"sessionId"`
	Links     []Link `json:"links"`
	Listed    bool   `json:"listed"`
	Card      string `json:"card"`
}

// State is the host-wide messaging state.
type State struct {
	Paused bool `json:"paused"`
	// HostID is this daemon's stable host ID, the one agent addresses use.
	HostID string `json:"hostId"`
}

// WaitingOn is one open outbound ask in an agent summary.
type WaitingOn struct {
	RequestID string  `json:"requestId"`
	To        Address `json:"to"`
	Since     string  `json:"since"`
	Deadline  string  `json:"deadline"`
}

// DirectoryEntry is one agent in the directory.
type DirectoryEntry struct {
	// Ref is the canonical agent reference "@agent:<host>/<agent>" as the
	// host that built the list names things (Service.Ref); agents pass it as
	// the `to` of the other messaging tools.
	Ref     string  `json:"ref"`
	Address Address `json:"address"`
	// HostName is the display name of the host, when known.
	HostName   string `json:"hostName,omitempty"`
	Agent      string `json:"agent"`
	Repo       string `json:"repo"`
	CWD        string `json:"cwd"`
	Card       string `json:"card"`
	Status     string `json:"status"`
	CanMessage bool   `json:"canMessage"`
}

// LinkRequestResult is the reply to agent_link_request.
type LinkRequestResult struct {
	// Status is "pending" (awaiting the human), or, when the call blocked for
	// the decision, "approved" or "denied".
	Status string `json:"status"`
	// Note is a human-readable explanation to show the agent (the outcome
	// notice for a decision, or why the call returned still pending).
	Note string `json:"note,omitempty"`
	// Ref is the reference of the agent the request was about.
	Ref   string `json:"ref,omitempty"`
	Error string `json:"error,omitempty"`
	// Message explains Error; not part of the wire contract.
	Message string `json:"message,omitempty"`
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func rfc3339(unixMilli int64) string {
	return time.UnixMilli(unixMilli).UTC().Format(time.RFC3339)
}

// cleanLabel makes a name safe to embed in a quoted prompt attribute.
func cleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '"', '<', '>', '\n', '\r', '\t':
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
