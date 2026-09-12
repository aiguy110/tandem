package federation

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/store"
)

type testLocal struct{}

func (testLocal) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"catalog":"local"}`), nil
}
func (testLocal) Execute(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	return append(json.RawMessage(`{"echo":`), append(raw, '}')...), nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRegistrationApprovalDurableTrustAndCommandRelay(t *testing.T) {
	masterStore := openStore(t)
	center := notifications.New()
	master, err := New(Options{Store: masterStore, Notifications: center})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(master)
	defer server.Close()
	slaveStore := openStore(t)
	slave, err := New(Options{Store: slaveStore, MasterURL: server.URL, Name: "build-host", Local: testLocal{}, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- slave.RunSlave(ctx) }()
	var notification notifications.Notification
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := center.List()
		if len(got) > 0 {
			notification = got[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if notification.ID == "" {
		t.Fatal("registration notification was not published")
	}
	agentID, handled, err := master.HandleNotificationAction(context.Background(), notification.ID, "accept")
	if err != nil || !handled || agentID != "" {
		t.Fatalf("accept agent=%q handled=%v err=%v", agentID, handled, err)
	}
	hostID := strings.TrimPrefix(notification.ID, "federation-registration-")
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		peers, _ := masterStore.FederationSlaves()
		if len(peers) == 1 && peers[0].Status == "connected" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	peer, _ := masterStore.FederationSlave(hostID)
	if peer == nil || peer.Status != "connected" || peer.Credential == "" {
		t.Fatalf("peer=%#v", peer)
	}
	stored, _ := slaveStore.FederationMaster()
	if stored == nil || stored.Credential == "" || stored.HostID != hostID {
		t.Fatalf("slave trust=%#v", stored)
	}
	callCtx, callCancel := context.WithTimeout(context.Background(), time.Second)
	defer callCancel()
	got, err := master.Call(callCtx, hostID, json.RawMessage(`{"t":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"echo\":{\"t\":\"ping\"}}" {
		t.Fatalf("reply=%s", got)
	}
	if hosts := master.Hosts(); len(hosts) != 1 || string(hosts[0].Snapshot) != "{\"catalog\":\"local\"}" {
		t.Fatalf("hosts=%#v", hosts)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSlaveRejectsChildRegistration(t *testing.T) {
	s := openStore(t)
	service, err := New(Options{Store: s, MasterURL: "http://upstream.example"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", RegisterPath, strings.NewReader(`{"hostId":"child"}`))
	w := httptest.NewRecorder()
	service.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatalf("status=%d want 409", w.Code)
	}
}

func TestMasterRestartRestoresPendingNoticeAndMarksStaleTunnelOffline(t *testing.T) {
	s := openStore(t)
	if err := s.UpsertFederationSlave(store.FederationSlave{ID: "pending-host", Name: "Pending", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFederationSlave(store.FederationSlave{ID: "stale-host", Name: "Stale", Status: "connected"}); err != nil {
		t.Fatal(err)
	}
	center := notifications.New()
	service, err := New(Options{Store: s, Notifications: center})
	if err != nil {
		t.Fatal(err)
	}
	if notices := center.List(); len(notices) != 1 || notices[0].ID != "federation-registration-pending-host" {
		t.Fatalf("restored notices = %#v", notices)
	}
	hosts := service.Hosts()
	for _, host := range hosts {
		if host.ID == "stale-host" && host.Status != "offline" {
			t.Fatalf("stale host status = %q, want offline", host.Status)
		}
	}
}

// A slave that drops is recorded "offline", so reconnecting with its durable
// credential must still authenticate and must not be demoted back to pending
// approval. Requiring the one-shot "accepted" status here would make every
// reconnection after the first fail as unauthorized.
func TestOfflineSlaveReconnectsWithDurableCredential(t *testing.T) {
	masterStore := openStore(t)
	center := notifications.New()
	master, err := New(Options{Store: masterStore, Notifications: center})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(master)
	defer server.Close()
	if err := masterStore.UpsertFederationSlave(store.FederationSlave{ID: "host-1", Name: "build-host", Credential: "secret-credential", Status: "offline"}); err != nil {
		t.Fatal(err)
	}
	slaveStore := openStore(t)
	if err := slaveStore.SaveFederationMaster(store.FederationMaster{URL: server.URL, HostID: "host-1", Credential: "secret-credential"}); err != nil {
		t.Fatal(err)
	}
	slave, err := New(Options{Store: slaveStore, MasterURL: server.URL, Name: "build-host", Local: testLocal{}, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- slave.RunSlave(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if peer, _ := masterStore.FederationSlave("host-1"); peer != nil && peer.Status == "connected" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	peer, _ := masterStore.FederationSlave("host-1")
	if peer == nil || peer.Status != "connected" {
		t.Fatalf("offline host did not reconnect: %#v", peer)
	}
	if notices := center.List(); len(notices) != 0 {
		t.Fatalf("reconnection re-requested approval: %#v", notices)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// An already-trusted host that re-runs registration must not reset its durable
// record to pending, which would drop its credential and re-prompt the user.
func TestRegisterDoesNotDemoteTrustedHost(t *testing.T) {
	s := openStore(t)
	center := notifications.New()
	service, err := New(Options{Store: s, Notifications: center})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"accepted", "connected", "offline"} {
		if err := s.UpsertFederationSlave(store.FederationSlave{ID: "host-1", Name: "build-host", Credential: "secret-credential", Status: status}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", RegisterPath, strings.NewReader(`{"hostId":"host-1","name":"build-host"}`))
		w := httptest.NewRecorder()
		service.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("status=%q: re-registration code=%d want 401", status, w.Code)
		}
		if strings.Contains(w.Body.String(), "secret-credential") {
			t.Fatalf("status=%q: registration response leaked the credential", status)
		}
		peer, _ := s.FederationSlave("host-1")
		if peer == nil || peer.Status != status || peer.Credential != "secret-credential" {
			t.Fatalf("status=%q: record was demoted to %#v", status, peer)
		}
	}
	if notices := center.List(); len(notices) != 0 {
		t.Fatalf("re-registration re-requested approval: %#v", notices)
	}
}
