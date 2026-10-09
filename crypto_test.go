package main

import (
	"errors"
	"net"
	"testing"
)

type hsResult struct {
	sc  *secureConn
	err error
}

// runHandshake runs both sides over an in-memory pipe.
func runHandshake(t *testing.T, sharerCode, guestCode string, sharerIDs, guestIDs [2]string) (sharer, guest hsResult) {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan hsResult, 1)
	go func() {
		sc, err := handshake(a, sharerCode, true, []byte(sharerIDs[0]), []byte(sharerIDs[1]))
		if err != nil {
			a.Close() // the sharer hangs up on a wrong code
		}
		done <- hsResult{sc, err}
	}()
	sc, err := handshake(b, guestCode, false, []byte(guestIDs[0]), []byte(guestIDs[1]))
	if err != nil {
		b.Close()
	}
	return <-done, hsResult{sc, err}
}

var ids = [2]string{"sharer-peer-id", "guest-peer-id"}

func TestHandshakeRightCode(t *testing.T) {
	s, g := runHandshake(t, "8-maple-otter", "8-maple-otter", ids, ids)
	if s.err != nil || g.err != nil {
		t.Fatalf("handshake failed: sharer %v, guest %v", s.err, g.err)
	}
	// Both directions work, and messages arrive in order.
	go func() {
		s.sc.WriteMsg(msgHello, []byte(`{"port":3000}`))
		s.sc.WriteMsg(msgPing, nil)
	}()
	typ, payload, err := g.sc.ReadMsg()
	if err != nil || typ != msgHello || string(payload) != `{"port":3000}` {
		t.Fatalf("guest got %q %q %v", typ, payload, err)
	}
	if typ, _, err := g.sc.ReadMsg(); err != nil || typ != msgPing {
		t.Fatalf("guest got %q %v", typ, err)
	}
	go g.sc.WriteMsg(msgBye, nil)
	if typ, _, err := s.sc.ReadMsg(); err != nil || typ != msgBye {
		t.Fatalf("sharer got %q %v", typ, err)
	}
}

func TestHandshakeWrongCode(t *testing.T) {
	s, g := runHandshake(t, "8-maple-otter", "8-maple-tiger", ids, ids)
	if !errors.Is(s.err, errBadCode) {
		t.Errorf("sharer: want errBadCode, got %v", s.err)
	}
	if !errors.Is(g.err, errBadCode) {
		t.Errorf("guest: want errBadCode, got %v", g.err)
	}
}

// The PAKE is bound to both peer IDs: a man in the middle relaying the
// messages between two different peer pairs gets a key mismatch.
func TestHandshakeBoundToPeerIDs(t *testing.T) {
	s, g := runHandshake(t, "8-maple-otter", "8-maple-otter", ids, [2]string{"sharer-peer-id", "attacker-peer-id"})
	if !errors.Is(s.err, errBadCode) || !errors.Is(g.err, errBadCode) {
		t.Fatalf("want errBadCode on both sides, got sharer %v, guest %v", s.err, g.err)
	}
}

func TestSecureConnDetectsTampering(t *testing.T) {
	a, b := net.Pipe()
	k1, k2 := make([]byte, 32), make([]byte, 32)
	k2[0] = 1
	w, _ := newSecureConn(a, k1, k2)
	evil := &flipper{Conn: b}
	r, _ := newSecureConn(evil, k2, k1)
	go w.WriteMsg(msgHello, []byte("hello"))
	if _, _, err := r.ReadMsg(); err == nil {
		t.Fatal("tampered message was accepted")
	}
}

// flipper flips one bit of the ciphertext on its way in.
type flipper struct {
	net.Conn
	n int
}

func (f *flipper) Read(p []byte) (int, error) {
	n, err := f.Conn.Read(p)
	if f.n += n; f.n > 6 && n > 0 {
		p[n-1] ^= 1
	}
	return n, err
}
