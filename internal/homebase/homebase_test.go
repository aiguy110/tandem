package homebase

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
)

func cfgFor(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	return config.Config{Home: home, HomeBaseDir: filepath.Join(home, "home-base")}
}

func TestEnsureScaffoldsRepo(t *testing.T) {
	cfg := cfgFor(t)
	if err := Ensure(context.Background(), cfg, io.Discard); err != nil {
		t.Fatal(err)
	}

	agents, err := os.ReadFile(filepath.Join(cfg.HomeBaseDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md missing: %v", err)
	}
	text := string(agents)
	for _, want := range []string{genBegin, genEnd, "Tandem home base", ".tandem/scripts", "Self-evolving skills", authoredStarter} {
		if !strings.Contains(text, want) {
			t.Fatalf("AGENTS.md missing %q", want)
		}
	}

	// CLAUDE.md resolves to AGENTS.md (symlink or pointer).
	claude := filepath.Join(cfg.HomeBaseDir, "CLAUDE.md")
	if _, err := os.Lstat(claude); err != nil {
		t.Fatalf("CLAUDE.md missing: %v", err)
	}
	if target, err := os.Readlink(claude); err == nil && target != "AGENTS.md" {
		t.Fatalf("CLAUDE.md symlink = %q, want AGENTS.md", target)
	}

	for _, rel := range []string{filepath.Join("skills", "README.md"), filepath.Join(".tandem", "scripts", "README.md")} {
		if _, err := os.Stat(filepath.Join(cfg.HomeBaseDir, rel)); err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
	}

	if _, err := os.Stat(filepath.Join(cfg.HomeBaseDir, ".git")); err != nil {
		t.Fatalf("expected a git repo: %v", err)
	}
}

func TestEnsureIsIdempotentAndPreservesAuthored(t *testing.T) {
	cfg := cfgFor(t)
	ctx := context.Background()
	if err := Ensure(ctx, cfg, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Simulate a human/agent editing the authored region and a skill file.
	agentsPath := filepath.Join(cfg.HomeBaseDir, "AGENTS.md")
	orig, _ := os.ReadFile(agentsPath)
	authored := string(orig) + "\n\n## My custom section\nhand-written\n"
	if err := os.WriteFile(agentsPath, []byte(authored), 0o644); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(cfg.HomeBaseDir, "skills", "my-skill.md")
	if err := os.WriteFile(skillPath, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A second Ensure (e.g. next daemon boot) must not clobber authored content.
	if err := Ensure(ctx, cfg, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(agentsPath)
	if !strings.Contains(string(after), "## My custom section") {
		t.Fatalf("authored section was clobbered")
	}
	if b, _ := os.ReadFile(skillPath); string(b) != "mine" {
		t.Fatalf("authored skill file was overwritten")
	}
}

func TestRegeneratesGeneratedBlockOnly(t *testing.T) {
	cfg := cfgFor(t)
	ctx := context.Background()
	if err := Ensure(ctx, cfg, io.Discard); err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(cfg.HomeBaseDir, "AGENTS.md")

	// Corrupt the generated block; a later Ensure should restore it while
	// leaving the authored starter intact.
	text, _ := os.ReadFile(agentsPath)
	broken := strings.Replace(string(text), "Tandem home base", "TAMPERED", 1)
	if err := os.WriteFile(agentsPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(ctx, cfg, io.Discard); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(agentsPath)
	if strings.Contains(string(restored), "TAMPERED") {
		t.Fatalf("generated block was not regenerated")
	}
	if !strings.Contains(string(restored), authoredStarter) {
		t.Fatalf("authored starter lost during regeneration")
	}
}

func TestLocalDocsLinkedWhenSourcePresent(t *testing.T) {
	cfg := cfgFor(t)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.TandemRoot = src

	block := renderGenerated(cfg)
	if !strings.Contains(block, filepath.Join(src, "docs")) {
		t.Fatalf("expected local docs path in generated block:\n%s", block)
	}
	if !strings.Contains(block, sourceRepoURL) {
		t.Fatalf("expected upstream URL in generated block")
	}
}

func TestEmptyHomeBaseDirIsNoop(t *testing.T) {
	if err := Ensure(context.Background(), config.Config{}, io.Discard); err != nil {
		t.Fatalf("empty HomeBaseDir should be a no-op, got %v", err)
	}
}
