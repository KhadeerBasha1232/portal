package main

import (
	"fmt"
	"strings"
	"testing"
)

func collect() (*httpSniffer, *[]reqEntry) {
	var got []reqEntry
	return newHTTPSniffer(func(r reqEntry) { got = append(got, r) }), &got
}

// feed sends s in small pieces, like a slow network would.
func feed(f func([]byte), s string, step int) {
	for len(s) > 0 {
		n := min(step, len(s))
		f([]byte(s[:n]))
		s = s[n:]
	}
}

func TestSnifferKeepAlive(t *testing.T) {
	for _, step := range []int{1, 3, 7, 1000} {
		sn, got := collect()
		feed(sn.request, "GET /api/users HTTP/1.1\r\nHost: x\r\n\r\n"+
			"POST /api/login HTTP/1.1\r\nContent-Length: 11\r\n\r\nhello world"+
			"PUT /upload HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6;x=y\r\n world\r\n0\r\n\r\n"+
			"HEAD /x HTTP/1.1\r\n\r\n", step)
		feed(sn.response, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n[]"+
			"HTTP/1.1 100 Continue\r\n\r\n"+
			"HTTP/1.1 401 Unauthorized\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nno!\r\n0\r\nX-Trailer: 1\r\n\r\n"+
			"HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n"+
			"HTTP/1.1 200 OK\r\nContent-Length: 999\r\n\r\n", step)
		want := []string{"GET /api/users 200", "POST /api/login 401", "PUT /upload 201", "HEAD /x 200"}
		if len(*got) != len(want) {
			t.Fatalf("step %d: got %d entries %+v", step, len(*got), *got)
		}
		for i, r := range *got {
			if s := fmt.Sprintf("%s %s %d", r.Method, r.Path, r.Status); s != want[i] {
				t.Errorf("step %d entry %d: got %q want %q", step, i, s, want[i])
			}
		}
	}
}

func TestSnifferIgnoresNonHTTP(t *testing.T) {
	for _, first := range []string{
		"\x00\x00\x00\x08\x04\xd2\x16\x2f",         // Postgres SSLRequest
		"SSH-2.0-OpenSSH_9.6\r\n",                  // SSH
		"\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03", // TLS ClientHello
		"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",         // HTTP/2 prior knowledge
		"get / HTTP/1.1\r\n\r\n",                   // lowercase method
	} {
		sn, got := collect()
		sn.request([]byte(first))
		sn.response([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		if len(*got) != 0 || !sn.disabled() {
			t.Errorf("%q: should be ignored, got %+v", first, *got)
		}
	}
}

func TestSnifferStopsAfterUpgrade(t *testing.T) {
	sn, got := collect()
	sn.request([]byte("GET /ws HTTP/1.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	sn.response([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n\x81\x05hello"))
	sn.request([]byte("\x81\x85\x00\x00\x00\x00hello GET /fake HTTP/1.1\r\n\r\n"))
	sn.response([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	if len(*got) != 1 || (*got)[0].Status != 101 {
		t.Fatalf("got %+v", *got)
	}
}

func TestSnifferHugeHeaderGivesUp(t *testing.T) {
	sn, got := collect()
	sn.request([]byte("GET / HTTP/1.1\r\nX: " + strings.Repeat("a", maxHead+10)))
	if !sn.disabled() || len(*got) != 0 {
		t.Fatal("should give up on a huge header")
	}
}
