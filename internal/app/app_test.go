package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/buildinfo"
	"github.com/aiguy110/tandem/internal/daemon"
)

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

func TestDaemonCommandIsExplicitlyUnavailable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"daemon"}, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(daemon) exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), daemon.ErrNotReady.Error()) {
		t.Fatalf("Run(daemon) stderr = %q, want it to contain %q", stderr.String(), daemon.ErrNotReady)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run(daemon) stdout = %q, want empty", stdout.String())
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
