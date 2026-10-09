package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
)

// Two libp2p protocols:
//   - controlProto: one long-lived stream per guest. Carries the SPAKE2
//     handshake, then encrypted hello/ping/bye messages.
//   - tcpProto: one NEW stream per forwarded TCP connection. Only accepted
//     from peer IDs that passed the handshake (see sharer.handleTCP).
const (
	controlProto = "/portal/1.0.0/control"
	tcpProto     = "/portal/1.0.0/tcp"
)

// node is a throwaway libp2p peer (fresh identity every run).
//
// How two machines find each other without a server of our own:
//   - Same LAN: mDNS multicast, instant.
//   - Internet: both join the public IPFS DHT. The sharer publishes "I have
//     nameplate N" there; the guest looks it up.
//   - NAT: the sharer reserves a slot on a public libp2p relay (random IPFS
//     nodes run limited relays). The guest dials through it, then both
//     sides hole-punch (DCUtR) to a direct connection. Tunnel bytes never go
//     through the relay: libp2p refuses to open our streams on a "limited"
//     (relayed) connection, and public relays only allow ~128 KB anyway.
type node struct {
	host host.Host
	dht  *dht.IpfsDHT
	disc *drouting.RoutingDiscovery
}

func newNode(ctx context.Context, isSharer bool) (*node, error) {
	var dhtRef atomic.Pointer[dht.IpfsDHT]

	opts := []libp2p.Option{
		libp2p.NATPortMap(),         // ask the router for a port via UPnP/NAT-PMP
		libp2p.EnableHolePunching(), // DCUtR: upgrade relayed conns to direct
	}
	if isSharer {
		// Find relay candidates among the DHT peers we already know.
		peerSource := func(ctx context.Context, num int) <-chan peer.AddrInfo {
			ch := make(chan peer.AddrInfo, num)
			go func() {
				defer close(ch)
				d := dhtRef.Load()
				if d == nil {
					return
				}
				for _, id := range d.RoutingTable().ListPeers() {
					if num == 0 {
						return
					}
					ai := d.Host().Peerstore().PeerInfo(id)
					if len(ai.Addrs) == 0 {
						continue
					}
					select {
					case ch <- ai:
						num--
					case <-ctx.Done():
						return
					}
				}
			}()
			return ch
		}
		opts = append(opts,
			libp2p.EnableAutoRelayWithPeerSource(peerSource,
				autorelay.WithBootDelay(0),
				autorelay.WithMinCandidates(2),
				autorelay.WithNumRelays(2),
			),
			// Assume we're behind NAT so relay addresses are published right
			// away instead of after AutoNAT probing. Direct addresses still
			// get used when they work (LAN, hole punching).
			libp2p.ForceReachabilityPrivate(),
		)
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}
	d, err := dht.New(h, dht.Mode(dht.ModeClient), dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...))
	if err != nil {
		h.Close()
		return nil, err
	}
	dhtRef.Store(d)
	return &node{host: h, dht: d, disc: drouting.NewRoutingDiscovery(d)}, nil
}

func (n *node) Close() {
	n.dht.Close()
	n.host.Close()
}

// joinDHT connects to the public bootstrap nodes and fills the routing table.
func (n *node) joinDHT(ctx context.Context) {
	var wg sync.WaitGroup
	for _, ai := range dht.GetDefaultBootstrapPeerAddrInfos() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			n.host.Connect(cctx, ai)
		}()
	}
	wg.Wait()
	n.dht.Bootstrap(ctx)
	waitFor(ctx, 20*time.Second, func() bool { return n.dht.RoutingTable().Size() >= 5 })
}

// waitForRelay waits until we hold a relay reservation (a /p2p-circuit address).
func (n *node) waitForRelay(ctx context.Context, timeout time.Duration) bool {
	return waitFor(ctx, timeout, func() bool {
		for _, a := range n.host.Addrs() {
			if strings.Contains(a.String(), "/p2p-circuit") {
				return true
			}
		}
		return false
	})
}

func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return true
}

func dhtNamespace(nameplate string) string { return "portal/v1/" + nameplate }

func mdnsService(nameplate string) string { return "_portal-" + nameplate + "._udp" }

// startMDNS announces us on the LAN and reports other portal peers with
// the same nameplate to found.
func (n *node) startMDNS(nameplate string, found func(peer.AddrInfo)) (func(), error) {
	svc := mdns.NewMdnsService(n.host, mdnsService(nameplate), notifee(found))
	if err := svc.Start(); err != nil {
		return nil, err
	}
	return func() { svc.Close() }, nil
}

type notifee func(peer.AddrInfo)

func (f notifee) HandlePeerFound(ai peer.AddrInfo) { f(ai) }
