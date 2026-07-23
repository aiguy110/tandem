package updater

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	tests := []struct {
		current string
		latest  string
		want    bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"1.2.3", "v2.0.0", true},
		{"1.2.3-rc.1", "v1.2.3", true},
		{"1.2.3", "v1.2.3", false},
		{"1.3.0", "v1.2.9", false},
	}
	for _, test := range tests {
		t.Run(test.current+"_"+test.latest, func(t *testing.T) {
			got, err := newerVersion(test.current, test.latest)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("newerVersion(%q, %q) = %v, want %v", test.current, test.latest, got, test.want)
			}
		})
	}
}

func TestCheckAtStartupLogsWithoutDownloadingWhenNonInteractive(t *testing.T) {
	var downloads atomic.Int32
	server := releaseServer(t, []byte("new binary"), &downloads)
	defer server.Close()
	interactive := false
	var log strings.Builder

	err := CheckAtStartup(context.Background(), Options{
		CurrentVersion: "v1.0.0",
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
		Log:            &log,
		Interactive:    &interactive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "newer release is available: v1.1.0") {
		t.Fatalf("log = %q", log.String())
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("download requests = %d, want 0", got)
	}
}

func TestCheckAtStartupAcceptedUpdateReplacesExecutable(t *testing.T) {
	newBinary := []byte("new binary contents")
	var downloads atomic.Int32
	server := releaseServer(t, newBinary, &downloads)
	defer server.Close()

	target := filepath.Join(t.TempDir(), "tandem")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "answer")
	if err := os.WriteFile(input, []byte("yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	interactive := true
	var log strings.Builder
	var reexecArgv0 string
	var reexecEnv []string
	reexecCalls := 0

	err = CheckAtStartup(context.Background(), Options{
		CurrentVersion: "1.0.0",
		APIBaseURL:     server.URL,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Stdin:          stdin,
		Log:            &log,
		HTTPClient:     server.Client(),
		Executable:     target,
		Interactive:    &interactive,
		reexec: func(argv0 string, argv, envv []string) error {
			reexecCalls++
			reexecArgv0 = argv0
			reexecEnv = envv
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("updated binary = %q, want %q", got, newBinary)
	}
	if got := downloads.Load(); got != 2 {
		t.Fatalf("download requests = %d, want 2", got)
	}
	if reexecCalls != 1 {
		t.Fatalf("reexec calls = %d, want 1", reexecCalls)
	}
	if reexecArgv0 != target {
		t.Fatalf("reexec argv0 = %q, want %q", reexecArgv0, target)
	}
	if !containsEnv(reexecEnv, updatedEnvVar+"=v1.1.0") {
		t.Fatalf("reexec env missing %s=v1.1.0: %v", updatedEnvVar, reexecEnv)
	}
	if !strings.Contains(log.String(), "restarting into the new binary") {
		t.Fatalf("log = %q", log.String())
	}
}

func TestUpdatedEnvVarSkipsCheck(t *testing.T) {
	t.Setenv(updatedEnvVar, "v1.1.0")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("re-exec'd process made an HTTP request")
		return nil, nil
	})}
	if err := CheckAtStartup(context.Background(), Options{CurrentVersion: "v1.0.0", HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
}

func TestReexecFailureReportsRestartRequired(t *testing.T) {
	newBinary := []byte("new binary contents")
	var downloads atomic.Int32
	server := releaseServer(t, newBinary, &downloads)
	defer server.Close()

	target := filepath.Join(t.TempDir(), "tandem")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "answer")
	if err := os.WriteFile(input, []byte("yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	interactive := true
	var log strings.Builder

	err = CheckAtStartup(context.Background(), Options{
		CurrentVersion: "1.0.0",
		APIBaseURL:     server.URL,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Stdin:          stdin,
		Log:            &log,
		HTTPClient:     server.Client(),
		Executable:     target,
		Interactive:    &interactive,
		reexec: func(string, []string, []string) error {
			return fmt.Errorf("exec denied")
		},
	})
	if !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("err = %v, want ErrRestartRequired", err)
	}
	if !strings.Contains(log.String(), "refusing to continue on the old version") {
		t.Fatalf("log = %q", log.String())
	}
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func TestDevelopmentBuildSkipsNetwork(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("development build made an HTTP request")
		return nil, nil
	})}
	if err := CheckAtStartup(context.Background(), Options{CurrentVersion: "dev", HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
}

func releaseServer(t *testing.T, binary []byte, downloads *atomic.Int32) *httptest.Server {
	t.Helper()
	sha := fmt.Sprintf("%x", sha256.Sum256(binary))
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/aiguy110/tandem/releases/latest":
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(writer, `{"tag_name":"v1.1.0","assets":[{"name":"tandem_linux_amd64","browser_download_url":%q},{"name":"tandem_linux_amd64.sha256","browser_download_url":%q}]}`,
				server.URL+"/binary", server.URL+"/checksum")
		case "/binary":
			downloads.Add(1)
			writer.Write(binary)
		case "/checksum":
			downloads.Add(1)
			fmt.Fprintf(writer, "%s  tandem_linux_amd64\n", sha)
		default:
			http.NotFound(writer, request)
		}
	}))
	return server
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
