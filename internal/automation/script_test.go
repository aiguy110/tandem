package automation

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func makeRepoScript(t *testing.T, relative, source string) string {
	t.Helper()
	repo := t.TempDir()
	path := filepath.Join(repo, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestLoadScript(t *testing.T) {
	repo := makeRepoScript(t, ".tandem/scripts/check.ts", "/** @tandem\nname: check\n*/\n")
	script, err := LoadScript(repo, ".tandem/scripts/check.ts")
	if err != nil {
		t.Fatal(err)
	}
	if script.Path != ".tandem/scripts/check.ts" || script.Manifest.Name != "check" {
		t.Fatalf("unexpected script: %+v", script)
	}
}

func TestResolveScriptPathRejectsInvalidLocations(t *testing.T) {
	repo := makeRepoScript(t, ".tandem/scripts/good.ts", "")
	outside := filepath.Join(repo, "outside.ts")
	if err := os.WriteFile(outside, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []string{"outside.ts", ".tandem/scripts/../../outside.ts", ".tandem/scripts", ".tandem/scripts/good.js", outside}
	for _, path := range tests {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			if _, _, err := ResolveScriptPath(repo, path); err == nil {
				t.Fatalf("ResolveScriptPath(%q) unexpectedly succeeded", path)
			}
		})
	}
}

func TestResolveScriptPathRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevation")
	}
	repo := makeRepoScript(t, ".tandem/scripts/good.ts", "")
	outside := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".tandem/scripts/escape.ts")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveScriptPath(repo, ".tandem/scripts/escape.ts"); err == nil || !strings.Contains(err.Error(), "symlink escapes") {
		t.Fatalf("got %v, want symlink escape error", err)
	}
}

func TestResolveScriptPathAllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevation")
	}
	repo := makeRepoScript(t, ".tandem/scripts/real.ts", "")
	if err := os.Symlink("real.ts", filepath.Join(repo, ".tandem/scripts/link.ts")); err != nil {
		t.Fatal(err)
	}
	full, _, err := ResolveScriptPath(repo, ".tandem/scripts/link.ts")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(full) != "real.ts" {
		t.Fatalf("resolved to %q", full)
	}
}

func TestResolveScriptPathRejectsScriptsDirectorySymlinkOutsideRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevation")
	}
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".tandem"), 0o755); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "escape.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(repo, ".tandem/scripts")); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveScriptPath(repo, ".tandem/scripts/escape.ts")
	if err == nil || !strings.Contains(err.Error(), "escapes repository") {
		t.Fatalf("got %v, want scripts directory escape error", err)
	}
}
