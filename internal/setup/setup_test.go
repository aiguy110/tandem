package setup

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
)

func TestUnitFile(t *testing.T) {
	unit := unitFile("/path/to/tandem", "/home/op/.tandem", "/home/op/.tandem/node/bin")

	if !strings.Contains(unit, "ExecStart=/path/to/tandem daemon") {
		t.Errorf("unit file missing ExecStart line:\n%s", unit)
	}
	if !strings.Contains(unit, "Environment=PATH=/home/op/.tandem/node/bin:/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("unit file missing PATH line:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("unit file missing WantedBy line:\n%s", unit)
	}
}

func TestUnitFileNoNodeBinDir(t *testing.T) {
	unit := unitFile("/path/to/tandem", "/home/op/.tandem", "")

	if !strings.Contains(unit, "Environment=PATH=/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("unit file missing bare PATH line:\n%s", unit)
	}
}

func TestRunEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TANDEM_HOME", home)
	steel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer steel.Close()

	// Answers in prompt order: project roots, bind, port, Steel, existing
	// service, URL, blank API key, managed Node, then decline systemd.
	answers := strings.Join([]string{
		"/tmp/proj-a, /tmp/proj-b",
		"0.0.0.0",
		"8080",
		"2",
		"2",
		steel.URL,
		"",
		"2",
		"n",
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(answers), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	settings, err := config.LoadSettings(home)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}

	wantRoots := []string{"/tmp/proj-a", "/tmp/proj-b"}
	if len(settings.ProjectRoots) != len(wantRoots) {
		t.Fatalf("ProjectRoots = %v, want %v", settings.ProjectRoots, wantRoots)
	}
	for i, root := range wantRoots {
		if settings.ProjectRoots[i] != root {
			t.Errorf("ProjectRoots[%d] = %q, want %q", i, settings.ProjectRoots[i], root)
		}
	}
	if settings.Bind != "0.0.0.0" {
		t.Errorf("Bind = %q, want 0.0.0.0", settings.Bind)
	}
	if settings.Port != 8080 {
		t.Errorf("Port = %d, want 8080", settings.Port)
	}
	if settings.BrowserDriver != "steel" {
		t.Errorf("BrowserDriver = %q, want steel", settings.BrowserDriver)
	}
	if settings.SteelBaseURL != steel.URL {
		t.Errorf("SteelBaseURL = %q, want %q", settings.SteelBaseURL, steel.URL)
	}
	if settings.Node.Mode != "managed" {
		t.Errorf("Node.Mode = %q, want managed", settings.Node.Mode)
	}
	if settings.Node.Version != config.DefaultManagedNodeVersion {
		t.Errorf("Node.Version = %q, want %q", settings.Node.Version, config.DefaultManagedNodeVersion)
	}

	if !strings.Contains(out.String(), config.ConfigFilePath(home)) {
		t.Errorf("expected wizard output to mention config path, got:\n%s", out.String())
	}
}

func TestPromptBrowserLocal(t *testing.T) {
	var out bytes.Buffer
	got, eof, err := promptBrowser(bufio.NewReader(strings.NewReader("1\n")), &out, config.Settings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if eof || got.BrowserDriver != "local" || got.SteelBaseURL != "" {
		t.Fatalf("browser settings = %+v, eof=%v", got, eof)
	}
	if !strings.Contains(out.String(), "Browser readiness: local") {
		t.Fatalf("missing readiness summary:\n%s", out.String())
	}
}
