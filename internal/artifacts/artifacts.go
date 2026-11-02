// Package artifacts publishes rendered artifacts (docs/artifacts.md) for the
// tandem-artifacts MCP server (internal/artifactsmcp). Every artifact is a
// line of revisions in a git repository (Repo); each publish or update
// commits a revision and appends an "artifact" event naming that exact
// revision to the publishing session's transcript, so the UI previews it
// inline and other transcripts keep showing the revisions they announced.
package artifacts

import (
	"bytes"
	"context"
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

// EventKind is the transcript event announcing a published revision.
const EventKind = "artifact"

// TypeHTML is the only artifact type so far: a free-form HTML document.
const TypeHTML = "html"

// MIMEType is the stored content type of HTML artifacts.
const MIMEType = "text/html"

// MaxBytes caps one revision's document.
const MaxBytes = assets.MaxAssetBytes

const maxTitleRunes = 200

// Sessions appends to live sessions' transcripts and names sessions.
type Sessions interface {
	AppendEvent(sessionID string, ev eventlog.Event) error
}

// Options configures a Service.
type Options struct {
	Token    string
	Repo     *Repo
	Sessions Sessions
	// SessionName names a host-local session for listings ("" if unknown).
	SessionName func(sessionID string) string
	Now         func() time.Time
}

// Service publishes, lists, and reads artifacts, and serves the MCP bridge
// under Path.
type Service struct{ opts Options }

// New returns a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SessionName == nil {
		opts.SessionName = func(string) string { return "" }
	}
	return &Service{opts: opts}
}

// PublishRequest is one artifact_publish call, after the MCP server has read
// any file argument into Content.
type PublishRequest struct {
	SessionID string `json:"sessionId"`
	// ArtifactID, when set, publishes a new revision of that artifact
	// instead of creating one. BaseRevision optionally pins the revision the
	// update was built on, so a concurrent update is refused, not lost.
	ArtifactID   string `json:"artifactId"`
	BaseRevision string `json:"baseRevision"`
	Type         string `json:"type"`
	Title        string `json:"title"`
	Filename     string `json:"filename"`
	Content      string `json:"content"`
	// SourcePath is the file the content was read from, if any. It is shown
	// for provenance only; the daemon never reads it.
	SourcePath string `json:"sourcePath"`
}

func invalid(msg string) error { return errors.New("invalid: " + msg) }

// ErrUnavailable reports a host whose revision repository could not be opened.
var ErrUnavailable = errors.New("artifacts are unavailable on this host (is git installed?)")

// Publish commits a revision and announces it on the session's transcript.
func (s *Service) Publish(ctx context.Context, req PublishRequest) (Revision, error) {
	if s.opts.Repo == nil {
		return Revision{}, ErrUnavailable
	}
	if req.SessionID == "" {
		return Revision{}, invalid("sessionId is required")
	}
	if req.Type == "" {
		req.Type = TypeHTML
	}
	if req.Type != TypeHTML {
		return Revision{}, invalid("unsupported artifact type " + req.Type + " (supported: html)")
	}
	content := []byte(req.Content)
	switch {
	case int64(len(content)) > MaxBytes:
		return Revision{}, invalid((&assets.TooLargeError{Limit: MaxBytes}).Error())
	case len(bytes.TrimSpace(content)) == 0:
		return Revision{}, invalid("HTML artifact is empty")
	case !utf8.Valid(content):
		return Revision{}, invalid("HTML artifact must be UTF-8 text")
	}
	if req.BaseRevision != "" && req.ArtifactID == "" {
		return Revision{}, invalid("baseRevision needs artifactId")
	}
	var previous *Revision
	id := req.ArtifactID
	if id == "" {
		id = NewID()
	} else {
		if !ValidID(id) {
			return Revision{}, invalid("unknown artifactId " + id + "; use artifact_list to find artifacts")
		}
		_, latest, err := s.opts.Repo.Content(ctx, id, "")
		if errors.Is(err, ErrNotFound) {
			return Revision{}, invalid("unknown artifactId " + id + "; use artifact_list to find artifacts")
		}
		if err != nil {
			return Revision{}, err
		}
		previous = &latest
	}
	// An update keeps the artifact's title and download name unless given.
	title, filename := req.Title, req.Filename
	if previous != nil {
		if strings.TrimSpace(title) == "" {
			title = previous.Title
		}
		if strings.TrimSpace(filename) == "" {
			filename = previous.Filename
		}
	}
	title = cleanTitle(title, req.SourcePath)
	rev, err := s.opts.Repo.Commit(ctx, req.BaseRevision, content, Revision{
		ArtifactID: id, Type: req.Type, Title: title, Filename: downloadName(filename, req.SourcePath, title),
		MIMEType: MIMEType, SessionID: req.SessionID, SourcePath: req.SourcePath, CreatedAt: s.opts.Now().UnixMilli(),
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return Revision{}, errors.New("conflict: " + strings.TrimPrefix(err.Error(), ErrConflict.Error()+": ") + "; read the latest revision and apply your change to it")
		}
		return Revision{}, err
	}
	rev.Kind = EventKind
	payload, err := json.Marshal(rev)
	if err != nil {
		return Revision{}, err
	}
	if err := s.opts.Sessions.AppendEvent(req.SessionID, eventlog.Event{Kind: EventKind, Payload: payload}); err != nil {
		// The revision is committed either way; it just was not announced.
		slog.Warn("artifact revision committed but not announced", "session_id", req.SessionID, "artifact_id", rev.ArtifactID, "revision", rev.Revision, "error", err)
		return Revision{}, err
	}
	return rev, nil
}

