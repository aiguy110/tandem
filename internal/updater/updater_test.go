package updater

import (
	"context"
	"crypto/sha256"
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

func TestDevelopmentVersion(t *testing.T) {
	for value, want := range map[string]bool{
		"": true, "dev": true, "v0.5.1.1234abcd": true,
		"v0.5.1": false, "v0.5.1.1234abc": false, "v0.5.1.1234abcd0": false,
	} {
		if got := isDevelopmentVersion(value); got != want {
			t.Errorf("isDevelopmentVersion(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestCheckAtStartupReportsWithoutDownloading(t *testing.T) {
	var downloads atomic.Int32
	server := releaseServer(t, []byte("new binary"), &downloads)
	defer server.Close()
	var log strings.Builder

	err := CheckAtStartup(context.Background(), Options{
		CurrentVersion: "v1.0.0",
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
		Log:            &log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "newer release is available: v1.1.0") {
		t.Fatalf("log = %q", log.String())
	}
	if !strings.Contains(log.String(), "run 'tandem update'") {
		t.Fatalf("log = %q, want update command", log.String())
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("download requests = %d, want 0", got)
	}
}

func TestUpdateReplacesExecutable(t *testing.T) {
	newBinary := []byte("new binary contents")
	var downloads atomic.Int32
	server := releaseServer(t, newBinary, &downloads)
	defer server.Close()

	target := filepath.Join(t.TempDir(), "tandem")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder

	updated, err := UpdateWithResult(context.Background(), Options{
		CurrentVersion: "1.0.0",
		APIBaseURL:     server.URL,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Log:            &log,
		HTTPClient:     server.Client(),
		Executable:     target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("UpdateWithResult reported no update")
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
	if !strings.Contains(log.String(), "updated "+target+" from 1.0.0 to v1.1.0") {
		t.Fatalf("log = %q", log.String())
	}
}

func TestUpdateAlreadyCurrentDoesNotDownload(t *testing.T) {
	var downloads atomic.Int32
	server := releaseServer(t, []byte("new binary contents"), &downloads)
	defer server.Close()
	var log strings.Builder

	updated, err := UpdateWithResult(context.Background(), Options{
		CurrentVersion: "v1.1.0",
		APIBaseURL:     server.URL,
		Log:            &log,
		HTTPClient:     server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("UpdateWithResult reported an update")
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("download requests = %d, want 0", got)
	}
	if !strings.Contains(log.String(), "already up to date") {
		t.Fatalf("log = %q", log.String())
	}
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

func TestDevelopmentBuildCannotSelfUpdate(t *testing.T) {
	if err := Update(context.Background(), Options{CurrentVersion: "dev"}); err == nil || !strings.Contains(err.Error(), "development builds") {
		t.Fatalf("Update(dev) error = %v", err)
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
