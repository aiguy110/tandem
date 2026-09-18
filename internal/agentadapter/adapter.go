// Package agentadapter defines the normalized lifecycle seam owned by sessions.
package agentadapter

import (
	"context"
	"encoding/json"

	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/workspace"
)

type Capabilities struct {
	Structured, Terminals, LoadSession, ForkSession, FS, Image, Steering bool
}

// SteeringAdapter is the optional ACP mid-turn input extension. Steer injects
// content into the currently running turn instead of starting another turn.
type SteeringAdapter interface {
	Steer(context.Context, []PromptBlock) error
}

// AsideAdapter is the optional ACP draft session/fork surface. The returned
// events are emitted by the adapter as durable aside_event wrappers.
type AsideAdapter interface {
	Aside(context.Context, string, []PromptBlock) (string, error)
}

type PromptBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	AssetID  string `json:"assetId,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
	// RefSeq/Role/Quote/Comment carry a "quote" block: a transcript annotation
	// sent as a citation of the original text plus the user's note (see
	// docs/transcript-annotations.md). ACP adapters don't understand this
	// variant; the daemon flattens it to a text block before handing prompts to
	// the adapter but keeps the rich block in the persisted user_message event.
	RefSeq  int64  `json:"refSeq,omitempty"`
	Role    string `json:"role,omitempty"`
	Quote   string `json:"quote,omitempty"`
	Comment string `json:"comment,omitempty"`
}

type ApprovalOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
}
type Approval struct {
	ReqID      string           `json:"reqId"`
	ToolCallID string           `json:"toolCallId"`
	Title      string           `json:"title"`
	Options    []ApprovalOption `json:"options"`
}

// Adapter is already started when returned by Factory. Implementations retain
// ownership of their subprocess; Session owns all calls and disposal.
type Adapter interface {
	Capabilities() Capabilities
	Events() <-chan eventlog.Event
	Done() <-chan struct{}
	Prompt(context.Context, []PromptBlock) (string, error)
	SendInput([]byte) error
	Resize(cols, rows uint16) error
	RespondPermission(reqID, optionID string) error
	Interrupt() error
	Close(context.Context) error
	ExternalSessionID() string
	PID() int
}

type Launch struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env,omitempty"`
	// ParentToolCallIDPath is the resolved `acp.meta.parentToolCallIdPath`: a
	// dotted path into an ACP session/update's `_meta` whose value is the parent
	// tool call's id. Empty = the agent exposes no such annotation.
	ParentToolCallIDPath string `json:"parentToolCallIdPath,omitempty"`
}
type TerminalLaunch struct {
	Cmd        string            `json:"cmd"`
	StartArgs  []string          `json:"startArgs,omitempty"`
	ResumeArgs []string          `json:"resumeArgs,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}
type ResolvedLaunch struct {
	Agent        string          `json:"agent"`
	ACP          *Launch         `json:"acp,omitempty"`
	Terminal     *TerminalLaunch `json:"terminal,omitempty"`
	Distribution *Distribution   `json:"distribution,omitempty"`
}

// Distribution pins a daemon-managed ACP adapter to an immutable install.
// Persisting it with the session prevents a later update from changing the
// executable used when that session is restored.
type Distribution struct {
	Source    string `json:"source"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	Integrity string `json:"integrity,omitempty"`
	Path      string `json:"path"`
}
type Spec struct {
	// HostID selects a registered federation host at the browser boundary. It
	// is consumed by wsserver and ignored by the local registry.
	HostID        string              `json:"hostId,omitempty"`
	Adapter       string              `json:"adapter"`
	Agent         string              `json:"agent,omitempty"`
	Harness       string              `json:"harness,omitempty"`
	ACPArgs       []string            `json:"acpArgs,omitempty"`
	TerminalArgs  []string            `json:"terminalArgs,omitempty"`
	Workspace     workspace.Workspace `json:"workspace"`
	Name          string              `json:"name,omitempty"`
	Task          string              `json:"task,omitempty"`
	SessionConfig json.RawMessage     `json:"sessionConfig,omitempty"`
	Preset        string              `json:"preset,omitempty"`
	// HandoffFrom is the agent id whose transcript should be rendered into this
	// agent's first user message (see internal/handoff). It is recorded on the
	// spec so a restored agent still shows where it came from.
	HandoffFrom string `json:"handoffFrom,omitempty"`
	// HandoffMode is "full" (default) or "brief"; see internal/handoff.Mode.
	HandoffMode    string          `json:"handoffMode,omitempty"`
	Profile        *ProfileSpec    `json:"profile,omitempty"`
	ResolvedLaunch *ResolvedLaunch `json:"resolvedLaunch,omitempty"`
}

// ProfileSpec carries the human-facing settings that identify and name a
// daemon-owned Profile, plus the browser snapshot to seed. Model/Effort/
// Permission mirror what SessionConfig applies to the ACP session; they are kept
// here in canonical form for profile identity and auto-naming. Snapshot is a
// browser snapshot id ("" = fresh browser state). When present, the daemon
// resolves-or-creates a Profile from these settings at spawn.
type ProfileSpec struct {
	ID         string `json:"id,omitempty"` // resolved/created profile id (filled in by the daemon)
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
}

type StartRequest struct {
	SessionID, CWD, ResumeSessionID string
	CaptureReplay                   bool
	Spec                            Spec
	Log                             *eventlog.Log
}
type Factory interface {
	Start(context.Context, StartRequest) (Adapter, error)
}

// EventBinder is implemented by adapters whose client services also produce
// normalized events (notably daemon-owned ACP terminals). Session binds those
// producers to its single append/fan-out sink immediately after construction.
type EventBinder interface {
	BindEventSink(func(eventlog.Event) (eventlog.LoggedEvent, error))
}
