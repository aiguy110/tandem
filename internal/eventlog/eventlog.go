// Package eventlog provides Tandem's durable, replayable per-agent event stream.
package eventlog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/store"
)

// Event retains normalized JSON for ordinary events and decoded bytes for raw
// PTY events. JSON key order is retained so Go writes Node-compatible payloads.
type Event struct {
	Kind    string
	Payload json.RawMessage
	Data    []byte
}

type LoggedEvent struct {
	Seq   int64
	Event Event
	TS    int64
}

// ParseNormalized accepts the persisted/wire normalization used by Node. A
// raw_pty event's dataB64 is decoded at the daemon boundary.
func ParseNormalized(data []byte) (Event, error) {
	var header struct {
		Kind    string `json:"kind"`
		DataB64 string `json:"dataB64"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	if header.Kind == "" {
		return Event{}, errors.New("event kind is required")
	}
	if header.Kind == "raw_pty" {
		decoded, err := base64.StdEncoding.DecodeString(header.DataB64)
		if err != nil {
			return Event{}, fmt.Errorf("decode raw_pty dataB64: %w", err)
		}
		return RawPTY(decoded), nil
	}
	if header.Kind == "shell_pty" {
		decoded, err := base64.StdEncoding.DecodeString(header.DataB64)
		if err != nil {
			return Event{}, fmt.Errorf("decode shell_pty dataB64: %w", err)
		}
		return ShellPTY(decoded), nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return Event{}, fmt.Errorf("compact event: %w", err)
	}
	return Event{Kind: header.Kind, Payload: compact.Bytes()}, nil
}

func RawPTY(data []byte) Event {
	return Event{Kind: "raw_pty", Data: bytes.Clone(data)}
}

// ShellPTY carries bytes from the user's escape-hatch shell (docs/terminal.md:
// the Terminal tab). It is a binary stream like raw_pty but tagged distinctly so
// it never mixes into the agent transcript / agent-CLI pty view.
func ShellPTY(data []byte) Event {
	return Event{Kind: "shell_pty", Data: bytes.Clone(data)}
}

// ShellExit records that the user shell process exited so the UI can offer a
// restart. message is a short human string (e.g. "exited (code 0)").
func ShellExit(message string) Event {
	payload, _ := json.Marshal(struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}{"shell_exit", message})
	return Event{Kind: "shell_exit", Payload: payload}
}

// NormalizedJSON returns the JSON representation shared by SQLite and the
// browser wire protocol.
func (e Event) NormalizedJSON() ([]byte, error) {
	if e.Kind == "raw_pty" {
		return json.Marshal(struct {
			Kind    string `json:"kind"`
			DataB64 string `json:"dataB64"`
		}{"raw_pty", base64.StdEncoding.EncodeToString(e.Data)})
	}
	if e.Kind == "shell_pty" {
		return json.Marshal(struct {
			Kind    string `json:"kind"`
			DataB64 string `json:"dataB64"`
		}{"shell_pty", base64.StdEncoding.EncodeToString(e.Data)})
	}
	if e.Kind == "" || !json.Valid(e.Payload) {
		return nil, errors.New("event has invalid normalized JSON")
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(e.Payload, &header); err != nil || header.Kind != e.Kind {
		return nil, errors.New("event kind does not match payload")
	}
	return bytes.Clone(e.Payload), nil
}

type ReplaySource string

const (
	ReplayHot      ReplaySource = "hot"
	ReplayCold     ReplaySource = "cold"
	ReplaySnapshot ReplaySource = "snapshot"
)

type Replay struct {
	Source ReplaySource
	Events []LoggedEvent
}

// Log serializes append and ring mutation. Store allocation is independently
// serialized, protecting sequence uniqueness across multiple Log instances.
type Log struct {
	mu        sync.RWMutex
	sessionID string
	store     *store.Store
	cap       int
	ring      []LoggedEvent
	head      int64
	now       func() time.Time
}

func New(sessionID string, backing *store.Store, capacity int) (*Log, error) {
	if backing == nil {
		return nil, errors.New("event store is required")
	}
	if capacity < 0 {
		return nil, errors.New("ring capacity cannot be negative")
	}
	_, head, err := backing.EventBounds(sessionID)
	if err != nil {
		return nil, err
	}
	return &Log{sessionID: sessionID, store: backing, cap: capacity, head: head, now: time.Now}, nil
}

func (l *Log) Append(event Event) (LoggedEvent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	payload, err := event.NormalizedJSON()
	if err != nil {
		return LoggedEvent{}, err
	}
	ts := l.now().UnixMilli()
	seq, err := l.store.AppendEvent(l.sessionID, event.Kind, string(payload), ts)
	if err != nil {
		return LoggedEvent{}, err
	}
	logged := LoggedEvent{Seq: seq, Event: cloneEvent(event), TS: ts}
	// Another Log may have advanced the durable stream. The local head follows
	// the allocated value and the ring only claims a contiguous local suffix.
	if seq != l.head+1 {
		l.ring = nil
	}
	l.head = seq
	l.ring = append(l.ring, logged)
	if len(l.ring) > l.cap {
		l.ring = l.ring[len(l.ring)-l.cap:]
	}
	return logged, nil
}

func (l *Log) Head() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.head
}

func (l *Log) EarliestInRing() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.ring) == 0 {
		return l.head + 1
	}
	return l.ring[0].Seq
}

// ReplaySince returns a hot or cold gapless suffix. A fresh subscription,
// future checkpoint, or checkpoint older than retained SQLite history signals
// snapshot fallback and includes the complete retained history.
func (l *Log) ReplaySince(since int64) (Replay, error) {
	l.mu.RLock()
	head := l.head
	earliest := head + 1
	if len(l.ring) != 0 {
		earliest = l.ring[0].Seq
	}
	if since > 0 && since <= head && since >= earliest-1 {
		out := make([]LoggedEvent, 0, len(l.ring))
		for _, event := range l.ring {
			if event.Seq > since {
				out = append(out, cloneLogged(event))
			}
		}
		l.mu.RUnlock()
		return Replay{Source: ReplayHot, Events: coalesceStreamChunks(out)}, nil
	}
	l.mu.RUnlock()

	min, durableHead, err := l.store.EventBounds(l.sessionID)
	if err != nil {
		return Replay{}, err
	}
	snapshot := since <= 0 || since > durableHead || (min > 0 && since < min-1)
	after := since
	source := ReplayCold
	if snapshot {
		after = 0
		source = ReplaySnapshot
	}
	rows, err := l.store.RangeEvents(l.sessionID, after)
	if err != nil {
		return Replay{}, err
	}
	events, err := decodeRows(rows)
	if err != nil {
		return Replay{}, err
	}
	return Replay{Source: source, Events: coalesceStreamChunks(events)}, nil
}

// LatestOfKind returns the newest logged event of a kind. ok is false when the
// agent has never logged one. It reads through to SQLite so it stays correct
// after a daemon restart, when the in-memory ring is empty.
func (l *Log) LatestOfKind(kind string) (LoggedEvent, bool, error) {
	row, err := l.store.LatestEventOfKind(l.sessionID, kind)
	if err != nil || row == nil {
		return LoggedEvent{}, false, err
	}
	event, err := ParseNormalized([]byte(row.Payload))
	if err != nil {
		return LoggedEvent{}, false, fmt.Errorf("decode event seq %d: %w", row.Seq, err)
	}
	return LoggedEvent{Seq: row.Seq, Event: event, TS: row.TS}, true, nil
}

func (l *Log) FullHistory() ([]LoggedEvent, error) {
	rows, err := l.store.RangeEvents(l.sessionID, 0)
	if err != nil {
		return nil, err
	}
	events, err := decodeRows(rows)
	if err != nil {
		return nil, err
	}
	return coalesceStreamChunks(events), nil
}

// coalesceStreamChunks turns a persisted run of arbitrary ACP text fragments
// into one logical block for snapshots, reconnect replay, and history users.
// The final sequence is retained so a client's replay checkpoint covers every
// durable row consumed by the merged event.
func coalesceStreamChunks(events []LoggedEvent) []LoggedEvent {
	out := make([]LoggedEvent, 0, len(events))
	for _, current := range events {
		currentKey, currentText, currentOK := chunkParts(current.Event)
		if len(out) > 0 && currentOK {
			previousKey, _, previousOK := chunkParts(out[len(out)-1].Event)
			if previousOK && previousKey == currentKey {
				last := &out[len(out)-1]
				last.Event = appendChunkText(last.Event, currentText)
				last.Seq, last.TS = current.Seq, current.TS
				continue
			}
		}
		out = append(out, current)
	}
	return out
}

func chunkParts(event Event) (key, text string, ok bool) {
	if event.Kind != "message_chunk" && event.Kind != "thought_chunk" {
		return "", "", false
	}
	var payload struct {
		Text     string `json:"text"`
		ParentID string `json:"parentId"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return "", "", false
	}
	return event.Kind + "\x00" + payload.ParentID, payload.Text, true
}

func appendChunkText(event Event, suffix string) Event {
	var payload map[string]any
	if json.Unmarshal(event.Payload, &payload) != nil {
		return event
	}
	text, _ := payload["text"].(string)
	payload["text"] = text + suffix
	event.Payload, _ = json.Marshal(payload)
	return event
}

func decodeRows(rows []store.StoredEvent) ([]LoggedEvent, error) {
	out := make([]LoggedEvent, 0, len(rows))
	for _, row := range rows {
		event, err := ParseNormalized([]byte(row.Payload))
		if err != nil {
			return nil, fmt.Errorf("decode event seq %d: %w", row.Seq, err)
		}
		if event.Kind != row.Kind {
			return nil, fmt.Errorf("event seq %d kind %q does not match payload kind %q", row.Seq, row.Kind, event.Kind)
		}
		out = append(out, LoggedEvent{Seq: row.Seq, Event: event, TS: row.TS})
	}
	return out, nil
}

func cloneEvent(event Event) Event {
	event.Payload = bytes.Clone(event.Payload)
	event.Data = bytes.Clone(event.Data)
	return event
}

func cloneLogged(event LoggedEvent) LoggedEvent {
	event.Event = cloneEvent(event.Event)
	return event
}
