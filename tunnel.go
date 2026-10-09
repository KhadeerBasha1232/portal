package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
)

// stats are the live counters shown on the dashboards. "in" is bytes that
// arrived over the tunnel, "out" is bytes sent into it.
type stats struct {
	in, out  atomic.Int64
	active   atomic.Int64
	conns    atomic.Int64
	requests atomic.Int64
}

// pipe joins a libp2p stream and a TCP connection until both directions are
// done. Each direction is half-closed on EOF, so protocols that shut down
// one side first (HTTP/1.0, many database clients) work. The taps see a
// copy of every chunk after it was forwarded; they must not block.
func pipe(st network.Stream, c *net.TCPConn, fromStream, fromConn func([]byte)) {
	var wg sync.WaitGroup
	var once sync.Once
	abort := func() {
		once.Do(func() {
			st.Reset()
			c.Close()
		})
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := copyTap(c, st, fromStream); err != nil {
			abort()
			return
		}
		c.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		if err := copyTap(st, c, fromConn); err != nil {
			abort()
			return
		}
		st.CloseWrite()
	}()
	wg.Wait()
	st.Close()
	c.Close()
}

func copyTap(dst io.Writer, src io.Reader, tap func([]byte)) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			tap(buf[:n])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// hinted is an error with a "how to fix it" line printed under it.
type hinted struct{ msg, hint string }

func (h hinted) Error() string { return h.msg }

func withHint(hint, format string, args ...any) error {
	return hinted{msg: fmt.Sprintf(format, args...), hint: hint}
}

func asHint(err error, h *hinted) bool { return errors.As(err, h) }

// parseTarget turns "3000", ":3000" or "host:3000" into a dial address and
// the port number.
func parseTarget(s string) (addr string, port int, err error) {
	host, p := "localhost", s
	if h, pp, err := net.SplitHostPort(s); err == nil {
		if h != "" {
			host = h
		}
		p = pp
	}
	port, err = strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, withHint("Examples: portal share 3000 · portal share 192.168.1.20:5432",
			"%q is not a port or host:port", s)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), port, nil
}

// checkTarget makes sure something is listening before we hand out a code.
func checkTarget(addr string, port int) error {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		c.Close()
		return nil
	}
	host, _, _ := net.SplitHostPort(addr)
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return withHint("Start your app first, then run portal share again.",
			"Nothing is listening on port %d.", port)
	}
	return withHint("Is the service running, and reachable from this machine?",
		"Can't reach %s.", addr)
}
