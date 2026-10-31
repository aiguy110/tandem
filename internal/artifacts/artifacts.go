// Package artifacts publishes rendered artifacts (docs/artifacts.md) for the
// tandem-artifacts MCP server (internal/artifactsmcp). An artifact's bytes are
// stored as a session asset and an "artifact" event on the session's
// transcript announces it, so the UI previews it inline and can replay it on
// reconnect like any other transcript row.
package artifacts

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/eventlog"
)

// Path is the prefix of the agent-facing HTTP bridge.
const Path = "/internal/artifacts/"

// EventKind is the transcript event announcing a published artifact.
const EventKind = "artifact"

// TypeHTML is the only artifact type so far: a free-form HTML document.
const TypeHTML = "html"

const maxTitleRunes = 200

// Store persists artifact bytes for a session.
type Store interface {
	PutHTML(sessionID string, data []byte) (assets.Stored, error)
}

// Sessions resolves a live session for its transcript.
type Sessions interface {
	AppendEvent(sessionID string, ev eventlog.Event) error
}

// Options configures a Service.
type Options struct {
	Token    string
	Store    Store
	Sessions Sessions
	Now      func() time.Time
}

// Service serves POST /internal/artifacts/publish.
type Service struct{ opts Options }

// New returns a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{opts: opts}
}

// PublishRequest is one artifact_publish call, after the MCP server has read
// any file argument into Content.
type PublishRequest struct {
	SessionID string `json:"sessionId"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Filename  string `json:"filename"`
	Content   string `json:"content"`
	// SourcePath is the file the content was read from, if any. It is shown
	// for provenance only; the daemon never reads it.
	SourcePath string `json:"sourcePath"`
}

// Artifact describes a published artifact. It is both the tool result and the
// payload of the transcript event.
type Artifact struct {
	Kind       string `json:"kind"`
	ArtifactID string `json:"artifactId"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Filename   string `json:"filename"`
	MIMEType   string `json:"mimeType"`
	Size       int64  `json:"size"`
	SourcePath string `json:"sourcePath,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
}

// Publish stores an artifact and announces it on the session's transcript.
func (s *Service) Publish(req PublishRequest) (Artifact, error) {
	if req.SessionID == "" {
		return Artifact{}, errors.New("invalid: sessionId is required")
	}
	if req.Type == "" {
		req.Type = TypeHTML
	}
	if req.Type != TypeHTML {
		return Artifact{}, errors.New("invalid: unsupported artifact type " + req.Type + " (supported: html)")
	}
	stored, err := s.opts.Store.PutHTML(req.SessionID, []byte(req.Content))
	if err != nil {
		var tooLarge *assets.TooLargeError
		var unsupported *assets.UnsupportedError
		if errors.As(err, &tooLarge) || errors.As(err, &unsupported) {
			return Artifact{}, errors.New("invalid: " + err.Error())
		}
		return Artifact{}, err
	}
	title := cleanTitle(req.Title, req.SourcePath)
	a := Artifact{
		Kind: EventKind, ArtifactID: stored.AssetID, Type: req.Type, Title: title,
		Filename: downloadName(req.Filename, req.SourcePath, title), MIMEType: stored.MIMEType,
		Size: stored.Size, SourcePath: req.SourcePath, CreatedAt: s.opts.Now().UnixMilli(),
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return Artifact{}, err
	}
	if err := s.opts.Sessions.AppendEvent(req.SessionID, eventlog.Event{Kind: EventKind, Payload: payload}); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// ServeHTTP serves the MCP bridge. A failure is {"error": "..."}; the text
// starts with a machine-readable code when the caller can fix it.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	want, got := "Bearer "+s.opts.Token, r.Header.Get("Authorization")
	if s.opts.Token == "" || len(want) != len(got) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if strings.TrimPrefix(r.URL.Path, Path) != "publish" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown artifacts operation"})
		return
	}
	var req PublishRequest
	// Content is capped by the asset store; leave headroom for JSON escaping.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*assets.MaxAssetBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid: request body is not valid JSON or is too large"})
		return
	}
	log := slog.With("session_id", req.SessionID, "type", req.Type, "bytes", len(req.Content), "from_file", req.SourcePath != "")
	a, err := s.Publish(req)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.HasPrefix(err.Error(), "invalid: ") {
			status = http.StatusBadRequest
		}
		log.Warn("artifact publish failed", "error", err)
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	log.Info("artifact published", "artifact_id", a.ArtifactID, "title", a.Title)
	writeJSON(w, http.StatusOK, a)
}

func cleanTitle(title, sourcePath string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" && sourcePath != "" {
		title = path.Base(strings.ReplaceAll(sourcePath, "\\", "/"))
	}
	if title == "" {
		title = "Untitled artifact"
	}
	if utf8.RuneCountInString(title) > maxTitleRunes {
		title = string([]rune(title)[:maxTitleRunes])
	}
	return title
}

// downloadName picks a safe single-component filename ending in .html.
func downloadName(filename, sourcePath, title string) string {
	name := filename
	if name == "" && sourcePath != "" {
		name = path.Base(strings.ReplaceAll(sourcePath, "\\", "/"))
	}
	if name == "" {
		name = title
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r):
			return '-'
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.Trim(name, ". -")
	if utf8.RuneCountInString(name) > 120 {
		name = string([]rune(name)[:120])
	}
	if name == "" {
		name = "artifact"
	}
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".html") && !strings.HasSuffix(lower, ".htm") {
		name += ".html"
	}
	return name
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
