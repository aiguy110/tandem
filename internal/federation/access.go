package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// Level is how much a remote host may do to this one. Levels are ordered:
// each includes everything below it.
type Level int

const (
	LevelNone Level = iota
	// LevelView reads state: agent lists, transcripts and screencasts
	// (subscriptions), history search, diffs and listings.
	LevelView
	// LevelMessage lets agents on the other host message agents here
	// (agent messaging, docs/agent-messaging.md): discover listed agents,
	// deliver envelopes and request links. It includes view. Delivery is still
	// default-deny per agent until a human grants a link.
	LevelMessage
	// LevelOperate drives agents that already exist: prompts, interrupts,
	// approvals, terminal/shell and browser input, renames.
	LevelOperate
	// LevelAdmin changes what exists on the host: spawn, resume, close,
	// settings, installs, notification actions (including federation
	// approvals), and every command not explicitly classified.
	LevelAdmin
)

func (l Level) String() string {
	switch l {
	case LevelView:
		return "view"
	case LevelMessage:
		return "message"
	case LevelOperate:
		return "operate"
	case LevelAdmin:
		return "admin"
	}
	return "none"
}

// ParseLevel accepts the names Level.String produces.
func ParseLevel(raw string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none":
		return LevelNone, nil
	case "view":
		return LevelView, nil
	case "message":
		return LevelMessage, nil
	case "operate":
		return LevelOperate, nil
	case "admin":
		return LevelAdmin, nil
	}
	return LevelNone, fmt.Errorf("unknown federation access level %q (want none, view, message, operate or admin)", raw)
}

func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }
func (l *Level) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	parsed, err := ParseLevel(raw)
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}

// AncestorsSubject matches any host above this one in the federation tree:
// its parent, its parent's parent, and so on.
const AncestorsSubject = "ancestors"

// AccessRule grants Level to the hosts matched by From: a host-ID glob
// (path.Match syntax, e.g. "laptop-*"), "*", or AncestorsSubject.
type AccessRule struct {
	From  string `json:"from"`
	Level Level  `json:"level"`
}

// Policy is the host-to-host access policy a daemon applies to commands it
// executes for remote hosts. The first matching rule wins; after the
// configured rules come the implicit defaults, ancestors: admin and *: none.
// The defaults preserve the original federation contract -- an accepted
// parent controls its subtree -- while control from anywhere else is opt-in.
type Policy []AccessRule

// Validate rejects rules that could never match or would be misread.
func (p Policy) Validate() error {
	for i, rule := range p {
		if strings.TrimSpace(rule.From) == "" {
			return fmt.Errorf("federation access rule %d: from is required", i+1)
		}
		if rule.From == AncestorsSubject || rule.From == "*" {
			continue
		}
		if _, err := path.Match(rule.From, ""); err != nil {
			return fmt.Errorf("federation access rule %d: invalid pattern %q: %w", i+1, rule.From, err)
		}
	}
	return nil
}

// LevelFor evaluates the policy for a command or view requested by origin.
// ancestor reports whether origin is above this host in the tree; an
// unattributed request from the upstream link (an older parent) counts as one.
func (p Policy) LevelFor(origin string, ancestor bool) Level {
	for _, rule := range p {
		if rule.matches(origin, ancestor) {
			return rule.Level
		}
	}
	if ancestor {
		return LevelAdmin
	}
	return LevelNone
}

func (r AccessRule) matches(origin string, ancestor bool) bool {
	switch r.From {
	case AncestorsSubject:
		return ancestor
	case "*":
		return true
	}
	if origin == "" {
		return false
	}
	ok, err := path.Match(r.From, origin)
	return err == nil && ok
}

