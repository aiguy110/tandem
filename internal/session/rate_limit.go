package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
)

// rateLimitState is persisted as a succession of rate_limit events. The last
// event is authoritative, which makes the opt-in and timer reconstructable on
// reconnect and daemon restart without another state store.
type rateLimitState struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Harness    string `json:"harness"`
	ResetAt    int64  `json:"resetAt"`
	Enabled    bool   `json:"enabled"`
	State      string `json:"state"` // pending, sent, failed
	DetectedAt int64  `json:"detectedAt"`
	Subagents  bool   `json:"subagents,omitempty"`
	Error      string `json:"error,omitempty"`
}

type limitParser func(string, time.Time) (time.Time, bool)

const rateLimitTextWindow = 8 * 1024

var (
	claudeReset   = regexp.MustCompile(`(?i)(?:session limit |usage limit )?resets?\s+(?:at\s+)?([0-9]{1,2}(?::[0-9]{2})?\s*(?:am|pm))\s*\(([^)]+)\)`)
	codexReset    = regexp.MustCompile(`(?i)(?:use codex again|try again|limit resets?)\s+(?:at|after)\s+((?:[A-Z][a-z]{2}\s+[0-9]{1,2},?\s+[0-9]{4}\s+)?[0-9]{1,2}:[0-9]{2}\s*(?:am|pm))(?:\s*\(([^)]+)\))?`)
	durationReset = regexp.MustCompile(`(?i)(?:resets?|try again)\s+(?:in|after)\s+([0-9]+)\s*(seconds?|minutes?|hours?)`)
)

var harnessLimitParsers = map[string][]limitParser{
	"claude": {parseClaudeLimit, parseDurationLimit},
	"codex":  {parseCodexLimit, parseDurationLimit},
	"gemini": {parseDurationLimit},
	"pi":     {parseDurationLimit},
}

func (s *Session) observeRateLimitEvent(ev eventlog.Event) {
	s.observeRateLimitEventAt(ev, time.Now())
}

func (s *Session) observeRateLimitEventAt(ev eventlog.Event, observedAt time.Time) {
	var attribution struct {
		ParentID string `json:"parentId"`
	}
	_ = json.Unmarshal(ev.Payload, &attribution)

	switch ev.Kind {
	case "user_message":
		s.rateLimitMu.Lock()
		s.rateLimitText = ""
		s.rateLimitSawSubagent = false
		s.rateLimitMu.Unlock()
	case "message_chunk":
		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Payload, &payload) != nil || payload.Text == "" {
			return
		}
		s.rateLimitMu.Lock()
		if attribution.ParentID != "" {
			s.rateLimitSawSubagent = true
		}
		s.rateLimitText += payload.Text
		if len(s.rateLimitText) > rateLimitTextWindow {
			s.rateLimitText = s.rateLimitText[len(s.rateLimitText)-rateLimitTextWindow:]
		}
		text := s.rateLimitText
		s.rateLimitMu.Unlock()
		s.detectRateLimitAt(text, observedAt)
	default:
		if attribution.ParentID != "" {
			s.rateLimitMu.Lock()
			s.rateLimitSawSubagent = true
			s.rateLimitMu.Unlock()
		}
	}
}

func (s *Session) detectRateLimit(message string) {
	s.detectRateLimitAt(message, time.Now())
}

