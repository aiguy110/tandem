// Package httpserver implements Tandem's authenticated asset API and static
// React application serving. WebSocket upgrades are intentionally layered on
// top by the daemon in a later migration phase.
package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/ui"
)

const noCache = "no-cache, no-store, must-revalidate"

type AssetStore interface {
	Put(agentID string, data []byte, declaredMIME string) (assets.Stored, error)
	Get(agentID, assetID string) (assets.Stored, error)
}

type Options struct {
	Token        string
	BootstrapURL string
	UIDir        string
	UI           fs.FS
	Assets       AssetStore
	AgentExists  func(string) bool
}

type Handler struct{ opts Options }

func New(opts Options) http.Handler {
	if opts.UI == nil && opts.UIDir == "" {
		if embedded, ok := ui.Filesystem(); ok {
			opts.UI = embedded
		}
	}
	return &Handler{opts: opts}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// URL.Path is already percent-decoded. Refuse dot segments before route
	// dispatch so encoded traversal cannot turn into a SPA response.
	if unsafePath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.serveAsset(w, r)
		return
	}
	h.serveUI(w, r)
}

func unsafePath(p string) bool {
	for _, segment := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if segment == "." || segment == ".." || strings.ContainsRune(segment, 0) {
			return true
		}
	}
	return false
}

func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request) {
	if !bearerMatches(r.Header.Get("Authorization"), h.opts.Token) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || len(parts) > 5 || parts[0] != "api" || parts[1] != "agents" || parts[3] != "assets" {
		http.NotFound(w, r)
		return
	}
	agentID, err := url.PathUnescape(parts[2])
	if err != nil || agentID == "" || h.opts.Assets == nil || h.opts.AgentExists == nil || !h.opts.AgentExists(agentID) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost && len(parts) == 4 {
		h.upload(w, r, agentID)
		return
	}
	if r.Method == http.MethodGet && len(parts) == 5 {
		h.download(w, r, agentID, parts[4])
		return
	}
	http.NotFound(w, r)
}

func bearerMatches(header, token string) bool {
	want := "Bearer " + token
	return token != "" && len(header) == len(want) && subtle.ConstantTimeCompare([]byte(header), []byte(want)) == 1
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request, agentID string) {
	if r.ContentLength > assets.MaxAssetBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("image exceeds %d byte limit", assets.MaxAssetBytes)})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, assets.MaxAssetBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	stored, err := h.opts.Assets.Put(agentID, data, r.Header.Get("Content-Type"))
	if err != nil {
		status := http.StatusInternalServerError
		var tooLarge *assets.TooLargeError
		var unsupported *assets.UnsupportedError
		switch {
		case errors.As(err, &tooLarge):
			status = http.StatusRequestEntityTooLarge
		case errors.As(err, &unsupported):
			status = http.StatusUnsupportedMediaType
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	name := uploadName(r.Header.Get("X-File-Name"))
	writeJSON(w, http.StatusCreated, map[string]any{"asset": map[string]any{
		"assetId": stored.AssetID, "mimeType": stored.MIMEType, "size": stored.Size, "name": name,
	}})
}

func uploadName(raw string) string {
	if decoded, err := url.PathUnescape(raw); err == nil && raw != "" {
		raw = decoded
	}
	if raw == "" {
		raw = "image"
	}
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, raw)
	if utf8.RuneCountInString(raw) > 255 {
		runes := []rune(raw)
		raw = string(runes[:255])
	}
	return raw
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request, agentID, assetID string) {
	stored, err := h.opts.Assets.Get(agentID, assetID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", stored.MIMEType)
	w.Header().Set("Content-Length", fmt.Sprint(stored.Size))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(stored.Data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *Handler) serveUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if rel == "." || rel == "" {
		rel = "index.html"
	}
	data, found := h.readUI(rel)
	if !found && path.Ext(rel) == "" {
		data, found = h.readUI("index.html")
		if found {
			rel = "index.html"
		}
	}
	if !found {
		if rel != "index.html" && path.Ext(rel) != "" {
			http.NotFound(w, r)
			return
		}
		data = []byte(placeholder(h.opts.BootstrapURL, h.opts.UIDir != ""))
		rel = "index.html"
	}
	w.Header().Set("Content-Type", contentType(rel))
	w.Header().Set("Cache-Control", noCache)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (h *Handler) readUI(name string) ([]byte, bool) {
	if !fs.ValidPath(name) {
		return nil, false
	}
	if h.opts.UIDir != "" {
		root, err := os.OpenRoot(h.opts.UIDir)
		if err != nil {
			return nil, false
		}
		defer root.Close()
		file, err := root.Open(name)
		if err != nil {
			return nil, false
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		return data, err == nil
	}
	if h.opts.UI == nil {
		return nil, false
	}
	data, err := fs.ReadFile(h.opts.UI, name)
	return data, err == nil
}

func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript"
	case ".css":
		return "text/css"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".wasm":
		return "application/wasm"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".ttf":
		return "font/ttf"
	}
	if detected := mime.TypeByExtension(path.Ext(name)); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

func placeholder(bootstrapURL string, configured bool) string {
	state := "No UI is built into this development executable (set <code>TANDEM_UI_DIR</code> to serve one)."
	if configured {
		state = "No <code>index.html</code> was found in the configured UI dir."
	}
	// Never include a query token. Bootstrap tokens belong in URL fragments,
	// which browsers do not send in requests or Referer headers.
	if parsed, err := url.Parse(bootstrapURL); err == nil {
		parsed.RawQuery = ""
		bootstrapURL = parsed.String()
	}
	return `<!doctype html><meta charset="utf-8"><title>Tandem daemon</title>
<style>body{font:14px/1.6 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222}code{background:#f2f2f2;padding:.1em .3em;border-radius:3px}</style>
<h1>Tandem daemon</h1><p>` + state + `</p>
<p>Connect a client to the WebSocket on this same port, presenting the bearer token:</p>
<p><code>ws://&lt;host&gt;/?token=&lt;token&gt;</code></p>
<p>Bootstrap URL (token in the URL fragment, never sent to the server):<br><code>` + htmlEscape(bootstrapURL) + `</code></p>`
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}
