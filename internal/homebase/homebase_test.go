package homebase

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
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
	for _, want := range []string{genBegin, genEnd, "Tandem home base", ".tandem/scripts", "Self-evolving skills", "Codex requires a current build", "project is trusted", authoredStarter} {
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

	for _, rel := range []string{
		filepath.Join("skills", "README.md"),
		filepath.Join(".tandem", "scripts", "README.md"),
		"opencode.json",
		filepath.Join(".pi", "settings.json"),
	} {
		if _, err := os.Stat(filepath.Join(cfg.HomeBaseDir, rel)); err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
	}
	for _, rel := range []string{filepath.Join(".claude", "skills"), filepath.Join(".codex", "skills")} {
		target, err := os.Readlink(filepath.Join(cfg.HomeBaseDir, rel))
		if err != nil {
			t.Fatalf("%s is not a symlink: %v", rel, err)
		}
		if target != "../skills" {
			t.Fatalf("%s symlink = %q, want ../skills", rel, target)
		}
	}
	for rel, want := range map[string]string{
		"opencode.json":                       "{\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"skills\": { \"paths\": [\"./skills\"] }\n}\n",
		filepath.Join(".pi", "settings.json"): "{ \"skills\": [\"../skills\"] }\n",
	} {
		got, err := os.ReadFile(filepath.Join(cfg.HomeBaseDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}

	if _, err := os.Stat(filepath.Join(cfg.HomeBaseDir, ".git")); err != nil {
		t.Fatalf("expected a git repo: %v", err)
	}
	gitFiles, err := exec.Command("git", "-C", cfg.HomeBaseDir, "ls-files", "-s").Output()
	if err != nil {
		t.Fatalf("git ls-files -s: %v", err)
	}
	for _, rel := range []string{".claude/skills", ".codex/skills"} {
		found := false
		for _, line := range strings.Split(string(gitFiles), "\n") {
			if strings.HasPrefix(line, "120000 ") && strings.HasSuffix(line, "\t"+rel) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("git index does not contain symlink %s (mode 120000):\n%s", rel, gitFiles)
		}
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
	for rel, body := range map[string]string{
		"opencode.json":                       "{\"custom\": true}\n",
		filepath.Join(".pi", "settings.json"): "{\"custom\": true}\n",
	} {
		if err := os.WriteFile(filepath.Join(cfg.HomeBaseDir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A second Ensure (e.g. next daemon boot) must not clobber authored content.
	var out bytes.Buffer
	if err := Ensure(ctx, cfg, &out); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(agentsPath)
	if !strings.Contains(string(after), "## My custom section") {
		t.Fatalf("authored section was clobbered")
	}
	if b, _ := os.ReadFile(skillPath); string(b) != "mine" {
		t.Fatalf("authored skill file was overwritten")
	}
	for _, rel := range []string{"opencode.json", filepath.Join(".pi", "settings.json")} {
		b, err := os.ReadFile(filepath.Join(cfg.HomeBaseDir, rel))
		if err != nil || !strings.Contains(string(b), "custom") {
			t.Fatalf("custom %s was overwritten: %q (%v)", rel, b, err)
		}
		if !strings.Contains(out.String(), rel+" already exists; leaving it unchanged") {
			t.Fatalf("expected warning about custom %s, got %q", rel, out.String())
		}
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
