package automation

import (
	"errors"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	source := []byte(`
/**
 * @tandem
 * name: inbox watcher
 * description: Finds interesting mail.
 * tools:
 *   - name: playwright.browser_find
 *     reason: Search the inbox
 * browser:
 *   snapshot: signed-in-mail
 * schedule:
 *   cron: "every 5m"
 *   timezone: America/New_York
 *   concurrency: skip
 * wake:
 *   agentProfile: inbox-triage
 *   prompt: Review the matching message.
 */
import { report } from "tandem:runtime";
`)
	m, err := ParseManifest(source)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "inbox watcher" || len(m.Tools) != 1 || m.Tools[0].Reason != "Search the inbox" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if m.Browser == nil || m.Browser.Snapshot != "signed-in-mail" {
		t.Fatalf("unexpected browser: %+v", m.Browser)
	}
	if m.Schedule == nil || m.Schedule.Cron != "every 5m" || m.Schedule.Concurrency != "skip" {
		t.Fatalf("unexpected schedule: %+v", m.Schedule)
	}
	if m.Wake == nil || m.Wake.AgentProfile != "inbox-triage" {
		t.Fatalf("unexpected wake: %+v", m.Wake)
	}
}

func TestParseManifestRequiresLeadingComment(t *testing.T) {
	_, err := ParseManifest([]byte(`console.log("before"); /** @tandem
name: late
*/`))
	if !errors.Is(err, ErrNoFrontmatter) {
		t.Fatalf("got %v, want ErrNoFrontmatter", err)
	}
}

func TestParseManifestRejectsUnknownAndInvalidFields(t *testing.T) {
	tests := []struct {
		name, yaml, contains string
	}{
		{"unknown", "name: x\npermissions: []", "field permissions not found"},
		{"missing name", "description: x", "name is required"},
		{"missing reason", "name: x\ntools:\n  - name: browser.find", "reason is required"},
		{"duplicate tool", "name: x\ntools:\n  - name: a\n    reason: one\n  - name: a\n    reason: two", "listed more than once"},
		{"empty snapshot", "name: x\nbrowser: {}", "browser.snapshot is required"},
		{"missing cron", "name: x\nschedule:\n  timezone: UTC", "schedule.cron is required"},
		{"bad concurrency", "name: x\nschedule:\n  cron: daily\n  concurrency: replace", "must be skip or queue"},
		{"missing agent", "name: x\nwake:\n  prompt: help", "wake.agentProfile is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := []byte("/**\n * @tandem\n" + tt.yaml + "\n */")
			_, err := ParseManifest(source)
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("got %v, want error containing %q", err, tt.contains)
			}
		})
	}
}
