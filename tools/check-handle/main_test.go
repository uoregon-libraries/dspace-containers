package main

import (
	"bytes"
	"crypto"
	"crypto/dsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"
)

// message builds a response message: header, body, and credential
func message(opCode, code uint32, body, credential []byte) []byte {
	m := be32(nil, opCode)
	m = be32(m, code)
	m = be32(m, 0) // flags
	m = append(m, 0, 1, 0, 0)
	m = be32(m, uint32(time.Now().Unix()))
	m = be32(m, uint32(len(body)))
	m = append(m, body...)
	m = be32(m, uint32(len(credential)))
	return append(m, credential...)
}

// resolution builds a successful resolution response with a URL value and,
// to be skipped over, an EMAIL value with a reference
func resolution(handle, u string) []byte {
	body := putString(nil, handle)
	body = be32(body, 2)
	for _, v := range []struct {
		index int
		typ   string
		data  string
		refs  int
	}{{100, "EMAIL", "nobody@example.org", 1}, {1, "URL", u, 0}} {
		body = be32(body, uint32(v.index))
		body = append(body, 0, 0, 0, 1, 0, 0, 0, 0, 100, 0x0a) // timestamp, TTL type and TTL, permissions
		body = putString(body, v.typ)
		body = putString(body, v.data)
		body = be32(body, uint32(v.refs))
		for range v.refs {
			body = putString(body, "0.NA/123456789")
			body = be32(body, 300)
		}
	}
	return message(opResolution, rcSuccess, body, nil)
}

// requestID pulls the request ID out of a request's envelope
func requestID(env []byte) uint32 { return binary.BigEndian.Uint32(env[8:]) }

func checkResolution(t *testing.T, cl client, i iface) {
	t.Helper()
	resp, err := cl.send(i, resolutionRequest("123456789/1"))
	if err != nil {
		t.Fatalf("%s: %s", i, err)
	}
	urls, err := urlsOf(resp)
	if err != nil {
		t.Fatalf("%s: %s", i, err)
	}
	if want := []string{"https://example.org/handle/123456789/1"}; !slices.Equal(urls, want) {
		t.Fatalf("%s: got %q, want %q", i, urls, want)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		env := make([]byte, envelopeSize)
		io.ReadFull(conn, env)
		io.ReadFull(conn, make([]byte, binary.BigEndian.Uint32(env[16:])))
		msg := resolution("123456789/1", "https://example.org/handle/123456789/1")
		conn.Write(append(envelope(requestID(env), len(msg)), msg...))
	}()

	cl := client{host: "127.0.0.1", timeout: time.Second}
	checkResolution(t, cl, iface{proto: "TCP", port: ln.Addr().(*net.TCPAddr).Port})
}

func TestUDP(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Answer in two packets, sent backwards, with a duplicate and a stray
	// packet from some other request mixed in
	go func() {
		buf := make([]byte, maxUDPPacket)
		_, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		id := requestID(buf)
		msg := resolution("123456789/1", "https://example.org/handle/123456789/1")
		packet := func(id, seq uint32, data []byte) []byte {
			env := envelope(id, len(msg))
			env[2] |= 0x20 // "truncated", as servers mark split responses
			binary.BigEndian.PutUint32(env[12:], seq)
			return append(env, data...)
		}
		half := len(msg) / 2
		for _, p := range [][]byte{
			packet(id+1, 0, msg[:half]),
			packet(id, 1, msg[half:]),
			packet(id, 1, msg[half:]),
			packet(id, 0, msg[:half]),
		} {
			conn.WriteTo(p, from)
		}
	}()

	cl := client{host: "127.0.0.1", timeout: time.Second}
	checkResolution(t, cl, iface{proto: "UDP", port: conn.LocalAddr().(*net.UDPAddr).Port})
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Like the real thing, answer with a web page unless asked otherwise
		if r.Method != "POST" || r.Header.Get("Accept") != mimeType {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html></html>")
			return
		}
		req, _ := io.ReadAll(r.Body)
		msg := resolution("123456789/1", "https://example.org/handle/123456789/1")
		w.Header().Set("Content-Type", mimeType)
		w.Write(append(envelope(requestID(req), len(msg)), msg...))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	p, _ := strconv.Atoi(port)
	checkResolution(t, client{host: host, timeout: time.Second}, iface{proto: "HTTP", port: p})
}

