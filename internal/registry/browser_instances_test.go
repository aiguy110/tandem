package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aiguy110/tandem/internal/browser"
)

type instanceDriver struct {
	mu      sync.Mutex
	cdp     string
	running map[string]bool
}

func (d *instanceDriver) Kind() string   { return "local" }
func (d *instanceDriver) PID(string) int { return 0 }
func (d *instanceDriver) Provision(_ context.Context, id string) (browser.ProvisionResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.running[id] = true
	return browser.ProvisionResult{CDPURL: d.cdp}, nil
}
func (d *instanceDriver) Teardown(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, id)
	return nil
}
func (d *instanceDriver) IsProvisioned(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[id]
}

func TestBrowserInstancesOpenPersistAndClose(t *testing.T) {
	cdp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/x"}`))
	}))
	defer cdp.Close()
	r, _, _ := setup(t, &fakeFactory{})
	driver := &instanceDriver{cdp: cdp.URL, running: map[string]bool{}}
	r.browser = browser.NewBroker(driver, browser.BrokerConfig{Authorize: r.AuthorizeBrowserKey})
	if err := r.browser.Start(); err != nil {
		t.Fatal(err)
	}
	defer r.browser.Stop(context.Background())
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AuthorizeBrowserKey(browser.InstanceKey(s.ID, "b2")); err == nil {
		t.Fatal("unapproved instance authorized")
	}
	opened, err := r.OpenInstance(context.Background(), s.ID, "", "Docs")
	if err != nil {
		t.Fatal(err)
	}
	if opened.ID != "b2" || opened.Name != "Docs" || !driver.IsProvisioned(browser.InstanceKey(s.ID, "b2")) {
		t.Fatalf("opened = %+v running=%v", opened, driver.running)
	}
	second, err := r.OpenInstance(context.Background(), s.ID, "", "Docs")
	if err != nil || second.ID != "b3" || second.Name != "Docs (2)" {
		t.Fatalf("second = %+v, %v", second, err)
	}
	// A restart reloads the list from the durable event log.
	r.instMu.Lock()
	r.browserInstances = map[string]*browserInstanceList{}
	r.instMu.Unlock()
	instances, err := r.ListInstances(s.ID)
	if err != nil || len(instances) != 3 || instances[0].ID != browser.DefaultBrowserID || instances[2].ID != "b3" {
		t.Fatalf("instances = %+v, %v", instances, err)
	}
	if err := r.CloseInstance(context.Background(), s.ID, browser.DefaultBrowserID); err == nil {
		t.Fatal("closed the default browser")
	}
	if err := r.CloseInstance(context.Background(), s.ID, "b2"); err != nil {
		t.Fatal(err)
	}
	if r.HasBrowserInstance(s.ID, "b2") || driver.IsProvisioned(browser.InstanceKey(s.ID, "b2")) {
		t.Fatal("closed instance still present")
	}
	third, err := r.OpenInstance(context.Background(), s.ID, "", "")
	if err != nil || third.ID != "b4" || third.Name != "Fresh browser" {
		t.Fatalf("third = %+v, %v (IDs must not be reused)", third, err)
	}
	if _, err := r.Close(context.Background(), s.ID, true, false, false); err != nil {
		t.Fatal(err)
	}
	if driver.IsProvisioned(browser.InstanceKey(s.ID, "b3")) || driver.IsProvisioned(browser.InstanceKey(s.ID, "b4")) {
		t.Fatalf("closing the session left instances running: %v", driver.running)
	}
}
