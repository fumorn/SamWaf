// Client TLS fingerprint (normalized JA3 + ALPN) capture for pass-credential
// binding.
//
// Site TLS listeners peek the client's first ClientHello before Go's
// crypto/tls consumes it and expose the digest to HTTP handlers through the
// request context. Plain-HTTP traffic, malformed hellos and oversized probes
// pass through with no fingerprint: capture must never break a handshake. A
// ClientHello spanning multiple TLS records is not reassembled (extremely
// rare); it simply yields no fingerprint.
//
// The digest deliberately deviates from stock JA3: GREASE values and
// session-resumption extensions (session_ticket, pre_shared_key,
// psk_key_exchange_modes, early_data) are removed, and every list is sorted
// and deduplicated (JA4-style). Modern browsers randomize cipher, extension
// and curve ORDER per connection and toggle resumption extensions on later
// connections, so an order-sensitive digest is unstable across ONE client's
// own connections and would reject its own pass. The sorted value SETS stay
// stable for one client across connections while still discriminating
// different clients (browsers vs curl vs Go).
//
// Scope: TCP TLS only. HTTP/3 (QUIC) and non-TLS listeners are not captured;
// RequestFingerprint returns "" there and strict binding fails closed.
package wafhttpserver

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

type fingerprintKeyType struct{}

var fingerprintKey fingerprintKeyType

// WithFingerprint returns a context carrying fp. An empty fp is equivalent to
// no fingerprint. ConnContext and tests share this single write path.
func WithFingerprint(ctx context.Context, fp string) context.Context {
	if ctx == nil || fp == "" {
		return ctx
	}
	return context.WithValue(ctx, fingerprintKey, fp)
}

// fingerprintResolver is stored lazily: tls.Server handshakes on first Read,
// AFTER http.Server.ConnContext runs, so the connection itself goes into the
// context and the fingerprint is resolved when the request actually reads it.
type fingerprintResolver interface{ fingerprint() string }

// RequestFingerprint returns the captured "ja3:<md5hex>[;alpn:<proto>]" value
// for the request, or "" when the connection was not TLS or capture failed.
func RequestFingerprint(r *http.Request) string {
	if r == nil {
		return ""
	}
	switch v := r.Context().Value(fingerprintKey).(type) {
	case string: // WithFingerprint direct injection (tests, wiring)
		return v
	case fingerprintResolver: // captured connection, resolved post-handshake
		return v.fingerprint()
	}
	return ""
}

// ConnContext adapts an http.Server.ConnContext callback: it unwraps the
// server's tls.Conn to the captured connection and stores it for lazy
// resolution. The fingerprint itself is only known after the lazy TLS
// handshake completes on first Read, so it must not be read eagerly here.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if ctx == nil {
		return ctx
	}
	if tc, ok := c.(*tls.Conn); ok {
		if fc, ok := tc.NetConn().(*fingerprintConn); ok {
			return context.WithValue(ctx, fingerprintKey, fc)
		}
		return ctx
	}
	if fc, ok := c.(*fingerprintConn); ok {
		return context.WithValue(ctx, fingerprintKey, fc)
	}
	return ctx
}

// FingerprintListener wraps a listener so every accepted connection captures
// the client fingerprint. Wrap the raw listener before TLS serving.
func FingerprintListener(l net.Listener) net.Listener {
	if l == nil {
		return nil
	}
	return &fingerprintListener{Listener: l}
}

type fingerprintListener struct{ net.Listener }

func (l *fingerprintListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newFingerprintConn(c), nil
}

// maxClientHello bounds probe memory; a legitimate ClientHello never reaches
// this size.
const maxClientHello = 1 << 16

type fingerprintConn struct {
	net.Conn
	probed atomic.Bool
	replay bytes.Reader
	fp     atomic.Value // string
}

func newFingerprintConn(c net.Conn) *fingerprintConn {
	return &fingerprintConn{Conn: c}
}

func (c *fingerprintConn) fingerprint() string {
	if v, ok := c.fp.Load().(string); ok {
		return v
	}
	return ""
}

