package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/buildinfo"
)

func TestDebugConfigCommand(t *testing.T) {
	t.Setenv("TANDEM_HOME", t.TempDir())
	t.Setenv("TANDEM_NODE_CMD", "node")
	t.Setenv("STEEL_API_KEY", "must-not-appear")
	t.Setenv("STEEL_SESSION_OPTIONS", "{}")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"debug", "config"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(debug config) exit code = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "must-not-appear") || !strings.Contains(stdout.String(), `"steelApiKey": "[REDACTED]"`) {
		t.Fatalf("debug output was not redacted: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("TANDEM_HOME"))); err != nil {
		t.Fatalf("TANDEM_HOME not created: %v", err)
	}
}

func TestVersionCommand(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = oldVersion, oldCommit, oldBuildTime
	})
	buildinfo.Version = "v1.2.3"
	buildinfo.Commit = "abc123"
	buildinfo.BuildTime = "2026-07-16T12:00:00Z"

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(version) exit code = %d, want 0", code)
	}
	if got, want := stdout.String(), "tandem version=v1.2.3 commit=abc123 built=2026-07-16T12:00:00Z\n"; got != want {
		t.Fatalf("Run(version) stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run(version) stderr = %q, want empty", stderr.String())
	}
}

func TestUpdateCommandRejectsDevelopmentBuild(t *testing.T) {
	oldVersion := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	buildinfo.Version = "dev"

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"update"}, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(update) exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "development builds") {
		t.Fatalf("Run(update) stderr = %q", stderr.String())
	}
}

func TestBareCommandReportsConfigurationFailure(t *testing.T) {
	t.Setenv("TANDEM_HOME", t.TempDir())
	t.Setenv("TANDEM_PORT", "not-a-port")
	var stdout, stderr bytes.Buffer
	if code := Run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(nil) exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "TANDEM_PORT") {
		t.Fatalf("Run(nil) stderr = %q, want configuration error", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run(nil) stdout = %q, want empty", stdout.String())
	}
}

func TestSetupCommandRequiresTerminal(t *testing.T) {
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"setup"}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run(setup) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "requires an interactive terminal") {
		t.Fatalf("Run(setup) stderr = %q", stderr.String())
	}
}

func TestDaemonCommandWasRemoved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"daemon"}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run(daemon) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("Run(daemon) stderr = %q", stderr.String())
	}
}

func TestInvalidCommandShowsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run(nope) exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), usage) {
		t.Fatalf("Run(nope) stderr = %q, want usage", stderr.String())
	}
}
