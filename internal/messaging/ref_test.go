package messaging

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

// namedFed adds operator host-name overrides (federation.Service.HostNames)
// to the test federation.
type namedFed struct {
	*testFed
	names map[string]string
}

func (f *namedFed) HostNames() map[string]string { return f.names }

func nameHosts(h *testHost, net *testNet, names map[string]string) {
	h.svc.opts.Federation = &namedFed{testFed: &testFed{net: net, self: h.id}, names: names}
}

func TestRefFormatting(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	newTestHost(t, "hostB", hostOpts{net: net})
	newTestHost(t, "hostC", hostOpts{net: net})

	cases := []struct {
		name string
		addr Address
		want string
	}{
		{"local host and agent names", Address{Host: "hostA", Agent: "s1", Name: "api-worker"}, "@agent:hostA/api-worker"},
		{"remote host display name", Address{Host: "hostB", Agent: "s2", Name: "Slow.Drag_2"}, "@agent:HOSTB/Slow.Drag_2"},
		{"agent name with spaces falls back to session ID", Address{Host: "hostB", Agent: "s2", Name: "slow drag"}, "@agent:HOSTB/s2"},
		{"empty agent name", Address{Host: "hostB", Agent: "s2"}, "@agent:HOSTB/s2"},
		{"agent name must start alphanumeric", Address{Host: "hostB", Agent: "s2", Name: "-x"}, "@agent:HOSTB/s2"},
		{"agent name with dots and dashes", Address{Host: "hostB", Agent: "s2", Name: "a.b-c_d"}, "@agent:HOSTB/a.b-c_d"},
		{"unknown host uses node ID", Address{Host: "hostZ", Agent: "s9", Name: "x"}, "@agent:hostZ/x"},
	}
	for _, c := range cases {
		if got := a.svc.Ref(c.addr); got != c.want {
			t.Errorf("%s: Ref = %q, want %q", c.name, got, c.want)
		}
	}

	// A host name that is not a valid token falls back to the node ID.
	a.svc.opts.LocalName = "My Laptop"
	if got := a.svc.Ref(Address{Host: "hostA", Agent: "s1", Name: "x"}); got != "@agent:hostA/x" {
		t.Errorf("local name with spaces: %q", got)
	}
	// Operator renames of remote hosts (keyed by the route ID) win, and the
	// local host is renamed under "local" like the browser host list.
	nameHosts(a, net, map[string]string{"hostB": "bifrost", "hostC": "big box", "local": "home-base"})
	for addr, want := range map[Address]string{
		{Host: "hostB", Agent: "s2", Name: "api-worker"}: "@agent:bifrost/api-worker",
		{Host: "hostC", Agent: "s3", Name: "x"}:          "@agent:hostC/x",
		{Host: "hostA", Agent: "s1", Name: "x"}:          "@agent:home-base/x",
	} {
		if got := a.svc.Ref(addr); got != want {
			t.Errorf("renamed %v: Ref = %q, want %q", addr, got, want)
		}
	}
	// Standalone daemon: no federation. The local host is not renamed, so its
	// node ID is used, and its configured name still resolves as an alias.
	solo := newTestHost(t, "solo", hostOpts{})
	if got := solo.svc.Ref(Address{Host: "solo", Agent: "s", Name: "n"}); got != "@agent:solo/n" {
		t.Errorf("standalone: %q", got)
	}
}

