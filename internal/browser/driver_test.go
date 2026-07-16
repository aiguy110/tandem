package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSteelDriverLifecycle(t *testing.T) {
	t.Parallel()
	var paths []string
	var createBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("steel-api-key") != "secret" {
			t.Errorf("missing API key")
		}
		switch r.URL.Path {
		case "/v1/sessions":
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "session-1", "websocketUrl": "ws://0.0.0.0:3000/devtools/browser/1", "profileId": "profile-1"})
		case "/v1/sessions/session-1/release":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	d, err := NewSteelDriver(SteelConfig{BaseURL: server.URL, APIKey: "secret", SessionOptions: map[string]any{"solveCaptcha": true}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Provision(context.Background(), "agent/one")
	if err != nil {
		t.Fatal(err)
	}
	if got.CDPURL != "ws://127.0.0.1:3000/devtools/browser/1" {
		t.Fatalf("normalized URL = %q", got.CDPURL)
	}
	if !d.IsProvisioned("agent/one") || d.PID("agent/one") != 0 {
		t.Fatal("bad provision state")
	}
	got2, err := d.Provision(context.Background(), "agent/one")
	if err != nil || got2 != got {
		t.Fatalf("idempotent provision = %#v, %v", got2, err)
	}
	if createBody["solveCaptcha"] != true {
		t.Fatalf("session options not merged: %#v", createBody)
	}
	if err := d.Teardown(context.Background(), "agent/one"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/v1/sessions", "/v1/sessions/session-1/release"}) {
		t.Fatalf("paths = %v", paths)
	}
}

func TestDiscoverChromiumConfigured(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "my-chrome")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverChromium(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != exe {
		t.Fatalf("got %q", got)
	}
	if _, err := DiscoverChromium(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected configured executable error")
	}
}

func TestDriverSelection(t *testing.T) {
	d, err := NewDriver(DriverConfig{Driver: "local", UserDataRoot: t.TempDir(), ChromiumExecutable: "configured-at-provision"})
	if err != nil || d.Kind() != "local" {
		t.Fatalf("local selection: %T, %v", d, err)
	}
	if _, err := NewDriver(DriverConfig{Driver: "steel"}); err == nil {
		t.Fatal("steel without base URL accepted")
	}
	if _, err := NewDriver(DriverConfig{Driver: "mystery"}); err == nil {
		t.Fatal("unknown driver accepted")
	}
}
