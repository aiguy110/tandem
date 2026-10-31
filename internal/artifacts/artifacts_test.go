package artifacts

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/eventlog"
)

type fakeStore struct{ got map[string][]byte }

func (f *fakeStore) PutHTML(sessionID string, data []byte) (assets.Stored, error) {
	if len(data) == 0 {
		return assets.Stored{}, &assets.UnsupportedError{Message: "HTML artifact is empty"}
	}
	f.got[sessionID] = data
	return assets.Stored{AssetID: "abc123", MIMEType: assets.HTMLMIME, Size: int64(len(data))}, nil
}

type fakeSessions struct{ events map[string][]eventlog.Event }

func (f *fakeSessions) AppendEvent(sessionID string, ev eventlog.Event) error {
	if sessionID == "gone" {
		return errors.New("invalid: no such session gone")
	}
	f.events[sessionID] = append(f.events[sessionID], ev)
	return nil
}

func newService() (*Service, *fakeStore, *fakeSessions) {
	st, ss := &fakeStore{got: map[string][]byte{}}, &fakeSessions{events: map[string][]eventlog.Event{}}
	return New(Options{Token: "tok", Store: st, Sessions: ss, Now: func() time.Time { return time.UnixMilli(42) }}), st, ss
}

func post(t *testing.T, s *Service, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path+"publish", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestPublishStoresContentAndAppendsTranscriptEvent(t *testing.T) {
	s, st, ss := newService()
	rec, out := post(t, s, "tok", `{"sessionId":"a-1","title":"  Latency\n report ","content":"<p>hi</p>","sourcePath":"/w/out/report.html"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if string(st.got["a-1"]) != "<p>hi</p>" {
		t.Fatalf("stored=%q", st.got["a-1"])
	}
	if out["artifactId"] != "abc123" || out["title"] != "Latency report" || out["filename"] != "report.html" || out["type"] != "html" {
		t.Fatalf("out=%v", out)
	}
	evs := ss.events["a-1"]
	if len(evs) != 1 || evs[0].Kind != EventKind {
		t.Fatalf("events=%v", evs)
	}
	var payload Artifact
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil || payload.Kind != EventKind || payload.ArtifactID != "abc123" || payload.CreatedAt != 42 {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
}

func TestPublishRejectsBadRequests(t *testing.T) {
	s, _, _ := newService()
	if rec, _ := post(t, s, "wrong", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status=%d", rec.Code)
	}
	for _, body := range []string{`{"sessionId":"a-1","content":""}`, `{"sessionId":"a-1","type":"pdf","content":"x"}`, `{"content":"x"}`, `{"sessionId":"gone","content":"x"}`, `not json`} {
		rec, out := post(t, s, "tok", body)
		if rec.Code != http.StatusBadRequest || !strings.HasPrefix(out["error"].(string), "invalid: ") {
			t.Fatalf("%s: status=%d out=%v", body, rec.Code, out)
		}
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
