package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyTreeSkipsLocksAndNonRegular(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")
	// A nested regular file, a singleton lock, and a subdir.
	if err := os.MkdirAll(filepath.Join(src, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "Default", "Cookies"), "cookie-data")
	writeFile(t, filepath.Join(src, "SingletonLock"), "pid")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dst, "Default", "Cookies")); got != "cookie-data" {
		t.Fatalf("Cookies = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "SingletonLock")); !os.IsNotExist(err) {
		t.Fatalf("SingletonLock should be skipped, err=%v", err)
	}
}

// A local driver whose ProfileDir points at a pre-populated dir, provisioned
// only far enough to satisfy CaptureSnapshot's liveness check.
func TestLocalDriverSeedThenCapture(t *testing.T) {
	root := t.TempDir()
	d := NewLocalDriver(LocalConfig{UserDataRoot: root})
	// Pretend agent "a" is provisioned with some state.
	agentDir := d.ProfileDir("a")
	if err := os.MkdirAll(filepath.Join(agentDir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(agentDir, "Default", "LocalStorage"), "session=1")
	d.handles["a"] = &localHandle{profile: agentDir} // mark provisioned for the broker check

	b := NewBroker(d, BrokerConfig{})
	snapDir := filepath.Join(root, "snap-1")
	kind, ref, err := b.CaptureSnapshot(nil, "a", snapDir)
	if err != nil || kind != "local" || ref != snapDir {
		t.Fatalf("CaptureSnapshot = %q,%q,%v", kind, ref, err)
	}
	if got := readFile(t, filepath.Join(snapDir, "Default", "LocalStorage")); got != "session=1" {
		t.Fatalf("captured LocalStorage = %q", got)
	}

	// Seeding a fresh agent "b" copies the snapshot into its (empty) profile dir
	// on first provision. We exercise the copy path directly rather than launch
	// Chromium: seed, then verify the seed dir is recorded and copies on demand.
	d.SeedProfile("b", snapDir)
	if d.seeds["b"] != snapDir {
		t.Fatalf("seed not recorded: %v", d.seeds)
	}
	bDir := d.ProfileDir("b")
	if err := os.MkdirAll(bDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(bDir); len(entries) == 0 {
		if err := copyTree(d.seeds["b"], bDir); err != nil {
			t.Fatal(err)
		}
	}
	if got := readFile(t, filepath.Join(bDir, "Default", "LocalStorage")); got != "session=1" {
		t.Fatalf("seeded LocalStorage = %q", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
