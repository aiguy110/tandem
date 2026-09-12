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

// Version skew used to surface only as commands that quietly failed on the
// older side, so both peers now report their versions on every connection and
// say so out loud when they disagree.
func TestProtocolVersionExchangeAndSkewNotice(t *testing.T) {
	masterStore := openStore(t)
	masterCenter := notifications.New()
	master, err := New(Options{Store: masterStore, Notifications: masterCenter, BuildVersion: "v9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(master)
	defer server.Close()
	slaveStore := openStore(t)
	slaveCenter := notifications.New()
	slave, err := New(Options{
		Store: slaveStore, Notifications: slaveCenter, MasterURL: server.URL, Name: "build-host",
		Local: testLocal{}, PollInterval: 10 * time.Millisecond, BuildVersion: "v9.9.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- slave.RunSlave(ctx) }()

	hostID := ""
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && hostID == "" {
		for _, item := range masterCenter.List() {
			if strings.HasPrefix(item.ID, "federation-registration-") {
				hostID = strings.TrimPrefix(item.ID, "federation-registration-")
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if hostID == "" {
		t.Fatal("registration notification was not published")
	}
	if _, _, err = master.HandleNotificationAction(context.Background(), "federation-registration-"+hostID, "accept"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	var peer *store.FederationSlave
	for time.Now().Before(deadline) {
		peer, _ = masterStore.FederationSlave(hostID)
		if peer != nil && peer.Status == "connected" && peer.ProtocolVersion != 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if peer == nil || peer.ProtocolVersion != ProtocolVersion || peer.BuildVersion != "v9.9.9" {
		t.Fatalf("recorded host versions = %#v", peer)
	}
	hosts := master.Hosts()
	if len(hosts) != 1 || hosts[0].ProtocolVersion != ProtocolVersion || hosts[0].BuildVersion != "v9.9.9" {
		t.Fatalf("hosts = %#v", hosts)
	}
	// Matching versions must stay silent on both sides.
	for _, item := range append(masterCenter.List(), slaveCenter.List()...) {
		if strings.Contains(item.ID, "protocol") {
			t.Fatalf("unexpected skew notice for matched versions: %#v", item)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A host one version off is reported, not refused, and the notice can be
	// dismissed.
	skewed := *peer
	skewed.ProtocolVersion = ProtocolVersion + 1
	skewed.BuildVersion = "v10.0.0"
	master.checkProtocol(skewed)
	notice := notifications.Notification{}
	for _, item := range masterCenter.List() {
		if item.ID == protocolNotificationPrefix+hostID {
			notice = item
		}
	}
	if !strings.Contains(notice.Title, "build-host") || !strings.Contains(notice.Message, "v10.0.0") {
		t.Fatalf("skew notice = %#v", notice)
	}
	if _, handled, err := master.HandleNotificationAction(context.Background(), notice.ID, "dismiss"); err != nil || !handled {
		t.Fatalf("dismiss handled=%v err=%v", handled, err)
	}
	for _, item := range masterCenter.List() {
		if item.ID == notice.ID {
			t.Fatal("dismissed skew notice is still listed")
		}
	}

	// The host's own UI reports the same fact about its master.
	slave.noteMasterProtocol(ProtocolVersion+1, "v10.0.0")
	if items := slaveCenter.List(); len(items) != 1 || items[0].ID != masterProtocolNotificationID {
		t.Fatalf("slave-side notices = %#v", slaveCenter.List())
	}
	if _, handled, err := slave.HandleNotificationAction(context.Background(), masterProtocolNotificationID, "dismiss"); err != nil || !handled {
		t.Fatalf("slave dismiss handled=%v err=%v", handled, err)
	}
	if items := slaveCenter.List(); len(items) != 0 {
		t.Fatalf("slave notices after dismiss = %#v", items)
	}
}

func TestHostIDsAreShortAndReadable(t *testing.T) {
	id, err := newHostID("Boremox.local")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "boremox-local-") || len(id) != len("boremox-local-")+6 {
		t.Fatalf("newHostID = %q", id)
	}
	if !ValidHostID(id) {
		t.Fatalf("generated host ID %q rejected", id)
	}
	unnamed, err := newHostID("  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(unnamed, "host-") {
		t.Fatalf("unnamed host ID = %q", unnamed)
	}
	for _, bad := range []string{"", "has~separator", "has/slash", "has space", strings.Repeat("a", 65)} {
		if ValidHostID(bad) {
			t.Fatalf("ValidHostID(%q) = true", bad)
		}
	}
	if !legacyHostID(strings.Repeat("ab", 24)) || legacyHostID(id) {
		t.Fatal("legacyHostID misclassified")
	}
}