func TestResolveAgentReference(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	c := newTestHost(t, "hostC", hostOpts{net: net, clock: a.clock})
	alice, _ := a.addSession("alice", "alice")
	b.addSession("bob", "api-worker")
	b.addSession("w1", "worker")
	b.addSession("w2", "Worker")
	b.addSession("sp", "slow drag")
	b.addSession("hid", "hidden-one")
	c.addSession("carl", "api-worker")
	if err := b.svc.SetListed("hid", false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resolve := func(to string) (Address, error) { return a.svc.resolve(ctx, alice, to) }

	ok := map[string]Address{
		"@agent:HOSTB/api-worker":   {Host: "hostB", Agent: "bob"},
		"@AGENT:hostb/API-WORKER":   {Host: "hostB", Agent: "bob"},
		"agent:HOSTB/api-worker":    {Host: "hostB", Agent: "bob"},
		"@agent:hostB/api-worker":   {Host: "hostB", Agent: "bob"}, // node ID as host
		"@agent:hostB/bob":          {Host: "hostB", Agent: "bob"}, // session ID as agent
		"@agent:hostB/BOB":          {Host: "hostB", Agent: "bob"},
		"  @agent:HOSTC/api-worker": {Host: "hostC", Agent: "carl"},
		"@agent:hostB/sp":           {Host: "hostB", Agent: "sp"}, // name with spaces: use the session ID
		// Legacy forms.
		"hostB~bob":         {Host: "hostB", Agent: "bob"},
		"@api-worker@hostB": {Host: "hostB", Agent: "bob"},
		"api-worker@HOSTC":  {Host: "hostC", Agent: "carl"},
	}
	for to, want := range ok {
		got, err := resolve(to)
		if err != nil || got.Host != want.Host || got.Agent != want.Agent {
			t.Errorf("resolve(%q) = %+v, %v; want %+v", to, got, err, want)
		}
	}
	// Legacy bare names are still resolved, and an ambiguous one lists refs only.
	if got, err := resolve("slow drag"); err != nil || got.Agent != "sp" {
		t.Errorf("legacy bare name with spaces: %+v, %v", got, err)
	}
	_, err := resolve("api-worker")
	if rejectionCode(err) != ErrInvalid || !strings.Contains(err.Error(), "@agent:HOSTB/api-worker") || !strings.Contains(err.Error(), "@agent:HOSTC/api-worker") || strings.Contains(err.Error(), "~") {
		t.Errorf("ambiguous legacy name: %v", err)
	}

	// Two agents on one host share a (case-insensitive) name: candidates are
	// listed by session ID so each reference is usable.
	_, err = resolve("@agent:hostB/worker")
	if rejectionCode(err) != ErrInvalid || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "@agent:HOSTB/w1") || !strings.Contains(err.Error(), "@agent:HOSTB/w2") {
		t.Errorf("ambiguous agent: %v", err)
	}
	// Not found, unlisted, unknown host and malformed references.
	for _, to := range []string{"@agent:hostB/nope", "@agent:hostB/hid", "@agent:hostB/hidden-one"} {
		if _, err := resolve(to); rejectionCode(err) != ErrInvalid || !strings.Contains(err.Error(), "messages_directory") {
			t.Errorf("resolve(%q) = %v", to, err)
		}
	}
	for _, to := range []string{"@agent:nowhere/bob", "@agent:hostB/", "@agent:/bob", "@agent:HOSTB"} {
		if _, err := resolve(to); rejectionCode(err) != ErrInvalid {
			t.Errorf("resolve(%q) = %v", to, err)
		}
	}

	// Two hosts that share a display name are ambiguous: the error lists node IDs.
	nameHosts(a, net, map[string]string{"hostB": "twin", "hostC": "twin"})
	_, err = resolve("@agent:twin/api-worker")
	if rejectionCode(err) != ErrInvalid || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "hostB") || !strings.Contains(err.Error(), "hostC") {
		t.Errorf("ambiguous host: %v", err)
	}
	// The node ID still disambiguates, and a rename is honoured.
	if got, err := resolve("@agent:hostC/api-worker"); err != nil || got.Agent != "carl" {
		t.Errorf("node ID with shared names: %+v, %v", got, err)
	}
	nameHosts(a, net, map[string]string{"hostB": "bifrost"})
	if got, err := resolve("@agent:Bifrost/api-worker"); err != nil || got.Agent != "bob" {
		t.Errorf("renamed host: %+v, %v", got, err)
	}
}

func TestDirectoryEntriesCarryRef(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	b.addSession("bob", "api-worker")
	b.addSession("sp", "slow drag")
	nameHosts(a, net, map[string]string{"hostB": "bifrost"})
	entries, err := a.svc.Directory(context.Background(), "alice", "")
	if err != nil || len(entries) != 2 {
		t.Fatalf("directory = %+v, %v", entries, err)
	}
	refs := map[string]string{}
	for _, e := range entries {
		refs[e.Address.Agent] = e.Ref
	}
	// The viewer's name for the host, not the host's own.
	if refs["bob"] != "@agent:bifrost/api-worker" || refs["sp"] != "@agent:bifrost/sp" {
		t.Fatalf("refs = %v", refs)
	}
	// ref is the first JSON field.
	raw, _ := json.Marshal(entries[0])
	if !strings.HasPrefix(string(raw), `{"ref":"@agent:bifrost/`) {
		t.Fatalf("entry JSON = %s", raw)
	}
	// The answering host's own local listing carries its own ref too.
	if local := b.svc.LocalDirectory(nil, ""); len(local) != 2 || !strings.HasPrefix(local[0].Ref, "@agent:hostB/") {
		t.Fatalf("local directory = %+v", local)
	}
}

func TestPromptShowsRecipientsReferenceForSender(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "slow drag")
	_, bobAd := b.addSession("bob", "bob")
	b.link(a.addr("alice"), "bob")
	// hostB calls hostA "slow-host"; alice's own name has spaces, so her
	// session ID is used.
	nameHosts(b, net, map[string]string{"hostA": "slow-host"})
	if _, err := a.svc.Send(context.Background(), "alice", "@agent:hostB/bob", "hello", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob to receive", func() bool { return len(bobAd.promptTexts()) == 1 })
	p := bobAd.promptTexts()[0]
	if !strings.Contains(p, `from="@agent:slow-host/alice" from-address="hostA~alice"`) {
		t.Fatalf("prompt = %s", p)
	}
}

func TestSummaryUnlistedFollowsListing(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	if h.svc.Unlisted("alice") {
		t.Fatal("agents are listed by default")
	}
	before := atomic.LoadInt32(&h.summaryChanges)
	if err := h.svc.SetListed("alice", false); err != nil {
		t.Fatal(err)
	}
	if !h.svc.Unlisted("alice") {
		t.Fatal("alice should be unlisted")
	}
	eventually(t, "summary re-broadcast on listing change", func() bool { return atomic.LoadInt32(&h.summaryChanges) > before })
	if err := h.svc.SetListed("alice", true); err != nil {
		t.Fatal(err)
	}
	if h.svc.Unlisted("alice") {
		t.Fatal("alice should be listed again")
	}
	// A restart reloads the flag from the store.
	_ = h.svc.SetListed("alice", false)
	h.restart(nil)
	if !h.svc.Unlisted("alice") {
		t.Fatal("unlisted flag lost across a restart")
	}
}
