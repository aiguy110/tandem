package runtimeinstall

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	tandem "github.com/aiguy110/tandem"
	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
)

func TestEnsureAgentUnknownAgentIsNoop(t *testing.T) {
	cfg := config.Config{RuntimeRoot: t.TempDir()}
	var log bytes.Buffer
	if err := EnsureAgent(context.Background(), cfg, "some-custom-agent", &log); err != nil {
		t.Fatalf("EnsureAgent for unknown agent = %v, want nil", err)
	}
	if log.Len() != 0 {
		t.Fatalf("expected no log output for unknown agent, got %q", log.String())
	}
}

func TestEnsureHistoryStagesEmbeddedRuntimeAndUsesExistingTSX(t *testing.T) {
	root := t.TempDir()
	distPath := filepath.Join(root, historyPin.dist)
	if err := os.MkdirAll(filepath.Dir(distPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(distPath, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	cfg := config.Config{RuntimeRoot: root}
	var log bytes.Buffer
	if err := EnsureHistory(context.Background(), cfg, &log); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string][]byte{
		filepath.Join("history", "sdk.ts"):                   tandem.RuntimeHistorySDK,
		filepath.Join("history", "runner.ts"):                tandem.RuntimeHistoryRunner,
		filepath.Join("history", "importers", "common.ts"):   tandem.RuntimeHistoryImporterCommon,
		filepath.Join("history", "importers", "claude.ts"):   tandem.RuntimeHistoryImporterClaude,
		filepath.Join("history", "importers", "codex.ts"):    tandem.RuntimeHistoryImporterCodex,
		filepath.Join("history", "importers", "pi.ts"):       tandem.RuntimeHistoryImporterPi,
		filepath.Join("history", "importers", "opencode.ts"): tandem.RuntimeHistoryImporterOpenCode,
		"tsconfig.json": tandem.RuntimeTSConfig,
	} {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s did not match embedded source", rel)
		}
	}
	if log.Len() != 0 {
		t.Fatalf("unexpected install log: %q", log.String())
	}
}

func TestEnsureAgentFastPathSkipsInstallWhenAlreadyPresent(t *testing.T) {
	root := t.TempDir()
	distRel := agentPins["claude"].dist
	distPath := filepath.Join(root, distRel)
	if err := os.MkdirAll(filepath.Dir(distPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(distPath, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	// PATH is empty so any attempt to shell out to npm would fail loudly,
	// proving the fast path never calls it.
	t.Setenv("PATH", "")
	cfg := config.Config{RuntimeRoot: root, Browser: config.BrowserConfig{MCPEnabled: false}}
	var log bytes.Buffer
	if err := EnsureAgent(context.Background(), cfg, "claude", &log); err != nil {
		t.Fatalf("EnsureAgent fast path = %v, want nil", err)
	}
	if log.Len() != 0 {
		t.Fatalf("expected no log output on the fast path, got %q", log.String())
	}
}

func TestLockfileRoundTripAndEntryPoint(t *testing.T) {
	root := t.TempDir()
	want := Lockfile{Version: 1, Agents: map[string]LockedAgent{"codex": {Package: "@agentclientprotocol/codex-acp", Version: "1.9.0", Path: filepath.Join(root, "agents", "codex", "1.9.0")}}}
	if err := writeLock(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agents["codex"].Version != "1.9.0" {
		t.Fatalf("lock=%#v", got)
	}
	entry, ok := EntryPoint("codex", &agentadapter.Distribution{Path: got.Agents["codex"].Path})
	if !ok || entry != filepath.Join(got.Agents["codex"].Path, agentPins["codex"].dist) {
		t.Fatalf("entry=%q ok=%v", entry, ok)
	}
}

func TestStageAgentSupportWritesEmbeddedFilesIdempotently(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{RuntimeRoot: root}
	if err := StageAgentSupport(cfg); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "pi", "tandem-pi")
	for path, want := range map[string][]byte{
		filepath.Join(root, "pi", "mcp-bridge.ts"): tandem.RuntimePiMCPBridge,
		wrapper: tandem.RuntimePiWrapper,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s did not match embedded source", path)
		}
	}
	info, err := os.Stat(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("wrapper mode = %v, want 0755", info.Mode().Perm())
	}
	// A stale copy is replaced; an up-to-date one is left alone.
	if err := os.WriteFile(wrapper, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageAgentSupport(cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(wrapper); !bytes.Equal(got, tandem.RuntimePiWrapper) {
		t.Fatal("stale wrapper was not replaced")
	}
	if info, _ := os.Stat(wrapper); info.Mode().Perm() != 0o755 {
		t.Fatalf("replaced wrapper mode = %v, want 0755", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Join(root, "pi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("pi support dir has leftover temp files: %v", entries)
	}
}
