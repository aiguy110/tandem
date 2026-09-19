package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleFork() ACPFork {
	return ACPFork{
		Repo: "https://github.com/me/pi-acp", Ref: "tandem",
		Commit:          "52f61f3850a883b9b5a3bb3245908bd5d95b7d43",
		UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.33",
		Reason: "Emits usage_update", UpstreamPRs: []int{114},
	}
}

func TestACPForkRoundTripPreservesUnrelatedKeys(t *testing.T) {
	home := t.TempDir()
	// A pre-existing config with sibling blocks that must survive the write.
	existing := "settings:\n  port: 4321\nmcpServers:\n  demo:\n    command: echo\n"
	if err := os.WriteFile(ConfigFilePath(home), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveACPFork(home, "pi", sampleFork()); err != nil {
		t.Fatalf("save: %v", err)
	}

	forks, err := LoadACPForks(home)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := forks["pi"]
	if !ok {
		t.Fatalf("fork not persisted: %+v", forks)
	}
	if got.Ref != "tandem" || got.UpstreamVersion != "0.0.33" || len(got.UpstreamPRs) != 1 || got.UpstreamPRs[0] != 114 {
		t.Fatalf("unexpected fork: %+v", got)
	}

	settings, err := LoadSettings(home)
	if err != nil || settings.Port != 4321 {
		t.Fatalf("settings block lost: %+v err=%v", settings, err)
	}
	servers, err := LoadMCPServers(home, "")
	if err != nil || servers["demo"].Command != "echo" {
		t.Fatalf("mcpServers block lost: %+v err=%v", servers, err)
	}
}

func TestACPForkDeleteRemovesBlockEntirely(t *testing.T) {
	home := t.TempDir()
	if _, err := SaveACPFork(home, "pi", sampleFork()); err != nil {
		t.Fatal(err)
	}
	_, existed, err := DeleteACPFork(home, "pi")
	if err != nil || !existed {
		t.Fatalf("delete: existed=%v err=%v", existed, err)
	}
	forks, err := LoadACPForks(home)
	if err != nil || len(forks) != 0 {
		t.Fatalf("expected no forks, got %+v err=%v", forks, err)
	}
	// An empty mapping would be noise in a hand-edited file.
	b, err := os.ReadFile(ConfigFilePath(home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), ACPForksKey) {
		t.Fatalf("expected %s key dropped, got:\n%s", ACPForksKey, b)
	}
	// Deleting an absent record is not an error; it reports existed=false.
	if _, existed, err = DeleteACPFork(home, "pi"); err != nil || existed {
		t.Fatalf("second delete: existed=%v err=%v", existed, err)
	}
}

func TestACPForkLoadRejectsInvalidRecords(t *testing.T) {
	for name, body := range map[string]string{
		"non-https repo":           "acpForks:\n  pi:\n    repo: git@github.com:me/pi-acp.git\n    ref: tandem\n    commit: abcdef1\n    upstreamPackage: pi-acp\n    upstreamVersion: 0.0.33\n",
		"branch as commit":         "acpForks:\n  pi:\n    repo: https://github.com/me/pi-acp\n    ref: tandem\n    commit: tandem\n    upstreamPackage: pi-acp\n    upstreamVersion: 0.0.33\n",
		"missing upstream version": "acpForks:\n  pi:\n    repo: https://github.com/me/pi-acp\n    ref: tandem\n    commit: abcdef1\n    upstreamPackage: pi-acp\n",
		"missing package":          "acpForks:\n  pi:\n    repo: https://github.com/me/pi-acp\n    ref: tandem\n    commit: abcdef1\n    upstreamVersion: 0.0.33\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(ConfigFilePath(home), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadACPForks(home); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
}

func TestACPForkLoadAbsentFileIsEmpty(t *testing.T) {
	forks, err := LoadACPForks(t.TempDir())
	if err != nil || len(forks) != 0 {
		t.Fatalf("expected empty, got %+v err=%v", forks, err)
	}
}

func TestACPForkInstallSpecAndVersion(t *testing.T) {
	f := sampleFork()
	want := "git+https://github.com/me/pi-acp.git#52f61f3850a883b9b5a3bb3245908bd5d95b7d43"
	if got := f.InstallSpec(); got != want {
		t.Fatalf("install spec = %q, want %q", got, want)
	}
	// A repo written with a .git suffix must not produce a doubled one.
	f.Repo = "https://github.com/me/pi-acp.git"
	if got := f.InstallSpec(); got != want {
		t.Fatalf("install spec with .git suffix = %q, want %q", got, want)
	}
	if got := f.DistributionVersion(); got != "0.0.33+fork.52f61f3850a8" {
		t.Fatalf("distribution version = %q", got)
	}
	if !IsForkVersion(f.DistributionVersion()) || IsForkVersion("0.0.33") {
		t.Fatal("IsForkVersion misclassified")
	}
}

func TestForkCloneDirDefaultsUnderRuntimeRoot(t *testing.T) {
	f := sampleFork()
	if got := ForkCloneDir("/rt", "pi", f); got != filepath.Join("/rt", "forks", "pi") {
		t.Fatalf("default clone dir = %q", got)
	}
	f.Clone = "/elsewhere/pi-acp"
	if got := ForkCloneDir("/rt", "pi", f); got != "/elsewhere/pi-acp" {
		t.Fatalf("override clone dir = %q", got)
	}
}
