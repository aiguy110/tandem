package automation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Script struct {
	Path     string
	FullPath string
	Source   []byte
	Manifest Manifest
}

// LoadScript resolves and reads a TypeScript file below .tandem/scripts. Both
// lexical traversal and symlinks escaping that directory are rejected.
func LoadScript(repoRoot, requestedPath string) (Script, error) {
	full, relative, err := ResolveScriptPath(repoRoot, requestedPath)
	if err != nil {
		return Script{}, err
	}
	source, err := os.ReadFile(full)
	if err != nil {
		return Script{}, fmt.Errorf("read automation script: %w", err)
	}
	manifest, err := ParseManifest(source)
	if err != nil {
		return Script{}, fmt.Errorf("%s: %w", relative, err)
	}
	return Script{Path: filepath.ToSlash(relative), FullPath: full, Source: source, Manifest: manifest}, nil
}

func ResolveScriptPath(repoRoot, requestedPath string) (fullPath, relativePath string, err error) {
	if strings.TrimSpace(repoRoot) == "" {
		return "", "", fmt.Errorf("repository root is required")
	}
	if requestedPath == "" || filepath.IsAbs(requestedPath) {
		return "", "", fmt.Errorf("script path must be repository-relative")
	}
	clean := filepath.Clean(filepath.FromSlash(requestedPath))
	prefix := filepath.Join(".tandem", "scripts")
	inside, err := pathWithin(prefix, clean)
	if err != nil || !inside || clean == prefix {
		return "", "", fmt.Errorf("script must be below .tandem/scripts")
	}
	if ext := strings.ToLower(filepath.Ext(clean)); ext != ".ts" && ext != ".mts" && ext != ".cts" {
		return "", "", fmt.Errorf("automation script must be a TypeScript file")
	}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve repository root: %w", err)
	}
	realScripts, err := filepath.EvalSymlinks(filepath.Join(realRoot, prefix))
	if err != nil {
		return "", "", fmt.Errorf("resolve .tandem/scripts: %w", err)
	}
	insideRepo, err := pathWithin(realRoot, realScripts)
	if err != nil || !insideRepo || realScripts == realRoot {
		return "", "", fmt.Errorf(".tandem/scripts symlink escapes repository")
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(realRoot, clean))
	if err != nil {
		return "", "", fmt.Errorf("resolve automation script: %w", err)
	}
	inside, err = pathWithin(realScripts, candidate)
	if err != nil || !inside || candidate == realScripts {
		return "", "", fmt.Errorf("script symlink escapes .tandem/scripts")
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", "", fmt.Errorf("stat automation script: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("automation script is not a regular file")
	}
	return candidate, clean, nil
}

func pathWithin(parent, child string) (bool, error) {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel), nil
}
