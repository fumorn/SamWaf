package wafhttpserver

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

type extEntry struct {
	typ  uint16
	data []byte
}

// buildClientHello assembles a single-record TLS ClientHello for known-answer tests.
func buildClientHello(version uint16, ciphers []uint16, exts []extEntry) []byte {
	body := make([]byte, 0, 256)
	body = append(body, byte(version>>8), byte(version))
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0)                   // empty session id
	csBytes := len(ciphers) * 2              // cipher_suites length is in bytes
	body = append(body, byte(csBytes>>8), byte(csBytes))
	for _, c := range ciphers {
		body = append(body, byte(c>>8), byte(c))
	}
	body = append(body, 1, 0) // compression: null
	var extBuf bytes.Buffer
	for _, e := range exts {
		extBuf.Write([]byte{byte(e.typ >> 8), byte(e.typ), byte(len(e.data) >> 8), byte(len(e.data))})
		extBuf.Write(e.data)
	}
	body = append(body, byte(extBuf.Len()>>8), byte(extBuf.Len()))
	body = append(body, extBuf.Bytes()...)
	hdr := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	n := len(hdr) + len(body)
	rec := []byte{0x16, 0x03, 0x01, byte(n >> 8), byte(n)}
	return append(append(rec, hdr...), body...)
}

func u16list(vals ...uint16) []byte {
	n := len(vals) * 2
	out := []byte{byte(n >> 8), byte(n)}
	for _, v := range vals {
		out = append(out, byte(v>>8), byte(v))
	}
	return out
}

func u8list(vals ...uint8) []byte {
	return append([]byte{byte(len(vals))}, vals...)
}

// referenceJA3 computes the JA3 digest independently of the production code,
// but from explicit, hand-checked lists (no shared helper).
func referenceJA3(ver uint16, ciphers, exts, curves, formats []uint16) string {
	sortu := func(vs []uint16) []uint16 {
		out := append([]uint16(nil), vs...)
		slices.Sort(out)
		return out
	}
	ciphers, exts, curves, formats = sortu(ciphers), sortu(exts), sortu(curves), sortu(formats)
	join := func(vs []uint16) string {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = fmt.Sprint(v)
		}
		return strings.Join(parts, "-")
	}
	ja3 := fmt.Sprintf("%d,%s,%s,%s,%s", ver, join(ciphers), join(exts), join(curves), join(formats))
	sum := md5.Sum([]byte(ja3))
	return "ja3:" + hex.EncodeToString(sum[:])
}

