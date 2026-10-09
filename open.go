package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

type openOpts struct {
	code    string
	port    int // local port to use; 0 = same as the sharer's
	timeout time.Duration
}

var (
	errTryLater  = errors.New("peer not usable")           // try another candidate / again later
	errRelayOnly = errors.New("only a relayed connection") // hole punching failed
)

// opener is the guest side: it finds the sharer, keeps an encrypted control
// channel to it, and forwards every local TCP connection over a new stream.
type opener struct {
	opts  openOpts
	n     *node
	rep   reporter
	stats *stats

	mu        sync.Mutex
	sharer    peer.ID // fixed after the first successful connection
	conn      string
	since     time.Time
	connected bool
	local     string // "localhost:3000" once listening

	found    atomic.Bool // printed "Found the sharer" once
	lastWarn atomic.Int64
}

type session struct {
	st    network.Stream
	sc    *secureConn
	hello hello
}

type openView struct {
	Connected bool
	Local     string
	Conn      string
	Since     time.Time
}

func (o *opener) view() openView {
	o.mu.Lock()
	defer o.mu.Unlock()
	return openView{o.connected, o.local, o.conn, o.since}
}

func runOpen(ctx context.Context, opts openOpts, rep reporter, ready func(*opener)) error {
	code, nameplate, err := parseCode(opts.code)
	if err != nil {
		return withHint("Copy the whole code from your friend, like 8-maple-otter.", "%s", err.Error())
	}
	opts.code = code
	n, err := newNode(ctx, false)
	if err != nil {
		return err
	}
	defer n.Close()
	o := &opener{opts: opts, n: n, rep: rep, stats: &stats{}}
	if ready != nil {
		ready(o)
	}

	candidates := make(chan peer.AddrInfo, 64)
	push := func(ai peer.AddrInfo) {
		if ai.ID == n.host.ID() {
			return
		}
		select {
		case candidates <- ai:
		default:
		}
	}
	if stop, err := n.startMDNS(nameplate, push); err != nil {
		rep.Warn(fmt.Sprintf("LAN discovery unavailable: %v", err))
	} else {
		defer stop()
		rep.Step("Searching your local network")
	}
	go func() {
		n.joinDHT(ctx)
		if ctx.Err() == nil && !o.found.Load() {
			rep.Step("Searching the internet " + sDim.Render(fmt.Sprintf("· joined the p2p network, %d peers", n.dht.RoutingTable().Size())))
		}
		for ctx.Err() == nil {
			if ch, err := n.disc.FindPeers(ctx, dhtNamespace(nameplate)); err == nil {
				for ai := range ch {
					push(ai)
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}()

	rep.Spin("Looking for the sharer…")
	sess, err := o.find(ctx, candidates, opts.timeout)
	if err != nil {
		return err
	}

	want := opts.port
	if want == 0 {
		want = sess.hello.Port
	}
	ln, err := listenLocal(want, opts.port != 0)
	if err != nil {
		sess.sc.WriteMsg(msgBye, nil)
		hangUp(sess.st)
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	o.mu.Lock()
	o.local = "localhost:" + strconv.Itoa(port)
	o.mu.Unlock()
	if port != sess.hello.Port {
		rep.Warn(fmt.Sprintf("Port %d is busy here, using %d instead", sess.hello.Port, port))
	}
	go o.serve(ctx, ln)

	for {
		o.setConnected(sess)
		rep.Step("Ready · open " + sCyan.Render("http://"+o.view().Local))
		bye := o.hold(ctx, sess)
		o.mu.Lock()
		o.connected = false
		o.mu.Unlock()
		switch {
		case ctx.Err() != nil:
			return nil
		case bye:
			rep.Warn("The sharer stopped sharing")
			return nil
		}
		rep.Warn("Connection lost " + sDim.Render("· reconnecting…"))
		rep.Spin("Reconnecting to the sharer…")
		if sess, err = o.reconnect(ctx, candidates); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (o *opener) setConnected(sess *session) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.connected = true
	o.sharer = sess.st.Conn().RemotePeer()
	o.conn = describeConn(sess.st.Conn())
	o.since = time.Now()
}

// find tries discovered peers until one completes the handshake.
func (o *opener) find(ctx context.Context, candidates <-chan peer.AddrInfo, timeout time.Duration) (*session, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	lastTry := map[peer.ID]time.Time{}
	rejected := map[peer.ID]bool{}
	relayOnly := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			switch {
			case len(rejected) > 0:
				return nil, withHint("Check for typos and ask your friend for the code again.",
					"The sharer is there, but the code didn't match.")
			case relayOnly > 0:
				return nil, errNoDirect
			}
			return nil, withHint("Check the code, and that `portal share` is still running on the other machine.",
				"Couldn't find the sharer.")
		case ai := <-candidates:
			if rejected[ai.ID] || time.Since(lastTry[ai.ID]) < 10*time.Second {
				continue
			}
			lastTry[ai.ID] = time.Now()
			sess, err := o.dial(ctx, ai)
			switch {
			case err == nil:
				return sess, nil
			case errors.Is(err, errBadCode):
				// Probably a typo. (It could also be someone else sharing
				// with the same number, so keep looking for a little while.)
				rejected[ai.ID] = true
				o.rep.Warn("Found a sharer, but the code didn't match " + sDim.Render("· typo? still looking"))
				deadline.Reset(15 * time.Second)
			case errors.Is(err, errRelayOnly):
				relayOnly++
				if relayOnly == 1 {
					o.rep.Warn(errNoDirect.Error() + " " + sDim.Render("· still trying"))
				}
			case errors.Is(err, errTryLater):
			default:
				return nil, err
			}
			o.rep.Spin("Looking for the sharer…")
		}
	}
}

var errNoDirect = withHint("Both networks block direct connections (strict NAT). portal never sends your data through public relays. Try another network (e.g. a phone hotspot) on one side, or the same Wi-Fi.",
	"Only a relayed connection to the sharer is possible.")

// reconnect re-runs the handshake with the same sharer after a drop. The
// sharer already approved us, so it lets us back in without asking.
func (o *opener) reconnect(ctx context.Context, candidates <-chan peer.AddrInfo) (*session, error) {
	o.mu.Lock()
	id := o.sharer
	o.mu.Unlock()
	backoff := time.Second
	for {
		sess, err := o.dial(ctx, peer.AddrInfo{ID: id})
		if err == nil {
			return sess, nil
		}
		if errors.Is(err, errBadCode) || ctx.Err() != nil {
			return nil, err
		}
		var h hinted
		if asHint(err, &h) {
			return nil, err // rejected by the sharer
		}
		o.rep.Spin("Reconnecting to the sharer…")
		timer := time.NewTimer(backoff)
	wait:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case ai := <-candidates: // fresh addresses from discovery
				if ai.ID == id {
					o.n.host.Peerstore().AddAddrs(ai.ID, ai.Addrs, time.Hour)
				}
			case <-timer.C:
				break wait
			}
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (o *opener) dial(ctx context.Context, ai peer.AddrInfo) (*session, error) {
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cctx = network.WithDialPeerTimeout(cctx, 45*time.Second)
	h := o.n.host

	o.rep.Spin("Connecting to a possible sharer…")
	if len(ai.Addrs) == 0 && len(h.Peerstore().Addrs(ai.ID)) == 0 {
		found, err := o.n.dht.FindPeer(cctx, ai.ID)
		if err != nil {
			return nil, errTryLater
		}
		ai = found
	}
	if err := h.Connect(cctx, ai); err != nil {
		return nil, errTryLater
	}
	// If we only reached the sharer through a relay, NewStream waits here
	// while libp2p hole-punches a direct connection, and fails if it can't.
	o.rep.Spin("Opening a direct connection…")
	st, err := h.NewStream(cctx, ai.ID, controlProto)
	if err != nil {
		if errors.Is(err, network.ErrLimitedConn) {
			return nil, errRelayOnly
		}
		return nil, errTryLater
	}
	ok := false
	defer func() {
		if !ok {
			st.Reset()
		}
	}()

	o.rep.Spin("Verifying the code…")
	st.SetDeadline(time.Now().Add(30 * time.Second))
	sc, err := handshake(st, o.opts.code, false, []byte(ai.ID), []byte(h.ID()))
	if errors.Is(err, errBadCode) {
		return nil, err
	}
	if err != nil {
		return nil, errTryLater
	}
	st.SetDeadline(time.Time{})
	if o.found.CompareAndSwap(false, true) {
		o.rep.Step("Found the sharer " + sDim.Render("· "+describeConn(st.Conn())))
		o.rep.Step("Code verified " + sDim.Render("· SPAKE2, end-to-end encrypted"))
	}

	for {
		st.SetReadDeadline(time.Now().Add(30 * time.Second))
		typ, payload, err := sc.ReadMsg()
		if err != nil {
			return nil, errTryLater
		}
		switch typ {
		case msgWait:
			o.rep.Spin("Waiting for the sharer to let you in…")
			st.SetReadDeadline(time.Now().Add(10 * time.Minute))
			typ, payload, err = sc.ReadMsg()
			if err != nil {
				return nil, errTryLater
			}
		}
		switch typ {
		case msgHello:
			var hl hello
			if json.Unmarshal(payload, &hl) != nil || hl.Port < 1 || hl.Port > 65535 {
				return nil, errors.New("bad hello from the sharer")
			}
			st.SetReadDeadline(time.Time{})
			ok = true
			return &session{st: st, sc: sc, hello: hl}, nil
		case msgReject:
			ok = true
			hangUp(st)
			return nil, withHint("Ask your friend to share again, or to run it with --max-guests.",
				"Not let in: %s.", payload)
		case msgPing:
			continue
		}
		return nil, fmt.Errorf("unexpected message %q from the sharer", typ)
	}
}

// hold keeps the session alive. It returns true if the sharer said bye.
func (o *opener) hold(ctx context.Context, sess *session) bool {
	stop := context.AfterFunc(ctx, func() {
		sess.st.SetWriteDeadline(time.Now().Add(2 * time.Second))
		sess.sc.WriteMsg(msgBye, nil)
		sess.st.CloseWrite()
		sess.st.SetReadDeadline(time.Now().Add(2 * time.Second))
	})
	defer stop()
	bye, _ := keepalive(sess.sc, sess.st)
	sess.st.Close()
	return bye
}

// serve accepts local connections and forwards each over a new stream.
func (o *opener) serve(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go o.forward(ctx, c.(*net.TCPConn))
	}
}

func (o *opener) forward(ctx context.Context, c *net.TCPConn) {
	o.mu.Lock()
	id := o.sharer
	o.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	st, err := o.n.host.NewStream(sctx, id, tcpProto)
	cancel()
	if err != nil {
		c.Close()
		if now := time.Now().UnixNano(); now-o.lastWarn.Swap(now) > int64(10*time.Second) {
			o.rep.Warn("Couldn't open a tunnel connection " + sDim.Render("· is the sharer still connected?"))
		}
		return
	}
	o.stats.conns.Add(1)
	o.stats.active.Add(1)
	defer o.stats.active.Add(-1)
	pipe(st, c,
		func(b []byte) { o.stats.in.Add(int64(len(b))) },
		func(b []byte) { o.stats.out.Add(int64(len(b))) })
}

// listenLocal listens on 127.0.0.1 only (never 0.0.0.0: the tunnel is for
// this machine, not its whole network). If the port is taken and the user
// didn't ask for it explicitly, the next free port is used.
func listenLocal(port int, explicit bool) (net.Listener, error) {
	try := func(p int) net.Listener {
		if inUse(p) {
			return nil
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			return nil
		}
		return ln
	}
	if ln := try(port); ln != nil {
		return ln, nil
	}
	if explicit {
		return nil, withHint("Pick another one with --port, or stop whatever is using it.",
			"Port %d is already in use on this machine.", port)
	}
	for p := port + 1; p <= min(port+100, 65535); p++ {
		if ln := try(p); ln != nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// inUse reports whether something already accepts connections on the port.
// (On Windows binding 127.0.0.1:N can succeed even when another program
// listens on 0.0.0.0:N, which would silently shadow it.)
func inUse(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 300*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
	}
	return false
}