// Read answers from the replayed probe bytes first; the original bytes are
// always returned unchanged to crypto/tls afterwards.
func (c *fingerprintConn) Read(p []byte) (int, error) {
	if c.probed.Load() {
		if c.replay.Len() > 0 {
			return c.replay.Read(p)
		}
		return c.Conn.Read(p)
	}
	return c.probe(p)
}

// probe accumulates the first ClientHello across reads, computes the
// fingerprint, then replays everything consumed. Every path either returns,
// or strictly grows the pending-need size, so the loop terminates. Probe-time
// read errors propagate unchanged after the captured bytes drain.
func (c *fingerprintConn) probe(p []byte) (int, error) {
	head := make([]byte, 0, 4096)
	passthrough := func() (int, error) { return c.finish("", head, p, nil) }
	need := 5 // TLS record header
	for {
		if len(head) >= 1 && head[0] != 0x16 {
			return passthrough() // not a handshake record: nothing to capture
		}
		if len(head) >= 5 {
			recLen := int(head[3])<<8 | int(head[4])
			if recLen < 1 || recLen > maxClientHello {
				return passthrough()
			}
			if len(head) >= 5+recLen {
				body := head[5 : 5+recLen]
				total, ok := handshakeTotal(body)
				if !ok {
					return passthrough() // first handshake message is not a ClientHello
				}
				if len(body) >= total {
					// body[4:total] strips the 4-byte handshake header: the ClientHello itself.
					return c.finish(computeFingerprint(body[4:total]), head, p, nil)
				}
				return passthrough() // spans multiple TLS records: documented limitation
			}
			need = 5 + recLen
		}
		if len(head) > maxClientHello {
			return passthrough()
		}
		chunk := make([]byte, need-len(head))
		n, err := c.Conn.Read(chunk)
		if n > 0 {
			head = append(head, chunk[:n]...)
		}
		if err != nil {
			return c.finish("", head, p, err)
		}
	}
}

// finish stores the fingerprint, marks probing complete and answers the
// pending Read from the replayed bytes.
func (c *fingerprintConn) finish(fp string, head []byte, p []byte, readErr error) (int, error) {
	c.probed.Store(true)
	if fp != "" {
		c.fp.Store(fp)
	}
	c.replay.Reset(head)
	n, err := c.replay.Read(p)
	if readErr != nil {
		return n, readErr
	}
	return n, err
}

// handshakeTotal returns the full handshake-message size (4-byte header
// included) when the buffer starts a ClientHello handshake record body.
func handshakeTotal(body []byte) (int, bool) {
	if len(body) < 4 || body[0] != 0x01 {
		return 0, false
	}
	l := int(body[1])<<16 | int(body[2])<<8 | int(body[3])
	if l <= 0 || l > maxClientHello {
		return 0, false
	}
	return 4 + l, true
}

