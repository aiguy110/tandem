package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
)

func TestHarnessRateLimitParsers(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 30, 9, 0, 0, 0, ny)
	tests := []struct {
		name, message string
		parse         limitParser
		want          time.Time
	}{
		{"claude", "You've hit your monthly spend limit; your session limit resets 10:20am (America/New_York)", parseClaudeLimit, time.Date(2026, time.September, 30, 10, 20, 0, 0, ny)},
		{"codex dated", "You've hit your usage limit. You can use Codex again at Sep 30, 2026 2:03 PM (America/New_York)", parseCodexLimit, time.Date(2026, time.September, 30, 14, 3, 0, 0, ny)},
		{"duration", "Quota limit reached; try again in 2 hours", parseDurationLimit, now.Add(2 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.parse(tt.message, now)
			if !ok || !got.Equal(tt.want) {
				t.Fatalf("got %v, %v; want %v", got, ok, tt.want)
			}
		})
	}
	if _, ok := parseClaudeLimit("ordinary failure at 10:20am (America/New_York)", now); ok {
		t.Fatal("non-limit message matched")
	}
}

func TestRateLimitOptInSchedulesContinueAndPersistsState(t *testing.T) {
	s, adapter, _ := testSession(t)
	s.Spec.Harness = "claude"
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Now().In(ny).Add(time.Hour).Format("3:04pm")
	s.detectRateLimit("You've hit your monthly spend limit; your session limit resets " + reset + " (America/New_York)")

	s.rateLimitMu.Lock()
	s.rateLimit.ResetAt = time.Now().Add(20 * time.Millisecond).UnixMilli()
	s.rateLimitMu.Unlock()
	if err := s.SetRateLimitAutoContinue(true); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		adapter.mu.Lock()
		joined := strings.Join(adapter.prompts, ",")
		adapter.mu.Unlock()
		if joined == "continue" {
			adapter.gate <- struct{}{}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	adapter.mu.Lock()
	got := append([]string(nil), adapter.prompts...)
	adapter.mu.Unlock()
	if len(got) != 1 || got[0] != "continue" {
		t.Fatalf("prompts=%v", got)
	}

	latest, ok, err := s.Log.LatestOfKind("rate_limit")
	if err != nil || !ok {
		t.Fatalf("latest rate limit: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(string(latest.Event.Payload), `"state":"sent"`) {
		t.Fatalf("payload=%s", latest.Event.Payload)
	}
	if err := s.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitParserIsHarnessSpecific(t *testing.T) {
	s, _, _ := testSession(t)
	s.Spec = agentadapter.Spec{Harness: "gemini"}
	s.detectRateLimit("You've hit your monthly spend limit; your session limit resets 10:20am (America/New_York)")
	if _, ok, err := s.Log.LatestOfKind("rate_limit"); err != nil || ok {
		t.Fatalf("unexpected rate limit: ok=%v err=%v", ok, err)
	}
}