func (s *Session) detectRateLimitAt(message string, now time.Time) {
	harness := strings.ToLower(strings.TrimSpace(s.Spec.Harness))
	parsers := harnessLimitParsers[harness]
	if len(parsers) == 0 {
		harness = strings.ToLower(strings.TrimSpace(s.Spec.Agent))
		parsers = harnessLimitParsers[harness]
	}
	if len(parsers) == 0 {
		return
	}
	for _, parse := range parsers {
		reset, ok := parse(message, now)
		if !ok {
			continue
		}
		s.rateLimitMu.Lock()
		state := rateLimitState{Kind: "rate_limit", ID: fmt.Sprintf("%s-%d", harness, reset.UnixMilli()), Harness: harness, ResetAt: reset.UnixMilli(), State: "pending", DetectedAt: now.UnixMilli(), Subagents: s.rateLimitSawSubagent}
		if s.rateLimit.ID == state.ID && s.rateLimit.State == "pending" {
			s.rateLimitMu.Unlock()
			return
		}
		s.rateLimit = state
		s.stopRateLimitTimerLocked()
		s.rateLimitMu.Unlock()
		s.emitRateLimit(state)
		slog.Info("agent rate limit detected", "session_id", s.ID, "harness", harness, "reset_at", reset, "auto_continue", false)
		return
	}
}

// SetRateLimitAutoContinue changes daemon-owned state for the latest pending
// widget. Enabling schedules the continuation; disabling cancels its timer.
func (s *Session) SetRateLimitAutoContinue(enabled bool) error {
	s.rateLimitMu.Lock()
	state := s.rateLimit
	if state.ID == "" || state.State != "pending" {
		s.rateLimitMu.Unlock()
		return fmt.Errorf("no pending rate limit")
	}
	state.Enabled = enabled
	s.rateLimit = state
	s.stopRateLimitTimerLocked()
	if enabled {
		s.scheduleRateLimitLocked(state)
	}
	s.rateLimitMu.Unlock()
	s.emitRateLimit(state)
	slog.Info("agent rate-limit auto-continue changed", "session_id", s.ID, "limit_id", state.ID, "enabled", enabled, "reset_at_ms", state.ResetAt)
	return nil
}

func (s *Session) restoreRateLimit() {
	logged, ok, err := s.Log.LatestOfKind("rate_limit")
	if err != nil {
		slog.Warn("restore agent rate-limit state failed", "session_id", s.ID, "error", err)
		return
	}
	if !ok {
		s.recoverRecentRateLimit()
		return
	}
	if json.Unmarshal(logged.Event.Payload, &s.rateLimit) != nil || s.rateLimit.ID == "" {
		return
	}
	if s.rateLimit.Enabled && s.rateLimit.State == "pending" {
		s.rateLimitMu.Lock()
		s.scheduleRateLimitLocked(s.rateLimit)
		s.rateLimitMu.Unlock()
		slog.Info("restored agent rate-limit auto-continue", "session_id", s.ID, "limit_id", s.rateLimit.ID, "reset_at_ms", s.rateLimit.ResetAt)
	}
}

// recoverRecentRateLimit repairs sessions whose transcript contains a quota
// message written before structured detection supported that message shape.
// The bounded window avoids manufacturing incidents from stale session history.
func (s *Session) recoverRecentRateLimit() {
	replay, err := s.Log.ReplaySince(0)
	if err != nil {
		slog.Warn("recover agent rate-limit transcript failed", "session_id", s.ID, "error", err)
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, logged := range replay.Events {
		observedAt := time.UnixMilli(logged.TS)
		if observedAt.Before(cutoff) {
			continue
		}
		s.observeRateLimitEventAt(logged.Event, observedAt)
	}
	s.rateLimitMu.Lock()
	recovered := s.rateLimit.ID != ""
	limitID := s.rateLimit.ID
	resetAt := s.rateLimit.ResetAt
	s.rateLimitMu.Unlock()
	if recovered {
		slog.Info("recovered agent rate limit from transcript", "session_id", s.ID, "limit_id", limitID, "reset_at_ms", resetAt)
	}
}

func (s *Session) scheduleRateLimitLocked(state rateLimitState) {
	delay := time.Until(time.UnixMilli(state.ResetAt))
	if delay < 0 {
		delay = 0
	}
	s.rateLimitTimer = time.AfterFunc(delay, func() { s.fireRateLimit(state.ID) })
}

func (s *Session) stopRateLimitTimerLocked() {
	if s.rateLimitTimer != nil {
		s.rateLimitTimer.Stop()
		s.rateLimitTimer = nil
	}
}

func (s *Session) fireRateLimit(id string) {
	s.rateLimitMu.Lock()
	if s.rateLimit.ID != id || !s.rateLimit.Enabled || s.rateLimit.State != "pending" {
		s.rateLimitMu.Unlock()
		return
	}
	state := s.rateLimit
	state.State = "sent"
	state.Enabled = false
	s.rateLimit = state
	s.rateLimitTimer = nil
	s.rateLimitMu.Unlock()

	prompt := "continue"
	if state.Subagents {
		prompt = "Continue. Some sub-agents may have been interrupted by the usage limit; resume them if needed."
	}
	_, err := s.EnqueuePrompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: prompt}})
	if err != nil {
		state.State = "failed"
		state.Error = err.Error()
		s.rateLimitMu.Lock()
		if s.rateLimit.ID == id {
			s.rateLimit = state
		}
		s.rateLimitMu.Unlock()
		slog.Warn("agent rate-limit auto-continue failed", "session_id", s.ID, "limit_id", id, "error", err)
	} else {
		slog.Info("agent rate-limit auto-continue sent", "session_id", s.ID, "limit_id", id)
	}
	s.emitRateLimit(state)
}

