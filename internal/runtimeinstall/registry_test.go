package runtimeinstall

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestRegistryLookupsEscapeScopedNamesAndSortVersions(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		if r.URL.EscapedPath() != "/@oh-my-pi%2Fpi-coding-agent" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"versions":{"18.9.0":{"dist":{"integrity":"sha512-old"}},"18.11.1":{"dist":{"integrity":"sha512-new"}},"18.12.0-beta.1":{}}}`))
	}))
	defer srv.Close()
	old := registryURL
	registryURL = func() string { return srv.URL + "/" }
	defer func() { registryURL = old }()

	versions, err := publishedVersions(context.Background(), "@oh-my-pi/pi-coding-agent")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(versions, []string{"18.12.0-beta.1", "18.11.1", "18.9.0"}) {
		t.Fatalf("versions = %v", versions)
	}
	if pick, ok := newestInRange(versions, "^18.11.0"); !ok || pick != "18.11.1" {
		t.Fatalf("range pick = %q %v, want the newest release in range", pick, ok)
	}
	if got := publishedIntegrity(context.Background(), "@oh-my-pi/pi-coding-agent", "18.11.1"); got != "sha512-new" {
		t.Fatalf("integrity = %q", got)
	}
	if _, err := publishedVersions(context.Background(), "missing-package"); err == nil {
		t.Fatal("expected an error for a package the registry does not have")
	}
	if paths[0] != "/@oh-my-pi%2Fpi-coding-agent" {
		t.Fatalf("scoped package requested as %q", paths[0])
	}
}
