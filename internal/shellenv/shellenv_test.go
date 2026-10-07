package shellenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const helperVar = "SHELLENV_TEST_DUMP"

// TestMain lets the test binary stand in for `tandem __shell-env-dump`.
func TestMain(m *testing.M) {
	if os.Getenv(helperVar) == "1" {
		if err := Dump(os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// bashWithProfile returns Options running bash as a login shell whose only rc
// file is a temporary ~/.bash_profile with the given contents.
func bashWithProfile(t *testing.T, profile string) Options {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{
		Shell:       bash,
		DumpCommand: []string{os.Args[0]},
		Env:         []string{"HOME=" + home, "PATH=/usr/bin:/bin", helperVar + "=1"},
		Timeout:     5 * time.Second,
	}
}

func TestResolveCapturesLoginShellEnvironmentDespiteNoise(t *testing.T) {
	opts := bashWithProfile(t, `
echo "welcome banner"
echo "stderr noise" >&2
export FROM_RC=yes
export PATH="/rc/bin:$PATH"
`)
	env, err := Resolve(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if env["FROM_RC"] != "yes" {
		t.Fatalf("FROM_RC = %q, want yes", env["FROM_RC"])
	}
	if !strings.HasPrefix(env["PATH"], "/rc/bin:") {
		t.Fatalf("PATH = %q, want /rc/bin first", env["PATH"])
	}
	if env[GuardVar] != "1" || env[vscodeGuardVar] != "1" {
		t.Fatalf("guard vars not visible to rc files: %q %q", env[GuardVar], env[vscodeGuardVar])
	}
}

func TestResolveGuardLetsRCSkipSlowSetup(t *testing.T) {
	opts := bashWithProfile(t, `[ -n "$TANDEM_RESOLVING_ENVIRONMENT" ] || sleep 30
export FROM_RC=yes`)
	env, err := Resolve(context.Background(), opts)
	if err != nil || env["FROM_RC"] != "yes" {
		t.Fatalf("env=%v err=%v", env["FROM_RC"], err)
	}
}

func TestResolveTimesOutAndKillsHungShell(t *testing.T) {
	opts := bashWithProfile(t, "sleep 30\n")
	opts.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := Resolve(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Resolve took %s after a 300ms timeout", elapsed)
	}
}

func TestResolveDoesNotWaitOnBackgroundedRCPrograms(t *testing.T) {
	// A daemon started from rc (ssh-agent and friends) inherits our stdout.
	opts := bashWithProfile(t, "sleep 20 &\nexport FROM_RC=yes\n")
	start := time.Now()
	env, err := Resolve(context.Background(), opts)
	if err != nil || env["FROM_RC"] != "yes" {
		t.Fatalf("env=%v err=%v", env["FROM_RC"], err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Resolve waited %s on a backgrounded rc program", elapsed)
	}
}

func TestResolveReportsShellFailure(t *testing.T) {
	opts := bashWithProfile(t, "")
	opts.DumpCommand = []string{"/nonexistent/tandem"}
	if _, err := Resolve(context.Background(), opts); err == nil {
		t.Fatal("expected an error when the dump command cannot run")
	}
}

func TestMergeKeepsDaemonValuesAndPrependsShellPath(t *testing.T) {
	current := map[string]string{
		"PATH":        "/fnm/bin:/usr/bin:/bin",
		"LANG":        "C.UTF-8",
		"TANDEM_HOME": "/srv/tandem",
	}
	shell := map[string]string{
		"PATH":             "/home/u/.local/bin:/usr/bin:/home/u/.local/bin:/bin:/home/u/go/bin",
		"LANG":             "en_US.UTF-8",
		"GOPATH":           "/home/u/go",
		"TANDEM_HOME":      "/elsewhere",
		"TANDEM_EXTRA":     "x",
		"PWD":              "/home/u",
		"SHLVL":            "1",
		"_":                "/usr/bin/env",
		"TERM":             "dumb",
		GuardVar:           "1",
		vscodeGuardVar:     "1",
		MarkerVar:          "m",
		"ANTHROPIC_SECRET": "s",
	}
	res := Merge(current, shell)
	wantSet := map[string]string{"GOPATH": "/home/u/go", "ANTHROPIC_SECRET": "s"}
	if !reflect.DeepEqual(res.Set, wantSet) {
		t.Fatalf("Set = %v, want %v", res.Set, wantSet)
	}
	if want := "/home/u/.local/bin:/usr/bin:/bin:/home/u/go/bin:/fnm/bin"; res.Path != want {
		t.Fatalf("Path = %q, want %q", res.Path, want)
	}
	if want := []string{"/home/u/.local/bin", "/home/u/go/bin"}; !reflect.DeepEqual(res.PathAdded, want) {
		t.Fatalf("PathAdded = %v, want %v", res.PathAdded, want)
	}
}

func TestMergeLeavesPathAloneWhenShellAddsNothing(t *testing.T) {
	res := Merge(map[string]string{"PATH": "/usr/bin:/bin"}, map[string]string{"PATH": "/usr/bin:/bin"})
	if res.Path != "" || len(res.PathAdded) != 0 {
		t.Fatalf("unexpected PATH change: %+v", res)
	}
	res = Merge(map[string]string{"PATH": "/usr/bin"}, map[string]string{})
	if res.Path != "" {
		t.Fatalf("empty shell PATH changed PATH to %q", res.Path)
	}
}

func TestParseDumpRejectsMissingOrTruncatedMarkers(t *testing.T) {
	if _, err := parseDump([]byte("noise only"), "MK"); err == nil {
		t.Fatal("expected error for missing marker")
	}
	if _, err := parseDump([]byte(`noiseMK{"A":"1"}`), "MK"); err == nil {
		t.Fatal("expected error for truncated dump")
	}
	env, err := parseDump([]byte(`banner MK{"A":"1"}MK trailing`), "MK")
	if err != nil || env["A"] != "1" {
		t.Fatalf("env=%v err=%v", env, err)
	}
}

func TestShellArgs(t *testing.T) {
	for shell, want := range map[string][]string{
		"/bin/zsh":       {"-i", "-l", "-c"},
		"/usr/bin/fish":  {"-i", "-l", "-c"},
		"/bin/tcsh":      {"-i", "-c"},
		"/usr/bin/pwsh":  {"-Login", "-Command"},
		"/usr/bin/bash":  {"-i", "-l", "-c"},
		"/opt/nu/bin/nu": {"-i", "-l", "-c"},
	} {
		if got := shellArgs(shell); !reflect.DeepEqual(got, want) {
			t.Errorf("shellArgs(%s) = %v, want %v", shell, got, want)
		}
	}
	if got := shellCommand("/bin/zsh", []string{"/opt/it's/tandem", DumpArg}); got != `'/opt/it'\''s/tandem' '__shell-env-dump'` {
		t.Errorf("shellCommand quoting = %s", got)
	}
}