// commandLevels classifies browser-protocol commands. The tunnel carries them
// opaquely, so this table is the one place federation understands them. A
// type missing here requires LevelAdmin: a new command stays denied to
// restricted hosts until someone decides what it is.
var commandLevels = map[string]Level{
	"list_hosts":                LevelView,
	"subscribe":                 LevelView,
	"unsubscribe":               LevelView,
	"list_agents":               LevelView,
	"list_dirs":                 LevelView,
	"list_workspace_entries":    LevelView,
	"list_automation":           LevelView,
	"list_git_refs":             LevelView,
	"list_agent_catalog":        LevelView,
	"list_sessions":             LevelView,
	"search_sessions":           LevelView,
	"history_status":            LevelView,
	"list_system_notifications": LevelView,
	"list_agent_distributions":  LevelView,
	"get_spawn_options":         LevelView,
	"list_snapshots":            LevelView,
	"list_profiles":             LevelView,
	"render_message_audio":      LevelView,
	"get_asset":                 LevelView,
	"has_upload_directory":      LevelView,
	"get_close_preview":         LevelView,
	"get_diff":                  LevelView,
	"get_messaging_state":       LevelView,
	"list_agent_links":          LevelView,

	"agent_directory":       LevelMessage,
	"agent_message_deliver": LevelMessage,
	"agent_link_request":    LevelMessage,
	// Pull fallback: a host that cannot push responses to a requester holds
	// them until the requester fetches (and acknowledges) them.
	"agent_message_pull":     LevelMessage,
	"agent_message_pull_ack": LevelMessage,

	"set_agent_link":       LevelOperate,
	"delete_agent_link":    LevelOperate,
	"set_agent_listed":     LevelOperate,
	"set_messaging_paused": LevelOperate,

	"refresh_history":           LevelOperate,
	"enter_terminal":            LevelOperate,
	"leave_terminal":            LevelOperate,
	"shell_open":                LevelOperate,
	"shell_input":               LevelOperate,
	"shell_resize":              LevelOperate,
	"shell_close":               LevelOperate,
	"prompt":                    LevelOperate,
	"steer":                     LevelOperate,
	"aside":                     LevelOperate,
	"remove_queued_prompt":      LevelOperate,
	"clear_prompt_queue":        LevelOperate,
	"interrupt_and_clear_queue": LevelOperate,
	"input":                     LevelOperate,
	"resize":                    LevelOperate,
	"permission_response":       LevelOperate,
	"interrupt":                 LevelOperate,
	"set_mode":                  LevelOperate,
	"set_config_option":         LevelOperate,
	"capture_snapshot":          LevelOperate,
	"reorder_session":           LevelOperate,
	"rename_agent":              LevelOperate,
	"set_audio_enabled":         LevelOperate,
	"set_audio_focus":           LevelOperate,
	"set_audio_position":        LevelOperate,
	"save_upload":               LevelOperate,
	"put_asset":                 LevelOperate,
	"browser_control":           LevelOperate,
	"grab":                      LevelOperate,
	"release":                   LevelOperate,
	"restart_browser":           LevelOperate,
	"restart_harness":           LevelOperate,
	"browser_input":             LevelOperate,
	"add_annotation":            LevelOperate,
	"update_annotation":         LevelOperate,
	"delete_annotation":         LevelOperate,
	"clear_annotations":         LevelOperate,
}

// accessDeniedMarker is part of the error text authorize produces. The
// denial crosses the tunnel as a plain string, so a caller recognizes it by
// this marker (see IsAccessDenied).
const accessDeniedMarker = "may not "

// IsAccessDenied reports whether err is a remote host's access-policy denial,
// as opposed to a transport failure. A denial will not clear up by retrying.
func IsAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "federation: ") && strings.Contains(msg, accessDeniedMarker) && strings.Contains(msg, "(needs ")
}

// CommandLevel is the level a browser-protocol command type requires.
func CommandLevel(commandType string) Level {
	if level, ok := commandLevels[commandType]; ok {
		return level
	}
	return LevelAdmin
}

type originKey struct{}

// WithOrigin attributes a routed command to the host that originated it. A
// relaying daemon sets this so the next hop, and ultimately the target, can
// apply its policy to the real requester rather than to the relay.
func WithOrigin(ctx context.Context, origin string) context.Context {
	if origin == "" {
		return ctx
	}
	return context.WithValue(ctx, originKey{}, origin)
}

// OriginFrom returns the origin set by WithOrigin, or "".
func OriginFrom(ctx context.Context) string {
	origin, _ := ctx.Value(originKey{}).(string)
	return origin
}

// OriginField is the private browser-envelope field a daemon uses to carry a
// relayed command's origin through its own loopback socket into wsserver.
// Only a holder of the daemon's browser token can set it, and that holder
// already has every right the daemon has.
const OriginField = "federationOrigin"

// envelopeType extracts a command's "t" and the next-hop "hostId".
func envelopeRoute(payload json.RawMessage) (commandType, hostID string) {
	var envelope struct {
		T      string `json:"t"`
		HostID string `json:"hostId"`
	}
	_ = json.Unmarshal(payload, &envelope)
	return envelope.T, envelope.HostID
}

// withEnvelopeField returns payload with one top-level field set (or removed
// when value is nil).
func withEnvelopeField(payload json.RawMessage, key string, value any) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	if value == nil {
		delete(envelope, key)
	} else {
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		envelope[key] = b
	}
	return json.Marshal(envelope)
}
