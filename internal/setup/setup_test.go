package setup

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
)

func TestUnitFile(t *testing.T) {
	unit := unitFile("/path/to/tandem", "/home/op/.tandem", "/home/op/.tandem/node/bin")

	if !strings.Contains(unit, "ExecStart=/path/to/tandem\n") {
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
	// service, URL, blank API key, managed Node, decline shared language-model
	// setup, then accept the
	// default systemd answer at EOF.
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
	if !strings.Contains(out.String(), "Kokoro-FastAPI") || !strings.Contains(out.String(), "OpenAI-compatible") {
		t.Errorf("expected voice provider guidance, got:\n%s", out.String())
	}
}

func TestPromptLanguageModelConfiguresSharedEndpoint(t *testing.T) {
	answers := strings.Join([]string{
		"y", "http://localhost:11434/v1/chat/completions", "ollama", "gpt-oss:20b",
	}, "\n") + "\n"
	var out bytes.Buffer
	got, eof, err := promptLanguageModel(bufio.NewReader(strings.NewReader(answers)), &out, config.LanguageModelSettings{}, config.VoiceSettings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if eof || got.Endpoint != "http://localhost:11434/v1/chat/completions" || got.APIKey != "ollama" || got.Model != "gpt-oss:20b" {
		t.Fatalf("language model settings = %+v, eof=%v", got, eof)
	}
	if !strings.Contains(out.String(), "docs.ollama.com") || !strings.Contains(out.String(), "localai.io") {
		t.Fatalf("missing local provider links:\n%s", out.String())
	}
}

func TestPromptVoiceConfiguresSpeechEndpoint(t *testing.T) {
	answers := strings.Join([]string{
		"y", "Speak plainly.", "http://localhost:8880/v1/audio/speech", "", "kokoro", "af_sky", "mp3",
	}, "\n") + "\n"
	var out bytes.Buffer
	got, err := promptVoice(bufio.NewReader(strings.NewReader(answers)), &out, config.VoiceSettings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Instructions != "Speak plainly." || got.TTSModel != "kokoro" || got.TTSVoice != "af_sky" || got.TTSFormat != "mp3" {
		t.Fatalf("voice settings = %+v", got)
	}
}

func TestPromptLanguageModelMigratesLegacyVoiceSettingsAtEOF(t *testing.T) {
	legacy := config.VoiceSettings{LegacyCleanupEndpoint: "http://legacy/clean", LegacyCleanupAPIKey: "key", LegacyCleanupModel: "model"}
	got, eof, err := promptLanguageModel(bufio.NewReader(strings.NewReader("")), io.Discard, config.LanguageModelSettings{}, legacy, true)
	if err != nil || !eof || got.Endpoint != "http://legacy/clean" || got.APIKey != "key" || got.Model != "model" {
		t.Fatalf("legacy migration = %+v, eof=%v, err=%v", got, eof, err)
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
