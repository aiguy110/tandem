// Package runtimeinstall provisions the locked Node dependencies used by
// standalone Tandem binaries.
package runtimeinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	tandem "github.com/aiguy110/tandem"
)

const fingerprintFile = ".tandem-runtime-lock"

// Ensure installs the embedded, locked runtime when root is absent or stale.
func Ensure(ctx context.Context, root string, log io.Writer) error {
	sum := sha256.Sum256(tandem.RuntimePackageLock)
	fingerprint := hex.EncodeToString(sum[:])
	if current, err := os.ReadFile(filepath.Join(root, fingerprintFile)); err == nil &&
		string(bytes.TrimSpace(current)) == fingerprint &&
		requiredFilesExist(root) {
		return nil
	}

	npm, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("provision runtime: npm is required: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("provision runtime: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), tandem.RuntimePackageJSON, 0o644); err != nil {
		return fmt.Errorf("provision runtime package.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "package-lock.json"), tandem.RuntimePackageLock, 0o644); err != nil {
		return fmt.Errorf("provision runtime package-lock.json: %w", err)
	}

	fmt.Fprintf(log, "tandem: provisioning Node runtime in %s\n", root)
	cmd := exec.CommandContext(ctx, npm, "ci", "--omit=dev", "--no-audit", "--no-fund")
	cmd.Dir = root
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provision runtime with npm ci: %w", err)
	}
	if !requiredFilesExist(root) {
		return fmt.Errorf("provision runtime: npm completed without required ACP modules")
	}
	if err := os.WriteFile(filepath.Join(root, fingerprintFile), []byte(fingerprint+"\n"), 0o644); err != nil {
		return fmt.Errorf("record runtime fingerprint: %w", err)
	}
	return nil
}

func requiredFilesExist(root string) bool {
	required := []string{
		"node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js",
		"node_modules/@agentclientprotocol/codex-acp/dist/index.js",
		"node_modules/pi-acp/dist/index.js",
		"node_modules/@playwright/mcp/cli.js",
	}
	for _, name := range required {
		if info, err := os.Stat(filepath.Join(root, name)); err != nil || info.IsDir() {
			return false
		}
	}
	return true
}
