package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Control messages, sent inside the encrypted control channel.
const (
	msgWait   = 'W' // sharer -> guest: code OK, waiting for the sharer to allow you
	msgHello  = 'H' // sharer -> guest: you're in; JSON hello
	msgReject = 'R' // sharer -> guest: not allowed; payload is the reason
	msgPing   = 'P' // both ways, keeps the connection alive and detects drops
	msgBye    = 'B' // both ways: clean shutdown
)

const (
	maxBadAttempts = 3
	pingEvery      = 10 * time.Second
	pingTimeout    = 35 * time.Second
)

type hello struct {
	Port int `json:"port"` // the port being shared, so the guest can mirror it
}

type shareOpts struct {
	target    string // dial address, e.g. localhost:3000
	port      int
	code      string
	yes       bool
	maxGuests int
}

// guest is one admitted, connected guest.
type guest struct {
	id     peer.ID
	conn   string // "direct · QUIC · LAN 192.168.1.5"
	since  time.Time
	active atomic.Int64
	sc     *secureConn
	st     network.Stream
}

// sharer serves one shared port.
//
// Security model, in short:
//  1. A guest opens a control stream and runs SPAKE2 with the code. The PAKE
//     binds both libp2p peer IDs, so success proves that the peer with this
//     exact peer ID knows the code (not just someone on the path).
//  2. libp2p already authenticates every connection by peer ID (Noise or
//     TLS 1.3 handshake proves the private key) and encrypts it end to end.
//  3. So once the code is verified (and the sharer said yes), we simply
//     authorize that peer ID. Each forwarded TCP connection is a new libp2p
//     stream; handleTCP accepts it only if the stream's authenticated remote
//     peer ID is a currently connected, authorized guest. Nobody else can
//     open streams as that peer ID, and the bytes are encrypted by the
//     libp2p transport between the two peer IDs.
//  4. Authorization ends when the guest's control stream ends.
//
// libp2p also refuses to open these streams over a relayed ("limited")
// connection, so tunnel data only ever flows on a direct connection.
type sharer struct {
	opts  shareOpts
	h     host.Host
	rep   reporter
	stats *stats
	fatal chan error

	hsMu sync.Mutex // one handshake + "Allow?" prompt at a time

	mu       sync.Mutex
	guests   map[peer.ID]*guest
	approved map[peer.ID]bool // said yes once: reconnects don't ask again
	bad      int
	lastWarn time.Time
	hadGuest atomic.Bool
}

func newSharer(h host.Host, opts shareOpts, rep reporter) *sharer {
	s := &sharer{
		opts: opts, h: h, rep: rep, stats: &stats{},
		fatal:    make(chan error, 1),
		guests:   map[peer.ID]*guest{},
		approved: map[peer.ID]bool{},
	}
	h.SetStreamHandler(controlProto, s.handleControl)
	h.SetStreamHandler(tcpProto, s.handleTCP)
	return s
}

func (s *sharer) handleControl(st network.Stream) {
	remote := st.Conn().RemotePeer()
	sc, g, ok := s.admit(st, remote)
	if !ok {
		return
	}
	s.rep.Step("Guest " + sBold.Render(shortID(remote.String())) + " connected " + sDim.Render("· "+g.conn))

	bye, _ := keepalive(sc, st)

	s.mu.Lock()
	if s.guests[remote] == g {
		delete(s.guests, remote)
	}
	s.mu.Unlock()
	st.Close()
	if bye {
		s.rep.Warn("Guest " + sBold.Render(shortID(remote.String())) + " left")
	} else {
		s.rep.Warn("Guest " + sBold.Render(shortID(remote.String())) + " lost connection " + sDim.Render("· they will reconnect automatically"))
	}
}