// TestComputeFingerprintKnownAnswer pins the digest against an independent
// construction, including GREASE stripping and ALPN extraction.
func TestComputeFingerprintKnownAnswer(t *testing.T) {
	hello := buildClientHello(0x0303,
		[]uint16{0x1301, 0x0a0a, 0xc02f, 0x1a1a},
		[]extEntry{
			{0x0a0a, nil}, // GREASE extension entry
			{0x000a, u16list(0x001d, 0x2a2a, 0x0017)}, // curves incl. GREASE
			{0x000b, u8list(0)},                       // point formats
			{0x0010, []byte{0, 3, 2, 'h', '2'}},       // ALPN list: 1 len byte + "h2"
		})
	record := hello[5:]
	total, ok := handshakeTotal(record)
	if !ok {
		t.Fatal("not a ClientHello record")
	}
	got := computeFingerprint(record[4:total])
	// Lists are normalized to sorted unique sets before hashing.
	want := referenceJA3(0x0303,
		[]uint16{0x1301, 0xc02f},
		[]uint16{0x000a, 0x000b, 0x0010},
		[]uint16{0x0017, 0x001d},
		[]uint16{0}) + ";alpn:h2"
	if got != want {
		t.Fatalf("fingerprint mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestComputeFingerprintRejectsMalformed trims fields and demands "".
func TestComputeFingerprintRejectsMalformed(t *testing.T) {
	hello := buildClientHello(0x0303, []uint16{0x1301},
		[]extEntry{{0x000a, u16list(0x001d)}})
	record := hello[5:]
	total, ok := handshakeTotal(record)
	if !ok {
		t.Fatal("setup")
	}
	body := record[4:total]
	for cut := 0; cut < len(body); cut++ {
		if fp := computeFingerprint(body[:cut]); fp != "" {
			t.Fatalf("cut %d produced fingerprint %q", cut, fp)
		}
	}
	if fp := computeFingerprint(nil); fp != "" {
		t.Fatal("nil produced fingerprint")
	}
}

func TestHandshakeTotalAndIsGREASE(t *testing.T) {
	if total, ok := handshakeTotal([]byte{0x01, 0, 0, 4, 1, 2, 3, 4}); !ok || total != 8 {
		t.Fatalf("handshakeTotal = %d %v", total, ok)
	}
	if _, ok := handshakeTotal([]byte{0x02, 0, 0, 4}); ok {
		t.Fatal("non-ClientHello accepted")
	}
	for _, v := range []uint16{0x0a0a, 0x1a1a, 0x2a2a, 0xfafa} {
		if !isGREASE(v) {
			t.Fatalf("%x should be GREASE", v)
		}
	}
	for _, v := range []uint16{0x1301, 0x000a, 0x001d, 0x0a0b} {
		if isGREASE(v) {
			t.Fatalf("%x should not be GREASE", v)
		}
	}
}

// pushHello feeds a raw byte stream through a fingerprintConn in tiny chunks
// and returns everything crypto/tls would see plus the computed fingerprint.
func pushHello(t *testing.T, hello []byte, chunk int) ([]byte, string) {
	t.Helper()
	raw, client := net.Pipe()
	wrapped := newFingerprintConn(&chunkConn{Conn: raw, size: chunk})
	go func() {
		client.Write(hello)
		client.Close()
	}()
	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := wrapped.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	return got, wrapped.fingerprint()
}

type chunkConn struct {
	net.Conn
	size int
}

func (c *chunkConn) Read(p []byte) (int, error) {
	if len(p) > c.size {
		p = p[:c.size]
	}
	return c.Conn.Read(p)
}

// TestFingerprintConnReplayAndFragmentation: bytes pass through unchanged at
// every chunk size, and the fingerprint is chunking-independent.
func TestFingerprintConnReplayAndFragmentation(t *testing.T) {
	hello := buildClientHello(0x0303, []uint16{0x1301, 0xc02f},
		[]extEntry{{0x000a, u16list(0x001d)}, {0x000b, u8list(0)}, {0x0010, []byte{0, 3, 2, 'h', '2'}}})
	var reference string
	for _, chunk := range []int{1, 3, 7, 64, 1 << 20} {
		got, fp := pushHello(t, hello, chunk)
		if !bytes.Equal(got, hello) {
			t.Fatalf("chunk %d: replay mismatch (%d vs %d bytes)", chunk, len(got), len(hello))
		}
		if fp == "" {
			t.Fatalf("chunk %d: no fingerprint", chunk)
		}
		if reference == "" {
			reference = fp
		} else if fp != reference {
			t.Fatalf("chunk %d: fingerprint differs: %q vs %q", chunk, fp, reference)
		}
	}
	if !strings.Contains(reference, ";alpn:h2") {
		t.Fatalf("ALPN missing: %q", reference)
	}
}

// TestFingerprintConnDifferentHellos: two different ClientHello shapes must
// produce two different JA3 digests.
func TestFingerprintConnDifferentHellos(t *testing.T) {
	helloA := buildClientHello(0x0303, []uint16{0x1301, 0xc02f},
		[]extEntry{{0x000a, u16list(0x001d)}, {0x000b, u8list(0)}})
	helloB := buildClientHello(0x0303, []uint16{0x1301, 0xc02c},
		[]extEntry{{0x000b, u8list(0)}, {0x000a, u16list(0x0017, 0x001d)}})
	_, fpA := pushHello(t, helloA, 8)
	_, fpB := pushHello(t, helloB, 8)
	if fpA == "" || fpB == "" {
		t.Fatal("missing fingerprint")
	}
	if fpA == fpB {
		t.Fatalf("different hellos produced identical JA3: %q", fpA)
	}
}

// TestPlainBytesPassthrough: a non-TLS stream must pass through unchanged
// with no fingerprint, including mid-stream garbage.
func TestPlainBytesPassthrough(t *testing.T) {
	raw, client := net.Pipe()
	wrapped := newFingerprintConn(raw)
	payload := []byte("GET /x HTTP/1.1\r\nHost: a\r\n\r\nbody")
	go func() {
		client.Write(payload)
		client.Close()
	}()
	got, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("passthrough altered: %q", got)
	}
	if wrapped.fingerprint() != "" {
		t.Fatalf("plain stream got fingerprint %q", wrapped.fingerprint())
	}
}

// TestResumptionExtensionsNormalizeOut: a client that offers session
// resumption (session_ticket/pre_shared_key/psk_key_exchange_modes/early_data)
// must get the SAME fingerprint as its full-handshake ClientHello, while a
// different client must still differ.
func TestResumptionExtensionsNormalizeOut(t *testing.T) {
	base := buildClientHello(0x0303, []uint16{0x1301, 0xc02f},
		[]extEntry{{0x000a, u16list(0x001d)}, {0x000b, u8list(0)}})
	resumed := buildClientHello(0x0303, []uint16{0x1301, 0xc02f},
		[]extEntry{
			{0x0023, []byte{1, 2, 3}}, // session_ticket
			{0x000a, u16list(0x001d)},
			{0x0029, []byte{9}}, // pre_shared_key
			{0x000b, u8list(0)}, // order shifted by resumption exts
			{0x0033, []byte{1}}, // psk_key_exchange_modes
		})
	other := buildClientHello(0x0303, []uint16{0x1301, 0xc02c},
		[]extEntry{{0x000a, u16list(0x001d)}, {0x000b, u8list(0)}})
	_, fpBase := pushHello(t, base, 8)
	_, fpResumed := pushHello(t, resumed, 8)
	_, fpOther := pushHello(t, other, 8)
	if fpBase == "" || fpResumed == "" || fpOther == "" {
		t.Fatal("missing fingerprint")
	}
	if fpBase != fpResumed {
		t.Fatalf("resumption changed the fingerprint:\n base    %q\n resumed %q", fpBase, fpResumed)
	}
	if fpBase == fpOther {
		t.Fatal("different clients produced the same fingerprint")
	}
}

// TestTLSHandshakeFingerprint: a real crypto/tls handshake through the
// wrapped listener captures a non-empty fingerprint with ALPN, and the HTTP
// handler sees it through ConnContext + RequestFingerprint.
func TestTLSHandshakeFingerprint(t *testing.T) {
	cert := selfSignCert(t)
	fps := make(chan string, 4)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fps <- RequestFingerprint(r)
			w.WriteHeader(200)
		}),
		ConnContext: ConnContext,
		TLSConfig:   &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(FingerprintListener(ln), "", "")
	defer srv.Close()

	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"http/1.1"},
	})
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	defer c.Close()
	if got := c.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("ALPN negotiated %q", got)
	}
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case fp := <-fps:
		if fp == "" {
			t.Fatal("handler saw no fingerprint")
		}
		if !strings.HasPrefix(fp, "ja3:") {
			t.Fatalf("fingerprint shape %q", fp)
		}
		if !strings.Contains(fp, ";alpn:http/1.1") {
			t.Fatalf("ALPN not captured in fingerprint: %q", fp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler never saw the request")
	}
}

func selfSignCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestPlainHTTPServerPassthrough: a plain HTTP server behind the wrapper
// serves normally and reports no fingerprint.
func TestPlainHTTPServerPassthrough(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fp := RequestFingerprint(r); fp != "" {
			t.Errorf("plain request got fingerprint %q", fp)
		}
		w.WriteHeader(200)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = FingerprintListener(ln)
	srv.Start()
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestConnContextInjection verifies the context helpers directly.
func TestConnContextInjection(t *testing.T) {
	ctx := WithFingerprint(context.Background(), "ja3:abc;alpn:h2")
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	if got := RequestFingerprint(r); got != "ja3:abc;alpn:h2" {
		t.Fatalf("got %q", got)
	}
	if got := RequestFingerprint(httptest.NewRequest("GET", "/", nil)); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if ctx2 := WithFingerprint(context.Background(), ""); ctx2.Value(fingerprintKey) != nil {
		t.Fatal("empty fingerprint must not be stored")
	}
}
