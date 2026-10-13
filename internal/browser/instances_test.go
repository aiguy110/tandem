package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestInstanceKeyRoundTrip(t *testing.T) {
	if got := InstanceKey("s1", DefaultBrowserID); got != "s1" {
		t.Fatalf("default key = %q", got)
	}
	key := InstanceKey("s1", "b2")
	if session, id := SplitInstanceKey(key); session != "s1" || id != "b2" {
		t.Fatalf("split %q = %q %q", key, session, id)
	}
	if session, id := SplitInstanceKey("plain"); session != "plain" || id != DefaultBrowserID {
		t.Fatalf("split plain = %q %q", session, id)
	}
	if ValidBrowserID("B2") || ValidBrowserID("../x") || !ValidBrowserID("b12") {
		t.Fatal("browser ID validation is wrong")
	}
}

func TestBrokerAuthorizeGatesProvisioning(t *testing.T) {
	cdp := newRawCDP(t)
	driver := &fakeDriver{cdp: cdp.server.URL, provisions: map[string]int{}, teardowns: map[string]int{}, seeds: map[string]string{}}
	b := NewBroker(driver, BrokerConfig{Authorize: func(key string) error {
		if _, id := SplitInstanceKey(key); id != DefaultBrowserID && id != "b2" {
			return errors.New("not approved")
		}
		return nil
	}})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	if err := b.EnsureProvisioned(context.Background(), InstanceKey("s", "b3")); err == nil {
		t.Fatal("unapproved instance provisioned")
	}
	if driver.count(InstanceKey("s", "b3")) != 0 {
		t.Fatal("driver launched an unapproved instance")
	}
	if err := b.EnsureProvisioned(context.Background(), InstanceKey("s", "b2")); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerSessionStateCoversEveryInstance(t *testing.T) {
	cdp := newRawCDP(t)
	driver := &fakeDriver{cdp: cdp.server.URL, provisions: map[string]int{}, teardowns: map[string]int{}, seeds: map[string]string{}}
	b := NewBroker(driver, BrokerConfig{})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	var mu sync.Mutex
	seen := map[string]BrowserState{}
	off := b.OnSessionState("s", func(id string, state BrowserState) {
		mu.Lock()
		seen[id] = state
		mu.Unlock()
	})
	defer off()
	if err := b.EnsureProvisioned(context.Background(), InstanceKey("s", "b2")); err != nil {
		t.Fatal(err)
	}
	b.Grab(InstanceKey("s", "b2"))
	b.Grab("other")
	mu.Lock()
	defer mu.Unlock()
	if _, ok := seen[DefaultBrowserID]; !ok {
		t.Fatal("default instance state missing from snapshot")
	}
	if got := seen["b2"]; !got.Active || got.ControlOwner != ControlUser {
		t.Fatalf("b2 state = %+v", got)
	}
	if len(seen) != 2 {
		t.Fatalf("listener saw another session's browser: %+v", seen)
	}
}

type fakeInstanceService struct {
	opened []string
}

func (f *fakeInstanceService) ListInstances(string) ([]Instance, error) {
	return []Instance{{ID: DefaultBrowserID, Name: "Default"}}, nil
}
func (f *fakeInstanceService) ListSavedProfiles() ([]SavedProfileInfo, error) {
	return []SavedProfileInfo{{ID: "snap-a", Name: "Work", Compatible: true}, {ID: "snap-old", Name: "Steel", Compatible: false}}, nil
}
func (f *fakeInstanceService) OpenInstance(_ context.Context, _, profileID, name string) (Instance, error) {
	f.opened = append(f.opened, profileID)
	return Instance{ID: "b2", Name: name, SourceProfileID: profileID}, nil
}
func (f *fakeInstanceService) CloseInstance(context.Context, string, string) error { return nil }

func TestInstanceRequestsRequireHumanApproval(t *testing.T) {
	service := &fakeInstanceService{}
	var requested []InstanceRequest
	var resolved []bool
	q := NewInstanceRequests(InstanceRequestOptions{
		Token: "secret", Service: service,
		AgentExists: func(id string) bool { return id == "s" },
		OnRequest:   func(r InstanceRequest) { requested = append(requested, r) },
		OnResolved:  func(_, _ string, granted bool, _ string) { resolved = append(resolved, granted) },
	})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer secret")
		q.ServeHTTP(w, r)
		return w
	}
	if w := do(http.MethodPost, InstanceRequestsPath+"/request", `{"sessionId":"s","source":{"kind":"saved_profile","profileId":"snap-old"},"reason":"r"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("incompatible profile accepted: %d %s", w.Code, w.Body)
	}
	w := do(http.MethodPost, InstanceRequestsPath+"/request", `{"sessionId":"s","source":{"kind":"saved_profile","profileId":"snap-a"},"reason":"review PR"}`)
	var registered struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &registered)
	if registered.RequestID == "" || len(requested) != 1 || requested[0].Source.ProfileName != "Work" {
		t.Fatalf("request not registered: %s %+v", w.Body, requested)
	}
	status := do(http.MethodGet, InstanceRequestsPath+"/status?sessionId=s&requestId="+registered.RequestID, "")
	if !strings.Contains(status.Body.String(), `"done":false`) || len(service.opened) != 0 {
		t.Fatalf("instance opened before approval: %s", status.Body)
	}
	if _, err := q.Resolve(context.Background(), "s", registered.RequestID, true, "snap-other"); err == nil {
		t.Fatal("approval swapped in a different profile")
	}
	if _, err := q.Resolve(context.Background(), "s", registered.RequestID, true, ""); err != nil {
		t.Fatal(err)
	}
	if len(service.opened) != 1 || service.opened[0] != "snap-a" || len(resolved) != 1 || !resolved[0] {
		t.Fatalf("approval did not open requested profile: %+v %+v", service.opened, resolved)
	}
	status = do(http.MethodGet, InstanceRequestsPath+"/status?sessionId=s&requestId="+registered.RequestID, "")
	if !strings.Contains(status.Body.String(), `"id":"b2"`) {
		t.Fatalf("status lacks instance: %s", status.Body)
	}
	if _, err := q.Resolve(context.Background(), "s", registered.RequestID, false, ""); err == nil {
		t.Fatal("resolved request twice")
	}

	w = do(http.MethodPost, InstanceRequestsPath+"/request", `{"sessionId":"s","source":{"kind":"user_choice"},"reason":"any"}`)
	_ = json.Unmarshal(w.Body.Bytes(), &registered)
	if _, err := q.Resolve(context.Background(), "s", registered.RequestID, false, ""); err != nil {
		t.Fatal(err)
	}
	status = do(http.MethodGet, InstanceRequestsPath+"/status?sessionId=s&requestId="+registered.RequestID, "")
	if !strings.Contains(status.Body.String(), `"denied":true`) || len(service.opened) != 1 {
		t.Fatalf("denial opened a browser: %s", status.Body)
	}

	unauthorized := httptest.NewRecorder()
	q.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, InstanceRequestsPath+"?sessionId=s", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d", unauthorized.Code)
	}
}
