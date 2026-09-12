package federation

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
)

// socksRecorder is a minimal no-auth SOCKS5 CONNECT server that records which
// requests were relayed through it, so a test can prove the federation
// transport dialed the proxy rather than the master directly.
type socksRecorder struct {
	listener net.Listener
	mu       sync.Mutex
	paths    []string
}

func startSocksRecorder(t *testing.T) *socksRecorder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socksRecorder{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return s
}

func (s *socksRecorder) addr() string { return s.listener.Addr().String() }

func (s *socksRecorder) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *socksRecorder) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, line)
}

func (s *socksRecorder) serve(client net.Conn) {
	defer client.Close()
	target, err := socksHandshake(client)
	if err != nil {
		return
	}
	defer target.Close()
	go func() { _, _ = io.Copy(client, target) }()
	// Sniff the client's first request line so the test can distinguish the
	// registration calls from the websocket tunnel upgrade.
	head := make([]byte, 4096)
	n, err := client.Read(head)
	if n > 0 {
		if line, _, ok := strings.Cut(string(head[:n]), "\r\n"); ok {
			s.record(line)
		}
		if _, werr := target.Write(head[:n]); werr != nil {
			return
		}
	}
	if err != nil {
		return
	}
	_, _ = io.Copy(target, client)
}

func socksHandshake(client net.Conn) (net.Conn, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(client, head); err != nil {
		return nil, err
	}
	if head[0] != 5 {
		return nil, errors.New("not socks5")
	}
	if _, err := io.ReadFull(client, make([]byte, int(head[1]))); err != nil {
		return nil, err
	}
	if _, err := client.Write([]byte{5, 0}); err != nil { // no authentication
		return nil, err
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(client, request); err != nil {
		return nil, err
	}
	if request[1] != 1 {
		return nil, errors.New("only CONNECT is supported")
	}
	var host string
	switch request[3] {
	case 1:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(client, addr); err != nil {
			return nil, err
		}
		host = net.IP(addr).String()
	case 3:
		size := make([]byte, 1)
		if _, err := io.ReadFull(client, size); err != nil {
			return nil, err
		}
		name := make([]byte, int(size[0]))
		if _, err := io.ReadFull(client, name); err != nil {
			return nil, err
		}
		host = string(name)
	case 4:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(client, addr); err != nil {
			return nil, err
		}
		host = net.IP(addr).String()
	default:
		return nil, errors.New("unsupported address type")
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(client, portBytes); err != nil {
		return nil, err
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(portBytes))))
	if err != nil {
		_, _ = client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return nil, err
	}
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		target.Close()
		return nil, err
	}
	return target, nil
}

// Both halves of the slave transport must honor ProxyURL: the REST
// registration calls go through net/http, while the durable tunnel is dialed
// by gorilla, which ignores the HTTP client entirely.
func TestSlaveDialsMasterThroughSocks5Proxy(t *testing.T) {
	socks := startSocksRecorder(t)
	masterStore := openStore(t)
	center := notifications.New()
	master, err := New(Options{Store: masterStore, Notifications: center})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(master)
	defer server.Close()
	slave, err := New(Options{
		Store: openStore(t), MasterURL: server.URL, Name: "build-host", Local: testLocal{},
		PollInterval: 10 * time.Millisecond, ProxyURL: "socks5://" + socks.addr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- slave.RunSlave(ctx) }()
	var notification notifications.Notification
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := center.List(); len(got) > 0 {
			notification = got[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if notification.ID == "" {
		t.Fatal("registration through the proxy never reached the master")
	}
	if _, handled, err := master.HandleNotificationAction(context.Background(), notification.ID, "accept"); err != nil || !handled {
		t.Fatalf("accept handled=%v err=%v", handled, err)
	}
	hostID := strings.TrimPrefix(notification.ID, "federation-registration-")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if peer, _ := masterStore.FederationSlave(hostID); peer != nil && peer.Status == "connected" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if peer, _ := masterStore.FederationSlave(hostID); peer == nil || peer.Status != "connected" {
		t.Fatalf("slave did not connect through the proxy: %#v", peer)
	}
	var sawStatus, sawTunnel bool
	for _, line := range socks.requests() {
		if strings.Contains(line, StatusPath) || strings.Contains(line, RegisterPath) {
			sawStatus = true
		}
		if strings.Contains(line, TunnelPath) {
			sawTunnel = true
		}
	}
	if !sawStatus {
		t.Errorf("registration request bypassed the proxy: %#v", socks.requests())
	}
	if !sawTunnel {
		t.Errorf("tunnel upgrade bypassed the proxy: %#v", socks.requests())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsUnusableProxyURL(t *testing.T) {
	for _, raw := range []string{"::not a url::", "socks5://", "gopher://127.0.0.1:1080"} {
		if _, err := New(Options{Store: openStore(t), MasterURL: "http://upstream.example", ProxyURL: raw}); err == nil {
			t.Errorf("ProxyURL %q was accepted", raw)
		}
	}
}