func (s *Session) emitRateLimit(state rateLimitState) {
	payload, _ := json.Marshal(state)
	s.emit(eventlog.Event{Kind: "rate_limit", Payload: payload})
}

func parseClaudeLimit(message string, now time.Time) (time.Time, bool) {
	m := claudeReset.FindStringSubmatch(message)
	if m == nil || !strings.Contains(strings.ToLower(message), "limit") {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(strings.TrimSpace(m[2]))
	if err != nil {
		return time.Time{}, false
	}
	clockText := strings.ToLower(strings.ReplaceAll(m[1], " ", ""))
	var clock time.Time
	for _, layout := range []string{"3:04pm", "3pm"} {
		clock, err = time.ParseInLocation(layout, clockText, loc)
		if err == nil {
			break
		}
	}
	if err != nil {
		return time.Time{}, false
	}
	localNow := now.In(loc)
	reset := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
	if !reset.After(localNow) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset, true
}

func parseCodexLimit(message string, now time.Time) (time.Time, bool) {
	if !strings.Contains(strings.ToLower(message), "limit") {
		return time.Time{}, false
	}
	m := codexReset.FindStringSubmatch(message)
	if m == nil {
		return time.Time{}, false
	}
	loc := now.Location()
	if m[2] != "" {
		var err error
		loc, err = time.LoadLocation(strings.TrimSpace(m[2]))
		if err != nil {
			return time.Time{}, false
		}
	}
	value := strings.TrimSpace(m[1])
	for _, layout := range []string{"Jan 2, 2006 3:04 PM", "Jan 2 2006 3:04 PM"} {
		if reset, err := time.ParseInLocation(layout, value, loc); err == nil {
			return reset, reset.After(now)
		}
	}
	clock, err := time.ParseInLocation("3:04 PM", strings.ToUpper(value), loc)
	if err != nil {
		return time.Time{}, false
	}
	localNow := now.In(loc)
	reset := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
	if !reset.After(localNow) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset, true
}

func parseDurationLimit(message string, now time.Time) (time.Time, bool) {
	if !strings.Contains(strings.ToLower(message), "limit") && !strings.Contains(strings.ToLower(message), "quota") {
		return time.Time{}, false
	}
	m := durationReset.FindStringSubmatch(message)
	if m == nil {
		return time.Time{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	unit := time.Second
	if strings.HasPrefix(strings.ToLower(m[2]), "minute") {
		unit = time.Minute
	}
	if strings.HasPrefix(strings.ToLower(m[2]), "hour") {
		unit = time.Hour
	}
	return now.Add(time.Duration(n) * unit), true
}
