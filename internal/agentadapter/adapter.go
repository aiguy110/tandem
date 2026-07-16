// Package agentadapter defines the normalized lifecycle seam owned by sessions.
package agentadapter

import (
	"context"
	"encoding/json"

	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/workspace"
)

type Capabilities struct {
	Structured, Terminals, LoadSession, FS, Image bool
}

type PromptBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	AssetID  string `json:"assetId,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
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
	SessionID() string
	PID() int
}

type Launch struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env,omitempty"`
}
type TerminalLaunch struct {
	Cmd        string            `json:"cmd"`
	StartArgs  []string          `json:"startArgs,omitempty"`
	ResumeArgs []string          `json:"resumeArgs,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}
type ResolvedLaunch struct {
	Agent    string          `json:"agent"`
	ACP      *Launch         `json:"acp,omitempty"`
	Terminal *TerminalLaunch `json:"terminal,omitempty"`
}
type Spec struct {
	Adapter        string              `json:"adapter"`
	Agent          string              `json:"agent,omitempty"`
	Profile        string              `json:"profile,omitempty"`
	ACPArgs        []string            `json:"acpArgs,omitempty"`
	TerminalArgs   []string            `json:"terminalArgs,omitempty"`
	Workspace      workspace.Workspace `json:"workspace"`
	Name           string              `json:"name,omitempty"`
	Task           string              `json:"task,omitempty"`
	SessionConfig  json.RawMessage     `json:"sessionConfig,omitempty"`
	Preset         string              `json:"preset,omitempty"`
	ResolvedLaunch *ResolvedLaunch     `json:"resolvedLaunch,omitempty"`
}

type StartRequest struct {
	AgentID, CWD, ResumeSessionID string
	CaptureReplay                 bool
	Spec                          Spec
	Log                           *eventlog.Log
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
