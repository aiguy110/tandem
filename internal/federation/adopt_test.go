package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/store"
)

// adoptionChild serves a child that parents dial.
func adoptionChild(t *testing.T, opts Options) (*Service, *httptest.Server) {
	t.Helper()
	if opts.Store == nil {
		opts.Store = openStore(t)
	}
	if opts.Name == "" {
		opts.Name = "devbox"
	}
	opts.Local, opts.PollInterval = testLocal{}, 10*time.Millisecond
	child, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(child)
	t.Cleanup(server.Close)
	return child, server
}

// runAdoptingParent starts a parent that dials children and returns it with a
// function that stops its links and waits for them to end.
func runAdoptingParent(t *testing.T, parentStore *store.Store, children ...ChildLink) (*Service, func()) {
	t.Helper()
	parent, err := New(Options{Store: parentStore, Name: "workstation", Children: children, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { parent.RunChildLinks(ctx); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return parent, stop
}

func connectedHost(parent *Service) *Host {
	hosts := parent.Hosts()
	if len(hosts) == 1 && hosts[0].Status == "connected" {
		return &hosts[0]
	}
	return nil
}

func TestParentAdoptsChildWithJoinTokenAndReconnectsWithoutIt(t *testing.T) {
	childStore := openStore(t)
	_, server := adoptionChild(t, Options{Store: childStore, JoinToken: "open-sesame"})
	parentStore := openStore(t)
	parent, stop := runAdoptingParent(t, parentStore, ChildLink{URL: server.URL, Name: "container", JoinToken: "open-sesame"})

	eventuallyTest(t, "adopted child connected", func() bool { return connectedHost(parent) != nil })
	host := connectedHost(parent)
	if host.Name != "container" || host.Endpoint != server.URL || len(host.Snapshot) == 0 || host.Dialer != DialerParent {
		t.Fatalf("adopted host = %#v", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := parent.Call(ctx, host.ID, json.RawMessage(`{"t":"ping"}`))
	if err != nil || string(got) != `{"echo":{"t":"ping"}}` {
		t.Fatalf("call through adopted link = %s, %v", got, err)
	}
	trust, _ := childStore.FederationParent()
	if trust == nil || !trust.Adopted || trust.URL != "" || trust.HostID != host.ID {
		t.Fatalf("child trust = %#v", trust)
	}

	// The credential is now trusted on its own: the token can leave the
	// parent's configuration, and a restarted parent reconnects.
	stop()
	eventuallyTest(t, "adopted child offline", func() bool {
		peer, _ := parentStore.FederationChild(host.ID)
		return peer != nil && peer.Status == "offline"
	})
	again, _ := runAdoptingParent(t, parentStore, ChildLink{URL: server.URL})
	eventuallyTest(t, "adopted child reconnected", func() bool { return connectedHost(again) != nil })
	if id := connectedHost(again).ID; id != host.ID {
		t.Fatalf("reconnected as %q, want %q", id, host.ID)
	}
}

func TestAdoptionWithoutJoinTokenWaitsForChildOperator(t *testing.T) {
	center := notifications.New()
	child, server := adoptionChild(t, Options{Notifications: center, JoinToken: "the-real-token"})
	// A wrong token is no better than none: the child's operator decides.
	parent, _ := runAdoptingParent(t, openStore(t), ChildLink{URL: server.URL, JoinToken: "a-guess"})

	var notification notifications.Notification
	eventuallyTest(t, "adoption notification", func() bool {
		for _, n := range center.List() {
			if strings.HasPrefix(n.ID, adoptionNotificationPrefix) {
				notification = n
				return true
			}
		}
		return false
	})
	if !strings.Contains(notification.Title, "workstation") {
		t.Fatalf("notification = %#v", notification)
	}
	if connectedHost(parent) != nil {
		t.Fatal("parent connected before the child's operator accepted")
	}
	if _, handled, err := child.HandleNotificationAction(context.Background(), notification.ID, "accept"); !handled || err != nil {
		t.Fatalf("accept handled=%v err=%v", handled, err)
	}
	eventuallyTest(t, "adopted child connected after approval", func() bool { return connectedHost(parent) != nil })
	if host := connectedHost(parent); host.Name != "devbox" {
		t.Fatalf("adopted host name = %q, want the child's own", host.Name)
	}
}

func TestRejectedAdoptionIsRefused(t *testing.T) {
	center := notifications.New()
	child, server := adoptionChild(t, Options{Notifications: center})
	credential := strings.Repeat("c", 48)
	dial := func() int {
		req, _ := http.NewRequest(http.MethodGet, server.URL+AdoptPath, nil)
		req.Header.Set("Authorization", "Bearer "+credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := dial(); code != http.StatusAccepted {
		t.Fatalf("first dial status = %d, want pending", code)
	}
	if _, _, err := child.HandleNotificationAction(context.Background(), adoptionNotificationPrefix+adoptionKey(credential), "reject"); err != nil {
		t.Fatal(err)
	}
	if code := dial(); code != http.StatusForbidden {
		t.Fatalf("dial after reject status = %d, want forbidden", code)
	}
	if len(center.List()) != 0 {
		t.Fatalf("notifications after reject = %#v", center.List())
	}
}

func TestHostWithItsOwnParentRefusesAdoption(t *testing.T) {
	_, server := adoptionChild(t, Options{ParentURL: "http://127.0.0.1:1", JoinToken: "tok"})
	req, _ := http.NewRequest(http.MethodGet, server.URL+AdoptPath, nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("c", 48))
	req.Header.Set(joinTokenHeader, "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want conflict", resp.StatusCode)
	}
}

func TestJoinTokenDoesNotDisplaceAConnectedParent(t *testing.T) {
	_, server := adoptionChild(t, Options{JoinToken: "shared"})
	first, _ := runAdoptingParent(t, openStore(t), ChildLink{URL: server.URL, JoinToken: "shared"})
	eventuallyTest(t, "first parent connected", func() bool { return connectedHost(first) != nil })
	req, _ := http.NewRequest(http.MethodGet, server.URL+AdoptPath, nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("d", 48))
	req.Header.Set(joinTokenHeader, "shared")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second parent status = %d, want conflict", resp.StatusCode)
	}
	if connectedHost(first) == nil {
		t.Fatal("first parent lost its child")
	}
}
