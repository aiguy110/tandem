package runtimeinstall

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsureInstallsOnceAndWritesEmbeddedManifests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}
	root := filepath.Join(t.TempDir(), "runtime")
	bin := t.TempDir()
	count := filepath.Join(t.TempDir(), "npm-count")
	npm := filepath.Join(bin, "npm")
	script := `#!/bin/sh
set -eu
printf x >> "$NPM_COUNT"
mkdir -p node_modules/@agentclientprotocol/claude-agent-acp/dist
mkdir -p node_modules/@agentclientprotocol/codex-acp/dist
mkdir -p node_modules/pi-acp/dist
mkdir -p node_modules/@playwright/mcp
touch node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js
touch node_modules/@agentclientprotocol/codex-acp/dist/index.js
touch node_modules/pi-acp/dist/index.js
touch node_modules/@playwright/mcp/cli.js
`
	if err := os.WriteFile(npm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("NPM_COUNT", count)

	var log bytes.Buffer
	if err := Ensure(context.Background(), root, &log); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(context.Background(), root, &log); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "x" {
		t.Fatalf("npm invocation count = %q, want one invocation", got)
	}
	if !strings.Contains(log.String(), "provisioning Node runtime") {
		t.Fatalf("missing provisioning log: %q", log.String())
	}
	for _, name := range []string{"package.json", "package-lock.json", fingerprintFile} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

func TestEnsureRequiresNPMWhenRuntimeIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Ensure(context.Background(), t.TempDir(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "npm is required") {
		t.Fatalf("Ensure error = %v, want npm requirement", err)
	}
}
