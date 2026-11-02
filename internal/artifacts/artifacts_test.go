package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
)

type fakeSessions struct{ events map[string][]eventlog.Event }

func (f *fakeSessions) AppendEvent(sessionID string, ev eventlog.Event) error {
	if sessionID == "gone" {
		return errors.New("invalid: no such session gone")
	}
	f.events[sessionID] = append(f.events[sessionID], ev)
	return nil
}

func newService(t *testing.T) (*Service, *fakeSessions, *int64) {
	t.Helper()
	repo, err := OpenRepo(filepath.Join(t.TempDir(), "artifacts.git"))
	if err != nil {
		t.Fatal(err)
	}
	now := int64(42_000)
	ss := &fakeSessions{events: map[string][]eventlog.Event{}}
	s := New(Options{Token: "tok", Repo: repo, Sessions: ss, SessionName: func(id string) string { return "name-" + id },
		Now: func() time.Time { now += 1000; return time.UnixMilli(now) }})
	return s, ss, &now
}

func call(t *testing.T, s *Service, token, op, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path+op, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func eventRevision(t *testing.T, ev eventlog.Event) Revision {
	t.Helper()
	var rev Revision
	if err := json.Unmarshal(ev.Payload, &rev); err != nil || rev.Kind != EventKind {
		t.Fatalf("payload=%s err=%v", ev.Payload, err)
	}
	return rev
}

func TestPublishCommitsRevisionAndAppendsTranscriptEvent(t *testing.T) {
	s, ss, _ := newService(t)
	rec, out := call(t, s, "tok", "publish", `{"sessionId":"a-1","title":"  Latency\n report ","content":"<p>hi</p>","sourcePath":"/w/out/report.html"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	id, _ := out["artifactId"].(string)
	if !strings.HasPrefix(id, "art-") || out["title"] != "Latency report" || out["filename"] != "report.html" || out["revisionNumber"] != float64(1) || !ValidRevision(out["revision"].(string)) {
		t.Fatalf("out=%v", out)
	}
	evs := ss.events["a-1"]
	if len(evs) != 1 || evs[0].Kind != EventKind {
		t.Fatalf("events=%v", evs)
	}
	rev := eventRevision(t, evs[0])
	if rev.ArtifactID != id || rev.Revision != out["revision"] || rev.SessionID != "a-1" || rev.CreatedAt != 43_000 || rev.Size != 9 || len(rev.ContentSHA256) != 64 {
		t.Fatalf("payload=%#v", rev)
	}
	data, meta, err := s.Content(context.Background(), id, rev.Revision)
	if err != nil || string(data) != "<p>hi</p>" || meta.Title != "Latency report" {
		t.Fatalf("content=%q meta=%#v err=%v", data, meta, err)
	}
}

func TestUpdateAddsRevisionAnnouncedOnlyInUpdatingSession(t *testing.T) {
	s, ss, _ := newService(t)
	ctx := context.Background()
	first, err := s.Publish(ctx, PublishRequest{SessionID: "a-1", Title: "Plan", Filename: "plan.html", Content: "<p>v1</p>"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Publish(ctx, PublishRequest{SessionID: "b-2", ArtifactID: first.ArtifactID, BaseRevision: first.Revision, Content: "<p>v2</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ArtifactID != first.ArtifactID || second.RevisionNumber != 2 || second.Title != "Plan" || second.Filename != "plan.html" || second.Revision == first.Revision {
		t.Fatalf("second=%#v", second)
	}
	if len(ss.events["a-1"]) != 1 || len(ss.events["b-2"]) != 1 || eventRevision(t, ss.events["a-1"][0]).Revision != first.Revision {
		t.Fatalf("events=%v", ss.events)
	}
	// A stale base is refused instead of silently overwriting the newer revision.
	if _, err := s.Publish(ctx, PublishRequest{SessionID: "a-1", ArtifactID: first.ArtifactID, BaseRevision: first.Revision, Content: "<p>v2b</p>"}); err == nil || !strings.HasPrefix(err.Error(), "conflict: ") {
		t.Fatalf("stale base err=%v", err)
	}
	if _, err := s.Publish(ctx, PublishRequest{SessionID: "a-1", ArtifactID: "art-missing", Content: "x"}); err == nil || !strings.HasPrefix(err.Error(), "invalid: unknown artifactId") {
		t.Fatalf("missing err=%v", err)
	}

	revs, err := s.Revisions(ctx, first.ArtifactID)
	if err != nil || len(revs) != 2 || revs[0].Revision != second.Revision || revs[1].Revision != first.Revision || revs[0].SessionName != "name-b-2" {
		t.Fatalf("revisions=%#v err=%v", revs, err)
	}
	old, meta, err := s.Content(ctx, first.ArtifactID, first.Revision)
	if err != nil || string(old) != "<p>v1</p>" || meta.RevisionNumber != 1 {
		t.Fatalf("old=%q meta=%#v err=%v", old, meta, err)
	}
	latest, _, err := s.Content(ctx, first.ArtifactID, "")
	if err != nil || string(latest) != "<p>v2</p>" {
		t.Fatalf("latest=%q err=%v", latest, err)
	}

	other, err := s.Publish(ctx, PublishRequest{SessionID: "a-1", Title: "Other", Content: "<p>o</p>"})
	if err != nil {
		t.Fatal(err)
	}
	// A revision of another artifact is not readable through this one.
	if _, _, err := s.Content(ctx, first.ArtifactID, other.Revision); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-artifact read err=%v", err)
	}
	gallery, err := s.Gallery(ctx, "", 0)
	if err != nil || len(gallery) != 2 || gallery[0].ArtifactID != other.ArtifactID || gallery[1].Revision != second.Revision {
		t.Fatalf("gallery=%#v err=%v", gallery, err)
	}
	if found, _ := s.Gallery(ctx, "PLAN", 0); len(found) != 1 || found[0].ArtifactID != first.ArtifactID {
		t.Fatalf("query=%#v", found)
	}
}

func TestBridgeListReadRevisions(t *testing.T) {
	s, _, _ := newService(t)
	_, pub := call(t, s, "tok", "publish", `{"sessionId":"a-1","title":"Doc","content":"<p>1</p>"}`)
	id := pub["artifactId"].(string)
	call(t, s, "tok", "publish", `{"sessionId":"a-1","artifactId":"`+id+`","content":"<p>2</p>"}`)
	if rec, out := call(t, s, "tok", "list", `{}`); rec.Code != 200 || len(out["artifacts"].([]any)) != 1 {
		t.Fatalf("list=%d %v", rec.Code, out)
	}
	if rec, out := call(t, s, "tok", "read", `{"artifactId":"`+id+`","revision":"`+pub["revision"].(string)+`"}`); rec.Code != 200 || out["content"] != "<p>1</p>" {
		t.Fatalf("read=%d %v", rec.Code, out)
	}
	if rec, out := call(t, s, "tok", "revisions", `{"artifactId":"`+id+`"}`); rec.Code != 200 || len(out["revisions"].([]any)) != 2 {
		t.Fatalf("revisions=%d %v", rec.Code, out)
	}
	if rec, out := call(t, s, "tok", "read", `{"artifactId":"nope"}`); rec.Code != http.StatusNotFound || !strings.HasPrefix(out["error"].(string), "not_found") {
		t.Fatalf("missing read=%d %v", rec.Code, out)
	}
}

func TestPublishRejectsBadRequests(t *testing.T) {
	s, _, _ := newService(t)
	if rec, _ := call(t, s, "wrong", "publish", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status=%d", rec.Code)
	}
	for _, body := range []string{`{"sessionId":"a-1","content":""}`, `{"sessionId":"a-1","type":"pdf","content":"x"}`, `{"content":"x"}`, `{"sessionId":"gone","content":"x"}`, `{"sessionId":"a-1","content":"x","baseRevision":"abc"}`, `not json`} {
		rec, out := call(t, s, "tok", "publish", body)
		if rec.Code != http.StatusBadRequest || !strings.HasPrefix(out["error"].(string), "invalid: ") {
			t.Fatalf("%s: status=%d out=%v", body, rec.Code, out)
		}
	}
}

func TestImportLegacyGivesOldArtifactsOneRevision(t *testing.T) {
	s, _, _ := newService(t)
	ctx := context.Background()
	sha := strings.Repeat("ab", 32)
	payload := `{"kind":"artifact","artifactId":"` + sha + `","type":"html","title":"Old","filename":"old.html","mimeType":"text/html","size":8,"createdAt":5000}`
	events := []LegacyEvent{{SessionID: "s1", Payload: []byte(payload)}, {SessionID: "s2", Payload: []byte(payload)}}
	read := func(sessionID, assetID string) ([]byte, error) { return []byte("<p>old</p>"), nil }
	s.ImportLegacy(ctx, events, read)
	s.ImportLegacy(ctx, events, read) // idempotent
	revs, err := s.Revisions(ctx, sha)
	if err != nil || len(revs) != 1 || revs[0].SessionID != "s1" || revs[0].Title != "Old" || revs[0].CreatedAt != 5000 {
		t.Fatalf("revs=%#v err=%v", revs, err)
	}
	updated, err := s.Publish(ctx, PublishRequest{SessionID: "s3", ArtifactID: sha, Content: "<p>new</p>"})
	if err != nil || updated.RevisionNumber != 2 {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
}

func TestDownloadName(t *testing.T) {
	for _, tc := range []struct{ filename, source, title, want string }{
		{"", "", "Q3 report", "Q3 report.html"},
		{"../../etc/passwd", "", "t", "etc-passwd.html"},
		{"page.HTM", "", "t", "page.HTM"},
		{"", "/w/a/b.html", "t", "b.html"},
		{"", "", "...", "artifact.html"},
	} {
		if got := downloadName(tc.filename, tc.source, tc.title); got != tc.want {
			t.Errorf("downloadName(%q,%q,%q)=%q want %q", tc.filename, tc.source, tc.title, got, tc.want)
		}
	}
}
