package main

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The HTTP request log is a passive tap: forwarded bytes are copied into a
// sniffer AFTER they were already written to the other side, and the sniffer
// never changes, delays or blocks them. It understands just enough HTTP/1.x
// framing (request/status lines, Content-Length, chunked bodies) to follow a
// keep-alive connection from one request to the next. The moment anything
// doesn't look like HTTP/1.x (Postgres, SSH, TLS, HTTP/2, a websocket after
// its 101 upgrade) it switches itself off for that connection for good.

type reqEntry struct {
	At     time.Time
	Method string
	Path   string
	Status int
	Took   time.Duration
}

type pendingReq struct {
	method, path string
	at           time.Time
}

// httpSniffer follows one TCP connection. request() gets the bytes going to
// the app, response() the bytes coming back. They are called from two
// goroutines.
type httpSniffer struct {
	req, resp httpParser
	onEntry   func(reqEntry)

	mu      sync.Mutex
	pending []pendingReq
	off     bool
}

func newHTTPSniffer(onEntry func(reqEntry)) *httpSniffer {
	s := &httpSniffer{onEntry: onEntry}
	s.req = httpParser{onHead: s.requestHead}
	s.resp = httpParser{isResp: true, onHead: s.responseHead}
	return s
}

func (s *httpSniffer) request(b []byte)  { s.feed(&s.req, b) }
func (s *httpSniffer) response(b []byte) { s.feed(&s.resp, b) }

func (s *httpSniffer) feed(p *httpParser, b []byte) {
	if s.disabled() || p.dead {
		return
	}
	p.write(b)
	if p.dead {
		s.disable()
	}
}

func (s *httpSniffer) disabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.off
}

func (s *httpSniffer) disable() {
	s.mu.Lock()
	s.off = true
	s.pending = nil
	s.mu.Unlock()
}

// requestHead is called with a complete request head. It returns how the
// body is framed.
func (s *httpSniffer) requestHead(first string, h headers) bodyMode {
	method, rest, _ := strings.Cut(first, " ")
	target, version, ok := strings.Cut(rest, " ")
	if !ok || !isToken(method) || !strings.HasPrefix(version, "HTTP/1.") || target == "" {
		return bodyInvalid
	}
	s.mu.Lock()
	if len(s.pending) < 64 {
		s.pending = append(s.pending, pendingReq{method, target, time.Now()})
	}
	s.mu.Unlock()
	switch {
	case h.chunked:
		return bodyChunked
	case h.length > 0:
		return bodyLength
	}
	return bodyNone
}

func (s *httpSniffer) responseHead(first string, h headers) bodyMode {
	version, rest, _ := strings.Cut(first, " ")
	code, _, _ := strings.Cut(rest, " ")
	status, err := strconv.Atoi(code)
	if !strings.HasPrefix(version, "HTTP/1.") || err != nil || len(code) != 3 {
		return bodyInvalid
	}
	if status >= 100 && status < 200 && status != 101 {
		return bodyNone // 100 Continue etc.: the real response follows
	}
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return bodyInvalid
	}
	r := s.pending[0]
	s.pending = s.pending[1:]
	s.mu.Unlock()

	s.onEntry(reqEntry{At: r.at, Method: r.method, Path: r.path, Status: status, Took: time.Since(r.at)})
	switch {
	case status == 101:
		return bodyInvalid // protocol switch (websocket): stop looking
	case r.method == "HEAD" || status == 204 || status == 304:
		return bodyNone
	case h.chunked:
		return bodyChunked
	case h.length >= 0:
		return bodyLength
	}
	return bodyUntilClose
}

func isToken(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- parser

type bodyMode int

const (
	bodyNone bodyMode = iota
	bodyLength
	bodyChunked
	bodyUntilClose
	bodyInvalid
)

type headers struct {
	length  int64 // -1 if absent
	chunked bool
}

type parserState int

const (
	stHead parserState = iota
	stBody
	stChunkSize
	stChunkData
	stChunkCRLF
	stTrailer
	stUntilClose
)

const maxHead = 64 << 10

type httpParser struct {
	isResp bool
	onHead func(first string, h headers) bodyMode
	dead   bool
	state  parserState
	buf    []byte
	remain int64
}

func (p *httpParser) write(b []byte) {
	for len(b) > 0 && !p.dead {
		switch p.state {
		case stHead:
			p.buf = append(p.buf, b...)
			b = nil
			if !p.plausible() {
				p.dead = true
				return
			}
			i := bytes.Index(p.buf, []byte("\r\n\r\n"))
			if i < 0 {
				if len(p.buf) > maxHead {
					p.dead = true
				}
				return
			}
			head, rest := string(p.buf[:i]), p.buf[i+4:]
			p.buf = nil
			p.startBody(head)
			b = rest
		case stBody, stChunkData:
			n := min(p.remain, int64(len(b)))
			p.remain -= n
			b = b[n:]
			if p.remain == 0 {
				if p.state == stBody {
					p.state = stHead
				} else {
					p.state, p.remain = stChunkCRLF, 2
				}
			}
		case stChunkCRLF:
			n := min(p.remain, int64(len(b)))
			p.remain -= n
			b = b[n:]
			if p.remain == 0 {
				p.state = stChunkSize
			}
		case stChunkSize, stTrailer:
			line, rest, ok := p.line(b)
			b = rest
			if !ok {
				continue
			}
			if p.state == stTrailer {
				if line == "" {
					p.state = stHead
				}
				continue
			}
			sz, _, _ := strings.Cut(line, ";")
			n, err := strconv.ParseInt(strings.TrimSpace(sz), 16, 64)
			switch {
			case err != nil || n < 0:
				p.dead = true
			case n == 0:
				p.state = stTrailer
			default:
				p.state, p.remain = stChunkData, n
			}
		case stUntilClose:
			return
		}
	}
}

// line collects one CRLF-terminated line across writes.
func (p *httpParser) line(b []byte) (string, []byte, bool) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		p.buf = append(p.buf, b...)
		if len(p.buf) > 4096 {
			p.dead = true
		}
		return "", nil, false
	}
	l := string(append(p.buf, b[:i]...))
	p.buf = nil
	return strings.TrimSuffix(l, "\r"), b[i+1:], true
}

// plausible rejects non-HTTP traffic early, from the first few bytes,
// instead of buffering 64 KB of a database protocol first.
func (p *httpParser) plausible() bool {
	b := p.buf
	if p.isResp {
		n := min(len(b), 7)
		return string(b[:n]) == "HTTP/1."[:n]
	}
	for i, c := range b {
		if c == ' ' {
			return i > 0
		}
		if c < 'A' || c > 'Z' || i >= 16 {
			return false
		}
	}
	return true
}

func (p *httpParser) startBody(head string) {
	first, rest, _ := strings.Cut(head, "\r\n")
	h := headers{length: -1}
	for _, l := range strings.Split(rest, "\r\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "content-length":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				p.dead = true
				return
			}
			h.length = n
		case "transfer-encoding":
			h.chunked = strings.Contains(strings.ToLower(v), "chunked")
		}
	}
	switch p.onHead(first, h) {
	case bodyNone:
		p.state = stHead
	case bodyLength:
		p.state, p.remain = stBody, h.length
		if h.length == 0 {
			p.state = stHead
		}
	case bodyChunked:
		p.state = stChunkSize
	case bodyUntilClose:
		p.state = stUntilClose
	default:
		p.dead = true
	}
}
