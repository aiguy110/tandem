package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
)

func TestACPManifestAppliesFlagsWithoutChangingRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TANDEM_HOME", home)
	var out, errOut bytes.Buffer
	if code := runACP([]string{"fork", "pi", "--repo", "https://github.com/me/pi-acp", "--commit", "aaaaaaa",
		"--upstream-package", "pi-acp", "--rebased-onto", "0.0.33", "--pr", "114", "--follow", "auto"}, &out, &errOut); code != 0 {
		t.Fatalf("fork: code=%d stderr=%s", code, errOut.String())
	}
	path := filepath.Join(t.TempDir(), runtimeinstall.BlessedManifestFile)
	out.Reset()
	if code := runACP([]string{"manifest", "pi", "--rebased-onto", "0.0.34", "--no-prs", "--write", path}, &out, &errOut); code != 0 {
		t.Fatalf("manifest: code=%d stderr=%s", code, errOut.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m runtimeinstall.BlessedManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != runtimeinstall.BlessedManifestSchema || m.UpstreamVersion != "0.0.34" || len(m.UpstreamPRs) != 0 || m.UpstreamPackage != "pi-acp" {
		t.Fatalf("manifest = %+v", m)
	}
	forks, err := config.LoadACPForks(home)
	if err != nil {
		t.Fatal(err)
	}
	if f := forks["pi"]; f.UpstreamVersion != "0.0.33" || len(f.UpstreamPRs) != 1 || f.Follow != config.ForkFollowAuto {
		t.Fatalf("record changed or follow lost: %+v", f)
	}
}

func TestACPForkRejectsUnknownFollowPolicy(t *testing.T) {
	t.Setenv("TANDEM_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := runACP([]string{"fork", "pi", "--repo", "https://github.com/me/pi-acp", "--commit", "aaaaaaa",
		"--upstream-package", "pi-acp", "--rebased-onto", "0.0.33", "--follow", "always"}, &out, &errOut); code == 0 {
		t.Fatal("expected an unknown follow policy to be rejected")
	}
}