// computeFingerprint parses a full ClientHello body into the standard JA3
// digest and the first offered ALPN protocol. Any malformed field stops
// parsing and yields "" rather than a partial fingerprint.
func computeFingerprint(body []byte) string {
	if len(body) < 40 {
		return ""
	}
	ver := int(body[0])<<8 | int(body[1])
	pos := 34
	sidLen := int(body[pos])
	pos++
	if pos+sidLen > len(body) {
		return ""
	}
	pos += sidLen
	if pos+2 > len(body) {
		return ""
	}
	csLen := int(body[pos])<<8 | int(body[pos+1])
	pos += 2
	if csLen%2 != 0 || pos+csLen > len(body) {
		return ""
	}
	var ciphers []uint16
	for i := 0; i+2 <= csLen; i += 2 {
		ciphers = append(ciphers, uint16(body[pos+i])<<8|uint16(body[pos+i+1]))
	}
	pos += csLen
	if pos >= len(body) {
		return ""
	}
	compLen := int(body[pos])
	pos++
	if pos+compLen > len(body) {
		return ""
	}
	pos += compLen
	if pos+2 > len(body) {
		return ""
	}
	extLen := int(body[pos])<<8 | int(body[pos+1])
	pos += 2
	end := pos + extLen
	if end > len(body) {
		return ""
	}
	var exts, curves, formats []uint16
	var alpn string
	for pos+4 <= end {
		et := uint16(body[pos])<<8 | uint16(body[pos+1])
		el := int(body[pos+2])<<8 | int(body[pos+3])
		pos += 4
		if pos+el > end {
			return ""
		}
		ext := body[pos : pos+el]
		exts = append(exts, et)
		switch et {
		case 0x000a: // supported_groups
			if len(ext) >= 2 {
				l := int(ext[0])<<8 | int(ext[1])
				if l%2 == 0 && 2+l <= len(ext) {
					for i := 2; i+2 <= 2+l; i += 2 {
						curves = append(curves, uint16(ext[i])<<8|uint16(ext[i+1]))
					}
				}
			}
		case 0x000b: // ec_point_formats
			if len(ext) >= 1 {
				l := int(ext[0])
				if 1+l <= len(ext) {
					for i := 1; i < 1+l; i++ {
						formats = append(formats, uint16(ext[i]))
					}
				}
			}
		case 0x0010: // ALPN: first offered protocol
			if len(ext) >= 4 {
				l := int(ext[0])<<8 | int(ext[1])
				if l >= 2 && 2+l <= len(ext) {
					pl := int(ext[2])
					if pl >= 1 && 3+pl <= len(ext) {
						alpn = string(ext[3 : 3+pl])
					}
				}
			}
		}
		pos += el
	}
	ciphers = filterGREASE16(ciphers)
	exts = filterGREASE16(exts)
	curves = filterGREASE16(curves)
	// Resumption-only extensions appear just when a client resumes a session,
	// so a stock digest would differ between the validating connection and
	// every later connection of the same browser, rejecting its own pass.
	exts = filterResumption16(exts)
	// Modern browsers (Chrome 110+) randomize the ORDER of cipher suites,
	// extensions and curves per connection as an anti-fingerprinting measure,
	// so any order-sensitive digest is unstable across one client's own
	// connections. Sort every list (JA4-style normalization): the value SETS
	// keep strong client-vs-client discrimination while staying stable for
	// one client across connections, resumption and order shuffles.
	ciphers = sortedUnique16(ciphers)
	exts = sortedUnique16(exts)
	curves = sortedUnique16(curves)
	formats = sortedUnique16(formats)
	ja3 := fmt.Sprintf("%d,%s,%s,%s,%s", ver,
		join16(ciphers), join16(exts), join16(curves), join16(formats))
	sum := md5.Sum([]byte(ja3))
	if alpn != "" {
		return "ja3:" + hex.EncodeToString(sum[:]) + ";alpn:" + alpn
	}
	return "ja3:" + hex.EncodeToString(sum[:])
}

func join16(vs []uint16) string {
	if len(vs) == 0 {
		return ""
	}
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(int(v))
	}
	return strings.Join(parts, "-")
}

func filterGREASE16(vs []uint16) []uint16 {
	out := make([]uint16, 0, len(vs))
	for _, v := range vs {
		if !isGREASE(v) {
			out = append(out, v)
		}
	}
	return out
}

// isGREASE matches the standard GREASE set 0x0a0a through 0xfafa: both bytes
// equal and the low nibble 0x0a.
func isGREASE(v uint16) bool {
	b := byte(v >> 8)
	return b == byte(v) && b&0x0f == 0x0a
}

// resumptionExtensions only appear when a client resumes a TLS session. They
// are not part of the client's stable fingerprint identity.
var resumptionExtensions = map[uint16]bool{
	0x0023: true, // session_ticket
	0x0029: true, // pre_shared_key
	0x002b: true, // early_data
	0x0033: true, // psk_key_exchange_modes
}

func filterResumption16(vs []uint16) []uint16 {
	out := make([]uint16, 0, len(vs))
	for _, v := range vs {
		if !resumptionExtensions[v] {
			out = append(out, v)
		}
	}
	return out
}

// sortedUnique16 returns the values ascending without duplicates.
func sortedUnique16(vs []uint16) []uint16 {
	seen := make(map[uint16]bool, len(vs))
	out := make([]uint16, 0, len(vs))
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