// listed names the revision's publishing session, as listed to browsers and
// agents.
func (s *Service) listed(rev Revision) Revision {
	rev.SessionName = s.opts.SessionName(rev.SessionID)
	return rev
}

// Gallery lists the latest revision of every artifact on this host, newest
// first, optionally filtered by a case-insensitive substring of the title,
// id, or session name.
func (s *Service) Gallery(ctx context.Context, query string, limit int) ([]Revision, error) {
	if s.opts.Repo == nil {
		return nil, ErrUnavailable
	}
	revs, err := s.opts.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	out := make([]Revision, 0, len(revs))
	for _, rev := range revs {
		item := s.listed(rev)
		if query != "" && !strings.Contains(strings.ToLower(item.Title+"\x00"+item.ArtifactID+"\x00"+item.SessionName+"\x00"+item.Filename), query) {
			continue
		}
		out = append(out, item)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// Revisions lists one artifact's revisions, newest first.
func (s *Service) Revisions(ctx context.Context, artifactID string) ([]Revision, error) {
	if s.opts.Repo == nil {
		return nil, ErrUnavailable
	}
	revs, err := s.opts.Repo.Revisions(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	out := make([]Revision, len(revs))
	for i, rev := range revs {
		out[i] = s.listed(rev)
	}
	return out, nil
}

// Content returns one revision ("" = latest) of an artifact.
func (s *Service) Content(ctx context.Context, artifactID, revision string) ([]byte, Revision, error) {
	if s.opts.Repo == nil {
		return nil, Revision{}, ErrUnavailable
	}
	if !ValidID(artifactID) {
		return nil, Revision{}, ErrNotFound
	}
	return s.opts.Repo.Content(ctx, artifactID, revision)
}

// LegacyEvent is an artifact event logged before revisions existed: its
// artifactId is the SHA-256 of the document, stored as a session asset.
type LegacyEvent struct {
	SessionID string
	Payload   []byte
}

// ImportLegacy gives every pre-revision artifact a one-revision history under
// its original ID (the content hash), so it shows in the gallery and agents
// can update it. The transcript events themselves are left alone. Events are
// expected oldest first; an ID already in the repository is skipped.
func (s *Service) ImportLegacy(ctx context.Context, events []LegacyEvent, read func(sessionID, assetID string) ([]byte, error)) {
	if s.opts.Repo == nil {
		return
	}
	imported := 0
	for _, ev := range events {
		var old Revision
		if err := json.Unmarshal(ev.Payload, &old); err != nil || old.Revision != "" || !ValidID(old.ArtifactID) {
			continue
		}
		if head, err := s.opts.Repo.Head(ctx, old.ArtifactID); err != nil || head != "" {
			continue
		}
		data, err := read(ev.SessionID, old.ArtifactID)
		if err != nil {
			slog.Warn("legacy artifact content unavailable; not imported", "session_id", ev.SessionID, "artifact_id", old.ArtifactID, "error", err)
			continue
		}
		old.SessionID, old.Kind = ev.SessionID, ""
		if old.Type == "" {
			old.Type = TypeHTML
		}
		if old.MIMEType == "" {
			old.MIMEType = MIMEType
		}
		if _, err := s.opts.Repo.Commit(ctx, "", data, old); err != nil {
			slog.Warn("legacy artifact import failed", "session_id", ev.SessionID, "artifact_id", old.ArtifactID, "error", err)
			continue
		}
		imported++
	}
	if imported > 0 {
		slog.Info("imported legacy artifacts into the revision repository", "artifacts", imported)
	}
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
	op := strings.TrimPrefix(r.URL.Path, Path)
	var req struct {
		PublishRequest
		Revision string `json:"revision"`
		Query    string `json:"query"`
		Limit    int    `json:"limit"`
	}
	// Content is capped by MaxBytes; leave headroom for JSON escaping.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*MaxBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid: request body is not valid JSON or is too large"})
		return
	}
	log := slog.With("op", op, "session_id", req.SessionID, "artifact_id", req.ArtifactID)
	var (
		value any
		err   error
	)
	switch op {
	case "publish":
		log = log.With("bytes", len(req.Content), "from_file", req.SourcePath != "", "base_revision", req.BaseRevision)
		var rev Revision
		rev, err = s.Publish(r.Context(), req.PublishRequest)
		if err == nil {
			log.Info("artifact revision published", "artifact_id", rev.ArtifactID, "revision", rev.Revision, "revision_number", rev.RevisionNumber, "title", rev.Title)
			rev.Kind = ""
			value = rev
		}
	case "list":
		var list []Revision
		list, err = s.Gallery(r.Context(), req.Query, req.Limit)
		value = map[string]any{"artifacts": list}
	case "revisions":
		var list []Revision
		list, err = s.Revisions(r.Context(), req.ArtifactID)
		value = map[string]any{"artifactId": req.ArtifactID, "revisions": list}
	case "read":
		var (
			data []byte
			rev  Revision
		)
		data, rev, err = s.Content(r.Context(), req.ArtifactID, req.Revision)
		value = map[string]any{"revision": s.listed(rev), "content": string(data)}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown artifacts operation"})
		return
	}
	if err != nil {
		status := http.StatusInternalServerError
		msg := err.Error()
		switch {
		case errors.Is(err, ErrNotFound):
			status, msg = http.StatusNotFound, "not_found: no such artifact or revision; use artifact_list / artifact_revisions"
		case strings.HasPrefix(msg, "invalid: "):
			status = http.StatusBadRequest
		case strings.HasPrefix(msg, "conflict: "):
			status = http.StatusConflict
		}
		log.Warn("artifacts request failed", "error", err)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	log.Debug("artifacts request served")
	writeJSON(w, http.StatusOK, value)
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
