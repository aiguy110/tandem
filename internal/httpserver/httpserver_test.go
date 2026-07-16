package httpserver

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/aiguy110/tandem/internal/assets"
)

type fakeAssets struct {
	put      assets.Stored
	putErr   error
	get      assets.Stored
	getErr   error
	putAgent string
	putData  []byte
	putMIME  string
	getAgent string
	getAsset string
}

func (f *fakeAssets) Put(agentID string, data []byte, declaredMIME string) (assets.Stored, error) {
	f.putAgent, f.putData, f.putMIME = agentID, append([]byte(nil), data...), declaredMIME
	return f.put, f.putErr
}

func (f *fakeAssets) Get(agentID, assetID string) (assets.Stored, error) {
	f.getAgent, f.getAsset = agentID, assetID
	return f.get, f.getErr
}

func request(t *testing.T, handler http.Handler, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestStaticUIAndSPAFallback(t *testing.T) {
	files := fstest.MapFS{
		"index.html":           {Data: []byte("<main>app shell</main>")},
		"assets/app.js":        {Data: []byte("app()")},
		"ghostty-vt.wasm":      {Data: []byte("wasm")},
		"assets/display.woff2": {Data: []byte("font")},
	}
	h := New(Options{UI: files})
	for _, tc := range []struct {
		path, contentType, body string
	}{
		{"/", "text/html; charset=utf-8", "app shell"},
		{"/agents/api-1/transcript", "text/html; charset=utf-8", "app shell"},
		{"/assets/app.js", "text/javascript", "app()"},
		{"/ghostty-vt.wasm", "application/wasm", "wasm"},
		{"/assets/display.woff2", "font/woff2", "font"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := request(t, h, http.MethodGet, tc.path, nil, nil)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != tc.contentType || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("response=%d content-type=%q body=%q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
			}
			if got := w.Header().Get("Cache-Control"); got != noCache {
				t.Fatalf("cache-control=%q", got)
			}
		})
	}
	w := request(t, h, http.MethodGet, "/assets/missing.js", nil, nil)
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "app shell") {
		t.Fatalf("missing static response=%d %q", w.Code, w.Body.String())
	}
	w = request(t, h, http.MethodHead, "/", nil, nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD response=%d length=%q body=%q", w.Code, w.Header().Get("Content-Length"), w.Body.String())
	}
}

func TestExternalUIAndTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/index.html", []byte("external shell"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() + "/secret.js"
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir+"/escape.js"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	h := New(Options{UIDir: dir, UI: fstest.MapFS{"index.html": {Data: []byte("embedded")}}})
	if w := request(t, h, http.MethodGet, "/", nil, nil); w.Code != http.StatusOK || w.Body.String() != "external shell" {
		t.Fatalf("external UI not preferred: %d %q", w.Code, w.Body.String())
	}
	for _, target := range []string{"/../secret.js", "/%2e%2e/secret.js", "/escape.js", "/api/agents/a/assets/%2e%2e"} {
		w := request(t, h, http.MethodGet, target, nil, map[string]string{"Authorization": "Bearer token"})
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("traversal %q response=%d %q", target, w.Code, w.Body.String())
		}
	}
}

func TestDevelopmentPlaceholderDoesNotPromoteQueryToken(t *testing.T) {
	h := New(Options{UI: fstest.MapFS{}, BootstrapURL: "http://127.0.0.1:7717/?token=leaked#t=fragment"})
	w := request(t, h, http.MethodGet, "/", nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No UI is built") || strings.Contains(w.Body.String(), "token=leaked") || !strings.Contains(w.Body.String(), "#t=fragment") {
		t.Fatalf("placeholder response=%d %q", w.Code, w.Body.String())
	}
}

func TestAssetBearerAuthenticationAndRoutes(t *testing.T) {
	id := strings.Repeat("a", 64)
	store := &fakeAssets{
		put: assets.Stored{AssetID: id, MIMEType: "image/png", Size: 3},
		get: assets.Stored{AssetID: id, MIMEType: "image/png", Size: 3, Data: []byte("png")},
	}
	h := New(Options{Token: "correct", Assets: store, AgentExists: func(id string) bool { return id == "agent one" }, UI: fstest.MapFS{}})

	for _, target := range []string{"/api/agents/agent%20one/assets", "/api/agents/agent%20one/assets?token=correct"} {
		w := request(t, h, http.MethodPost, target, bytes.NewReader([]byte("png")), map[string]string{"Content-Type": "image/png"})
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "unauthorized") {
			t.Fatalf("unauthorized response=%d %q", w.Code, w.Body.String())
		}
	}

	w := request(t, h, http.MethodPost, "/api/agents/agent%20one/assets", bytes.NewReader([]byte("png")), map[string]string{
		"Authorization": "Bearer correct", "Content-Type": "image/png; charset=binary", "X-File-Name": "screens%2Fshot%00.png",
	})
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"name":"screens_shot_.png"`) || store.putAgent != "agent one" || string(store.putData) != "png" {
		t.Fatalf("upload response=%d %q store=%#v", w.Code, w.Body.String(), store)
	}

	w = request(t, h, http.MethodGet, "/api/agents/agent%20one/assets/"+id, nil, map[string]string{"Authorization": "Bearer correct"})
	if w.Code != http.StatusOK || w.Body.String() != "png" || w.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download response=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	if store.getAgent != "agent one" || store.getAsset != id {
		t.Fatalf("download lookup=%q/%q", store.getAgent, store.getAsset)
	}

	w = request(t, h, http.MethodGet, "/api/agents/missing/assets/"+id, nil, map[string]string{"Authorization": "Bearer correct"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing agent response=%d", w.Code)
	}
}

func TestAssetUploadLimitsAndValidationStatus(t *testing.T) {
	store := &fakeAssets{}
	h := New(Options{Token: "token", Assets: store, AgentExists: func(string) bool { return true }, UI: fstest.MapFS{}})
	headers := map[string]string{"Authorization": "Bearer token"}
	r := httptest.NewRequest(http.MethodPost, "/api/agents/a/assets", strings.NewReader("x"))
	r.ContentLength = assets.MaxAssetBytes + 1
	r.Header.Set("Authorization", headers["Authorization"])
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversized response=%d %q", w.Code, w.Body.String())
	}
	w = request(t, h, http.MethodPost, "/api/agents/a/assets", bytes.NewReader(bytes.Repeat([]byte{'x'}, assets.MaxAssetBytes+1)), headers)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed oversized response=%d %q", w.Code, w.Body.String())
	}

	store.putErr = &assets.UnsupportedError{Message: "unsupported"}
	w = request(t, h, http.MethodPost, "/api/agents/a/assets", strings.NewReader("x"), headers)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported response=%d %q", w.Code, w.Body.String())
	}
	store.putErr = errors.New("disk broke")
	w = request(t, h, http.MethodPost, "/api/agents/a/assets", strings.NewReader("x"), headers)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("storage failure response=%d %q", w.Code, w.Body.String())
	}
}
