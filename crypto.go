package main

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/schollz/pake/v3"
	"golang.org/x/crypto/chacha20poly1305"
)

// errBadCode means the PAKE finished but the two sides derived different
// keys: the guest typed a different code (or is an attacker guessing).
var errBadCode = errors.New("code mismatch")

const maxFrame = 1 << 20

func writeFrame(w io.Writer, b []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, fmt.Errorf("frame too large (%d bytes)", n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

// handshake runs SPAKE2 over the control stream and returns an encrypted
// control channel.
//
// The guest is PAKE party A (it opened the stream, so it speaks first); the
// sharer is party B. Both libp2p peer IDs are bound into the PAKE
// transcript, so a successful handshake proves "the peer with THIS peer ID
// knows the code". Each peer ID is already authenticated by the libp2p
// transport (Noise/TLS prove possession of the ID's private key), which is
// what lets the sharer later trust plain libp2p streams from that peer ID.
func handshake(rw io.ReadWriter, code string, isSharer bool, sharerID, guestID []byte) (*secureConn, error) {
	role := 0
	if isSharer {
		role = 1
	}
	p, err := pake.InitCurveWithIdentities([]byte(code), role, "p256", guestID, sharerID)
	if err != nil {
		return nil, err
	}

	if isSharer {
		msgA, err := readFrame(rw)
		if err != nil {
			return nil, err
		}
		if err := p.Update(msgA); err != nil {
			return nil, err
		}
		if err := writeFrame(rw, p.Bytes()); err != nil {
			return nil, err
		}
	} else {
		if err := writeFrame(rw, p.Bytes()); err != nil {
			return nil, err
		}
		msgB, err := readFrame(rw)
		if err != nil {
			return nil, err
		}
		if err := p.Update(msgB); err != nil {
			return nil, err
		}
	}
	key, err := p.SessionKey()
	if err != nil {
		return nil, err
	}

	// Split the PAKE key into independent keys with HKDF.
	confirmG := derive(key, "confirm guest")
	confirmS := derive(key, "confirm sharer")
	s2g := derive(key, "control sharer->guest")
	g2s := derive(key, "control guest->sharer")

	// Key confirmation: the guest proves it knows the key first. If the
	// codes differ the sharer learns nothing except "wrong", and says nothing.
	tagG := mac(confirmG)
	tagS := mac(confirmS)
	if isSharer {
		got, err := readFrame(rw)
		if err != nil {
			return nil, err
		}
		if !hmac.Equal(got, tagG) {
			return nil, errBadCode
		}
		if err := writeFrame(rw, tagS); err != nil {
			return nil, err
		}
		return newSecureConn(rw, s2g, g2s)
	}
	if err := writeFrame(rw, tagG); err != nil {
		return nil, err
	}
	got, err := readFrame(rw)
	if err != nil {
		// The sharer hangs up without a reply when the code is wrong.
		return nil, errBadCode
	}
	if !hmac.Equal(got, tagS) {
		return nil, errBadCode
	}
	return newSecureConn(rw, g2s, s2g)
}

func derive(secret []byte, label string) []byte {
	k, err := hkdf.Key(sha256.New, secret, []byte("portal v1"), label, 32)
	if err != nil {
		panic(err)
	}
	return k
}

func mac(key []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("portal key confirmation"))
	return m.Sum(nil)
}

// secureConn sends typed, encrypted control messages. Each direction has its
// own ChaCha20-Poly1305 key and a counter nonce, so messages can't be
// dropped, reordered, replayed or reflected without decryption failing.
// WriteMsg is safe to call from several goroutines; ReadMsg is not.
type secureConn struct {
	rw               io.ReadWriter
	send, recv       cipher.AEAD
	wmu              sync.Mutex
	sendSeq, recvSeq uint64
}

func newSecureConn(rw io.ReadWriter, sendKey, recvKey []byte) (*secureConn, error) {
	send, err := chacha20poly1305.New(sendKey)
	if err != nil {
		return nil, err
	}
	recv, err := chacha20poly1305.New(recvKey)
	if err != nil {
		return nil, err
	}
	return &secureConn{rw: rw, send: send, recv: recv}, nil
}

func nonce(seq uint64) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(n[4:], seq)
	return n
}

func (c *secureConn) WriteMsg(typ byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	plain := append([]byte{typ}, payload...)
	sealed := c.send.Seal(nil, nonce(c.sendSeq), plain, nil)
	c.sendSeq++
	return writeFrame(c.rw, sealed)
}

func (c *secureConn) ReadMsg() (byte, []byte, error) {
	sealed, err := readFrame(c.rw)
	if err != nil {
		return 0, nil, err
	}
	plain, err := c.recv.Open(nil, nonce(c.recvSeq), sealed, nil)
	if err != nil {
		return 0, nil, errors.New("decryption failed: data was tampered with")
	}
	c.recvSeq++
	if len(plain) == 0 {
		return 0, nil, errors.New("empty message")
	}
	return plain[0], plain[1:], nil
}