// admit runs the handshake and the allow/limit checks for a new control stream.
func (s *sharer) admit(st network.Stream, remote peer.ID) (*secureConn, *guest, bool) {
	s.hsMu.Lock()
	defer s.hsMu.Unlock()

	st.SetDeadline(time.Now().Add(30 * time.Second))
	sc, err := handshake(st, s.opts.code, true, []byte(s.h.ID()), []byte(remote))
	if err != nil {
		st.Reset()
		if errors.Is(err, errBadCode) {
			s.wrongCode()
		}
		return nil, nil, false
	}
	st.SetDeadline(time.Time{})

	conn := describeConn(st.Conn())
	s.mu.Lock()
	_, already := s.guests[remote]
	full := !already && len(s.guests) >= s.opts.maxGuests
	known := s.approved[remote]
	s.mu.Unlock()

	reject := func(reason string) (*secureConn, *guest, bool) {
		sc.WriteMsg(msgReject, []byte(reason))
		hangUp(st)
		return nil, nil, false
	}
	if full {
		s.rep.Warn(fmt.Sprintf("Turned away a guest: %d of %d already connected %s",
			s.opts.maxGuests, s.opts.maxGuests, sDim.Render("· use --max-guests to allow more")))
		return reject("the sharer already has the maximum number of guests")
	}
	if !known && !s.opts.yes {
		sc.WriteMsg(msgWait, nil)
		// The guest sends nothing until our hello, so any read result while
		// we wait for an answer means it gave up: stop asking.
		ctx, cancel := context.WithCancel(context.Background())
		var answered atomic.Bool
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			buf := make([]byte, 1)
			st.Read(buf)
			if !answered.Load() {
				cancel()
			}
		}()
		ok := s.rep.Ask(ctx, fmt.Sprintf("Allow guest %s (%s)?", shortID(remote.String()), conn))
		answered.Store(true)
		st.SetReadDeadline(time.Now()) // unblock the reader
		<-readerDone
		st.SetReadDeadline(time.Time{})
		gone := ctx.Err() != nil
		cancel()
		if gone {
			st.Reset()
			s.rep.Warn("The guest left before you answered")
			return nil, nil, false
		}
		if !ok {
			s.rep.Warn("Declined guest " + shortID(remote.String()))
			return reject("the sharer declined")
		}
	}

	g := &guest{id: remote, conn: conn, since: time.Now(), sc: sc, st: st}
	s.mu.Lock()
	s.approved[remote] = true
	if old := s.guests[remote]; old != nil {
		old.st.Reset() // a reconnect replaces a stale control stream
	}
	s.guests[remote] = g
	s.mu.Unlock()
	s.hadGuest.Store(true)

	hb, _ := json.Marshal(hello{Port: s.opts.port})
	if err := sc.WriteMsg(msgHello, hb); err != nil {
		s.mu.Lock()
		if s.guests[remote] == g {
			delete(s.guests, remote)
		}
		s.mu.Unlock()
		st.Reset()
		return nil, nil, false
	}
	return sc, g, true
}

func (s *sharer) wrongCode() {
	s.mu.Lock()
	s.bad++
	left := maxBadAttempts - s.bad
	s.mu.Unlock()
	if left <= 0 {
		select {
		case s.fatal <- withHint("Share again to get a fresh code.",
			"Too many wrong codes, stopped sharing (someone may be guessing your code)."):
		default:
		}
		return
	}
	attempts := "attempts"
	if left == 1 {
		attempts = "attempt"
	}
	s.rep.Warn(fmt.Sprintf("Someone tried a wrong code %s", sDim.Render(fmt.Sprintf("· %d %s left", left, attempts))))
}

// handleTCP serves one forwarded TCP connection (see the security model above).
func (s *sharer) handleTCP(st network.Stream) {
	remote := st.Conn().RemotePeer()
	s.mu.Lock()
	g := s.guests[remote]
	s.mu.Unlock()
	if g == nil || st.Conn().Stat().Limited {
		st.Reset()
		return
	}
	c, err := net.DialTimeout("tcp", s.opts.target, 5*time.Second)
	if err != nil {
		st.Reset()
		s.mu.Lock()
		throttle := time.Since(s.lastWarn) < 10*time.Second
		s.lastWarn = time.Now()
		s.mu.Unlock()
		if !throttle {
			s.rep.Warn(fmt.Sprintf("A guest connected but nothing answered on %s %s", s.opts.target, sDim.Render("· is your app still running?")))
		}
		return
	}
	s.stats.conns.Add(1)
	s.stats.active.Add(1)
	g.active.Add(1)
	defer func() {
		s.stats.active.Add(-1)
		g.active.Add(-1)
	}()

	sniff := newHTTPSniffer(func(r reqEntry) {
		s.stats.requests.Add(1)
		s.rep.Request(r)
	})
	pipe(st, c.(*net.TCPConn),
		func(b []byte) { s.stats.in.Add(int64(len(b))); sniff.request(b) },
		func(b []byte) { s.stats.out.Add(int64(len(b))); sniff.response(b) })
}