func TestErrorResponse(t *testing.T) {
	resp, err := parseResponse(message(opResolution, rcServerNotResp, putString(nil, "That prefix doesn't live here"), nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = urlsOf(resp)
	re, ok := err.(*responseError)
	if !ok || re.code != rcServerNotResp || re.message != "That prefix doesn't live here" {
		t.Fatalf("got %#v", err)
	}
}

func TestMalformed(t *testing.T) {
	// A list that claims to be far longer than the message must fail fast
	body := putString(nil, "123456789/1")
	body = be32(body, 1<<31)
	resp, err := parseResponse(message(opResolution, rcSuccess, body, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseResolution(resp); err == nil {
		t.Fatal("no error for an impossible value count")
	}

	if _, err := parseResponse([]byte{0, 0, 0, 1}); err == nil {
		t.Fatal("no error for a truncated header")
	}
}

// encodeKey encodes a public key the way handle servers do. Like Java's
// encoding, RSA keys get an extra, empty field at the end.
func encodeKey(key crypto.PublicKey) []byte {
	putNum := func(b []byte, n *big.Int) []byte {
		raw := n.Bytes()
		return append(be32(b, uint32(len(raw))), raw...)
	}
	var b []byte
	switch k := key.(type) {
	case *rsa.PublicKey:
		b = append(putString(nil, "RSA_PUB_KEY"), 0, 0)
		b = putNum(b, big.NewInt(int64(k.E)))
		b = putNum(b, k.N)
		b = be32(b, 0)
	case *dsa.PublicKey:
		b = append(putString(nil, "DSA_PUB_KEY"), 0, 0)
		for _, n := range []*big.Int{k.Q, k.P, k.G, k.Y} {
			b = putNum(b, n)
		}
	}
	return b
}

// encodeSiteInfo builds a GET_SITEINFO body: one server at 192.0.2.10, with the
// standard interfaces
func encodeSiteInfo(key crypto.PublicKey) []byte {
	b := []byte{0, 1, 2, 11, 0, 1, 0x80, 2} // format, protocol 2.11, serial, primary, hash option
	b = putString(b, "")                    // hash filter
	b = be32(b, 1)
	b = putString(b, "desc")
	b = putString(b, "Test site")
	b = be32(b, 1)
	b = be32(b, 1) // server ID
	b = append(b, make([]byte, 12)...)
	b = append(b, 192, 0, 2, 10)
	k := encodeKey(key)
	b = append(be32(b, uint32(len(k))), k...)
	b = be32(b, 3)
	b = append(b, 3, 1, 0, 0, 0x0a, 0x51) // query+admin, TCP 2641
	b = append(b, 2, 0, 0, 0, 0x0a, 0x51) // query, UDP 2641
	b = append(b, 3, 2, 0, 0, 0x1f, 0x40) // query+admin, HTTP 8000
	return b
}

// signedMessage builds a site info response signed with key
func signedMessage(t *testing.T, body []byte, key crypto.Signer) []byte {
	unsigned := message(opGetSiteInfo, rcSuccess, body, nil)
	digest := sha1.Sum(unsigned[:len(unsigned)-4])
	sig, err := key.Sign(rand.Reader, digest[:], crypto.SHA1)
	if err != nil {
		t.Fatal(err)
	}
	info := putString(nil, "SHA1")
	info = append(be32(info, uint32(len(sig))), sig...)
	cred := []byte{0, 0, 0, 0}
	cred = putString(cred, "") // signer handle
	cred = be32(cred, 0)       // and index
	cred = putString(cred, "HS_SIGNED")
	cred = append(be32(cred, uint32(len(info))), info...)
	return message(opGetSiteInfo, rcSuccess, body, cred)
}

// dsaSigner signs the way Java does, with DER-encoded (r, s)
type dsaSigner struct{ *dsa.PrivateKey }

func (d dsaSigner) Public() crypto.PublicKey { return &d.PublicKey }

func (d dsaSigner) Sign(rnd io.Reader, digest []byte, _ crypto.SignerOpts) ([]byte, error) {
	r, s, err := dsa.Sign(rnd, d.PrivateKey, digest)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(struct{ R, S *big.Int }{r, s})
}

func TestSiteInfoAndSignatures(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherRSA, _ := rsa.GenerateKey(rand.Reader, 2048)
	var dsaKey dsa.PrivateKey
	dsa.GenerateParameters(&dsaKey.Parameters, rand.Reader, dsa.L1024N160)
	dsa.GenerateKey(&dsaKey, rand.Reader)

	for name, key := range map[string]crypto.Signer{"RSA": rsaKey, "DSA": dsaSigner{&dsaKey}} {
		t.Run(name, func(t *testing.T) {
			resp, err := parseResponse(signedMessage(t, encodeSiteInfo(key.Public()), key))
			if err != nil {
				t.Fatal(err)
			}
			site, err := parseSiteInfo(resp.content())
			if err != nil {
				t.Fatal(err)
			}
			s := site.servers[0]
			wantInterfaces := []iface{
				{proto: "TCP", port: 2641, query: true, admin: true},
				{proto: "UDP", port: 2641, query: true},
				{proto: "HTTP", port: 8000, query: true, admin: true},
			}
			if !site.primary || !s.address.Equal(net.IPv4(192, 0, 2, 10)) || !slices.Equal(s.interfaces, wantInterfaces) {
				t.Fatalf("got primary=%t, %s, %v", site.primary, s.address, s.interfaces)
			}
			if !sameKey(s.key, key.Public()) {
				t.Fatalf("decoded key doesn't match: %v", s.keyErr)
			}

			sig, err := resp.signature()
			if err != nil {
				t.Fatal(err)
			}
			if !sig.verify(key.Public()) {
				t.Fatal("signature doesn't verify with the signing key")
			}
			if sig.verify(otherRSA.Public()) {
				t.Fatal("signature verifies with the wrong key")
			}
			sig.signed = bytes.Clone(sig.signed)
			sig.signed[len(sig.signed)-1] ^= 1
			if sig.verify(key.Public()) {
				t.Fatal("signature verifies for a tampered message")
			}
		})
	}
}

func TestKeyFromJSON(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	b64 := func(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.Bytes()) }
	js := fmt.Sprintf(`{"kty":"RSA","n":%q,"e":%q}`, b64(rsaKey.N), b64(big.NewInt(int64(rsaKey.E))))
	key, err := keyFromJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	fromBinary, err := parsePublicKey(encodeKey(&rsaKey.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if !sameKey(key, fromBinary) {
		t.Fatal("JSON and binary encodings of the same key don't match")
	}

	if _, err := keyFromJSON([]byte(`{"kty":"RSA","n":"!!","e":"AQAB"}`)); err == nil {
		t.Fatal("no error for a malformed key")
	}
}
