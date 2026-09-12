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
	hostID, handled, err := master.HandleNotificationAction(context.Background(), notification.ID, "accept")
	if err != nil || !handled || hostID == "" {
		t.Fatalf("accept=%q handled=%v err=%v", hostID, handled, err)
	}
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