type guestView struct {
	ID     string
	Conn   string
	Since  time.Time
	Active int64
}

func (s *sharer) guestList() []guestView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var l []guestView
	for _, g := range s.guests {
		l = append(l, guestView{g.id.String(), g.conn, g.since, g.active.Load()})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Since.Before(l[j].Since) })
	return l
}

// close tells every guest we're leaving, so they stop instead of reconnecting.
func (s *sharer) close() {
	s.mu.Lock()
	var gs []*guest
	for _, g := range s.guests {
		gs = append(gs, g)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, g := range gs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.st.SetWriteDeadline(time.Now().Add(2 * time.Second))
			g.sc.WriteMsg(msgBye, nil)
			g.st.CloseWrite()
		}()
	}
	wg.Wait()
	if len(gs) > 0 {
		time.Sleep(300 * time.Millisecond) // let the bye reach them
	}
}

// keepalive pings over the control channel until the other side says bye
// (true) or the connection dies (false).
func keepalive(sc *secureConn, st network.Stream) (bye bool, err error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if sc.WriteMsg(msgPing, nil) != nil {
					return
				}
			}
		}
	}()
	for {
		st.SetReadDeadline(time.Now().Add(pingTimeout))
		typ, _, err := sc.ReadMsg()
		if err != nil {
			return false, err
		}
		if typ == msgBye {
			return true, nil
		}
	}
}

// hangUp closes our side and waits briefly for the peer to read our last
// message, so it isn't lost when the stream is torn down.
func hangUp(s network.Stream) {
	s.CloseWrite()
	s.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	for {
		if _, err := s.Read(buf); err != nil {
			break
		}
	}
	s.Close()
}

// ---------------------------------------------------------------- command

func runShare(ctx context.Context, opts shareOpts, rep reporter, ready func(*sharer)) error {
	_, nameplate, err := parseCode(opts.code)
	if err != nil {
		return err
	}
	n, err := newNode(ctx, true)
	if err != nil {
		return err
	}
	defer n.Close()

	s := newSharer(n.host, opts, rep)
	if ready != nil {
		ready(s)
	}
	defer s.close()

	if stop, err := n.startMDNS(nameplate, func(peer.AddrInfo) {}); err != nil {
		rep.Warn(fmt.Sprintf("LAN discovery unavailable: %v", err))
	} else {
		defer stop()
		rep.Step("Visible on your local network")
	}
	rep.Spin("Waiting for your friend…")
	go advertise(ctx, n, nameplate, rep, func() bool { return s.hadGuest.Load() })

	select {
	case err := <-s.fatal:
		return err
	case <-ctx.Done():
		return nil
	}
}

// advertise publishes the nameplate on the public DHT and keeps it fresh.
// Once a guest is connected, network progress is no longer news.
func advertise(ctx context.Context, n *node, nameplate string, rep reporter, quiet func() bool) {
	n.joinDHT(ctx)
	if ctx.Err() != nil {
		return
	}
	if !quiet() {
		rep.Step("Joined the peer-to-peer network " + sDim.Render(fmt.Sprintf("· %d peers", n.dht.RoutingTable().Size())))
	}
	if n.waitForRelay(ctx, 45*time.Second) && !quiet() {
		rep.Step("Relay reserved for NAT traversal " + sDim.Render("· used only to set up a direct connection"))
	} else if ctx.Err() == nil && !quiet() {
		rep.Warn("No public relay yet " + sDim.Render("· guests behind strict NAT may not reach you"))
	}
	announced := false
	for ctx.Err() == nil {
		_, err := n.disc.Advertise(ctx, dhtNamespace(nameplate))
		wait := 5 * time.Minute
		if err != nil {
			wait = 10 * time.Second
		} else if !announced {
			announced = true
			if !quiet() {
				rep.Step("Reachable over the internet")
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}
