package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// testReporter records what the sharer reports and answers "Allow?" with allow.
type testReporter struct {
	mu    sync.Mutex
	lines []string
	allow bool
	asked int
}

func (r *testReporter) add(s string)  { r.mu.Lock(); r.lines = append(r.lines, s); r.mu.Unlock() }
func (r *testReporter) Step(s string) { r.add("step " + s) }
func (r *testReporter) Warn(s string) { r.add("warn " + s) }
func (r *testReporter) Spin(s string) {}
func (r *testReporter) Request(e reqEntry) {
	r.add(fmt.Sprintf("req %s %s %d", e.Method, e.Path, e.Status))
}
func (r *testReporter) Ask(_ context.Context, q string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked++
	return r.allow
}

func newTestHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// testApp is the "local app" being shared.
func testApp(t *testing.T) (addr string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), ln.Addr().(*net.TCPAddr).Port
}

// setup starts a sharer and a connected guest host.
func setup(t *testing.T, opts shareOpts, rep *testReporter) (*sharer, host.Host) {
	addr, port := testApp(t)
	opts.target, opts.port = addr, port
	if opts.code == "" {
		opts.code = "8-maple-otter"
	}
	if opts.maxGuests == 0 {
		opts.maxGuests = 1
	}
	sh := newTestHost(t)
	s := newSharer(sh, opts, rep)
	g := newTestHost(t)
	if err := g.Connect(context.Background(), peer.AddrInfo{ID: sh.ID(), Addrs: sh.Addrs()}); err != nil {
		t.Fatal(err)
	}
	return s, g
}

// join runs the guest side of the control protocol by hand.
func join(t *testing.T, g host.Host, sharerID peer.ID, code string) (*secureConn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := g.NewStream(ctx, sharerID, controlProto)
	if err != nil {
		t.Fatal(err)
	}
	st.SetDeadline(time.Now().Add(10 * time.Second))
	sc, err := handshake(st, code, false, []byte(sharerID), []byte(g.ID()))
	if err != nil {
		return nil, err
	}
	for {
		typ, payload, err := sc.ReadMsg()
		if err != nil {
			return nil, err
		}
		switch typ {
		case msgWait:
			continue
		case msgHello:
			var h hello
			json.Unmarshal(payload, &h)
			return sc, nil
		case msgReject:
			return nil, errors.New("rejected: " + string(payload))
		}
	}
}

// get does an HTTP request over a new tunnel stream.
func get(g host.Host, sharerID peer.ID, path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := g.NewStream(ctx, sharerID, tcpProto)
	if err != nil {
		return "", err
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(st, "GET %s HTTP/1.0\r\nHost: x\r\n\r\n", path)
	st.CloseWrite()
	b, err := io.ReadAll(st)
	return string(b), err
}

func TestTunnelRequiresHandshake(t *testing.T) {
	rep := &testReporter{allow: true}
	s, g := setup(t, shareOpts{}, rep)

	// A peer that never proved the code gets its stream reset.
	if body, err := get(g, s.h.ID(), "/secret"); err == nil && strings.Contains(body, "hello") {
		t.Fatalf("unauthorized stream was forwarded: %q", body)
	}

	if _, err := join(t, g, s.h.ID(), "8-maple-otter"); err != nil {
		t.Fatal(err)
	}
	body, err := get(g, s.h.ID(), "/ok")
	if err != nil || !strings.HasSuffix(body, "hello /ok") {
		t.Fatalf("got %q, %v", body, err)
	}
	if rep.asked != 1 {
		t.Errorf("asked %d times, want 1", rep.asked)
	}
	time.Sleep(50 * time.Millisecond)
	if !strings.Contains(strings.Join(rep.lines, "\n"), "req GET /ok 200") {
		t.Errorf("request not logged: %q", rep.lines)
	}
}

func TestTunnelWrongCode(t *testing.T) {
	rep := &testReporter{allow: true}
	s, g := setup(t, shareOpts{}, rep)
	for i := range maxBadAttempts {
		_, err := join(t, g, s.h.ID(), "8-maple-tiger")
		if !errors.Is(err, errBadCode) {
			t.Fatalf("attempt %d: want errBadCode, got %v", i, err)
		}
		// A wrong code never authorizes anything.
		if body, err := get(g, s.h.ID(), "/"); err == nil && body != "" {
			t.Fatalf("tunnel open after wrong code: %q", body)
		}
	}
	select {
	case err := <-s.fatal:
		if !strings.Contains(err.Error(), "Too many wrong codes") {
			t.Fatalf("unexpected fatal error %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sharer didn't stop after 3 wrong codes")
	}
	if rep.asked != 0 {
		t.Error("asked to allow a guest with a wrong code")
	}
}

func TestTunnelDeclined(t *testing.T) {
	rep := &testReporter{allow: false}
	s, g := setup(t, shareOpts{}, rep)
	if _, err := join(t, g, s.h.ID(), "8-maple-otter"); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("want declined, got %v", err)
	}
	if body, err := get(g, s.h.ID(), "/"); err == nil && body != "" {
		t.Fatalf("tunnel open after decline: %q", body)
	}
}

func TestTunnelMaxGuestsAndYes(t *testing.T) {
	rep := &testReporter{}
	s, g1 := setup(t, shareOpts{yes: true}, rep)
	if _, err := join(t, g1, s.h.ID(), "8-maple-otter"); err != nil {
		t.Fatal(err)
	}
	if rep.asked != 0 {
		t.Error("--yes still asked")
	}
	g2 := newTestHost(t)
	if err := g2.Connect(context.Background(), peer.AddrInfo{ID: s.h.ID(), Addrs: s.h.Addrs()}); err != nil {
		t.Fatal(err)
	}
	if _, err := join(t, g2, s.h.ID(), "8-maple-otter"); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("second guest: want max guests rejection, got %v", err)
	}
}

func TestTunnelAuthorizationEndsWithControlStream(t *testing.T) {
	rep := &testReporter{allow: true}
	s, g := setup(t, shareOpts{}, rep)
	sc, err := join(t, g, s.h.ID(), "8-maple-otter")
	if err != nil {
		t.Fatal(err)
	}
	sc.WriteMsg(msgBye, nil)
	deadline := time.Now().Add(3 * time.Second)
	for len(s.guestList()) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if body, err := get(g, s.h.ID(), "/"); err == nil && body != "" {
		t.Fatalf("tunnel still open after the guest left: %q", body)
	}
	// Coming back with the code works without asking again.
	if _, err := join(t, g, s.h.ID(), "8-maple-otter"); err != nil {
		t.Fatal(err)
	}
	if rep.asked != 1 {
		t.Errorf("asked %d times, want 1 (reconnects are not re-asked)", rep.asked)
	}
}

func TestListenLocalSkipsBusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	p := busy.Addr().(*net.TCPAddr).Port
	if _, err := listenLocal(p, true); err == nil {
		t.Fatal("explicit busy port should fail")
	}
	ln, err := listenLocal(p, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a := ln.Addr().(*net.TCPAddr)
	if a.Port == p || !a.IP.IsLoopback() {
		t.Fatalf("got %v", a)
	}
}
