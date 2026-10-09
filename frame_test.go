package main

import (
	"crypto/rand"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"os"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestDashboardFrame renders one frame of each dashboard with the real view
// code and sample data. With PORTAL_FRAME_OUT=dir set it writes the ANSI
// frames there (used for the README screenshots).
func TestDashboardFrame(t *testing.T) {
	now := time.Now()
	opts := shareOpts{target: "localhost:3000", port: 3000, code: "8-maple-otter", maxGuests: 1}
	s := &sharer{opts: opts, stats: &stats{}, guests: map[peer.ID]*guest{}}
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := peer.IDFromPrivateKey(priv)
	g := &guest{id: id, conn: "direct · QUIC · internet xxx.xxx.xxx.xxx", since: now.Add(-4*time.Minute - 12*time.Second)}
	g.active.Store(3)
	s.guests[id] = g
	s.stats.in.Store(1_284_000)
	s.stats.out.Store(48_300_000)
	s.stats.active.Store(3)
	s.stats.conns.Store(128)
	s.stats.requests.Store(412)

	m := &dash{sharing: true, share: opts, sh: s, width: 100,
		inRate: 34_100, outRate: 1_240_000, lastSample: now}
	for i := range history {
		v := float64((i*37)%23) * 60_000
		if i > 28 {
			v *= 2.2
		}
		m.samples = append(m.samples, v)
	}
	m.steps = []stepMsg{
		{text: "Visible on your local network"},
		{text: "Joined the peer-to-peer network " + sDim.Render("· 5 peers")},
		{text: "Guest " + sBold.Render(shortID(id.String())) + " connected " + sDim.Render("· direct · QUIC · internet xxx.xxx.xxx.xxx")},
	}
	reqs := []struct {
		m, p string
		st   int
		ms   int
	}{
		{"GET", "/", 200, 14}, {"GET", "/assets/app.js", 200, 9}, {"GET", "/api/users", 200, 12},
		{"POST", "/api/login", 401, 31}, {"POST", "/api/login", 200, 48}, {"GET", "/ws", 101, 2},
		{"GET", "/api/projects?page=2", 200, 22}, {"GET", "/favicon.ico", 404, 1},
	}
	for i, r := range reqs {
		m.reqs = append(m.reqs, reqEntry{At: now.Add(time.Duration(i-8) * 3 * time.Second), Method: r.m, Path: r.p, Status: r.st, Took: time.Duration(r.ms) * time.Millisecond})
	}
	share := m.View()

	o := &opener{stats: &stats{}, connected: true, local: "localhost:3000",
		conn: "direct · QUIC · internet xxx.xxx.xxx.xxx", since: now.Add(-4*time.Minute - 12*time.Second)}
	o.stats.in.Store(48_300_000)
	o.stats.out.Store(1_284_000)
	o.stats.active.Store(3)
	o.stats.conns.Store(128)
	gm := &dash{op: o, samples: m.samples, inRate: 1_240_000, outRate: 34_100, lastSample: now}
	gm.steps = []stepMsg{
		{text: "Searching your local network"},
		{text: "Found the sharer " + sDim.Render("· direct · QUIC · internet xxx.xxx.xxx.xxx")},
		{text: "Code verified " + sDim.Render("· SPAKE2, end-to-end encrypted")},
		{text: "Ready · open " + sCyan.Render("http://localhost:3000")},
	}
	open := gm.View()

	if share == "" || open == "" {
		t.Fatal("empty view")
	}
	if dir := os.Getenv("PORTAL_FRAME_OUT"); dir != "" {
		os.WriteFile(dir+"/dash-share.ans", []byte(share), 0o644)
		os.WriteFile(dir+"/dash-open.ans", []byte(open), 0o644)
	}
}

func TestDashboardAnswersPrompt(t *testing.T) {
	for key, want := range map[string]bool{"y": true, "enter": true, "n": false, "esc": false} {
		m := &dash{sharing: true, share: shareOpts{code: "8-maple-otter", maxGuests: 1}, cancel: func() {}}
		reply := make(chan bool, 1)
		m.Update(askMsg{"Allow guest?", reply})
		if !strings.Contains(m.View(), "Allow guest?") {
			t.Fatal("prompt not shown")
		}
		var k tea.KeyMsg
		switch key {
		case "enter":
			k = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			k = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		}
		m.Update(k)
		if got := <-reply; got != want || m.ask != nil {
			t.Errorf("key %q: got %v, want %v", key, got, want)
		}
	}
	// ctrl+c while asking declines and stops.
	stopped := false
	m := &dash{sharing: true, cancel: func() { stopped = true }}
	reply := make(chan bool, 1)
	m.Update(askMsg{"Allow guest?", reply})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if <-reply || !stopped {
		t.Error("ctrl+c should decline and stop")
	}
}
