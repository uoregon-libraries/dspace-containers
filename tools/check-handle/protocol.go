package main

// The handle protocol's wire format (RFC 3652), or as much of it as checking a
// server takes. A message is an envelope, then a header, a body, and a
// credential, which is empty unless the client asked for a signature. Where
// CNRI's server and the RFC differ, this follows the server.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	opResolution  = 1
	opGetSiteInfo = 2

	rcSuccess        = 1
	rcHandleNotFound = 100
	rcServerNotResp  = 301

	flagCertify       = 0x40000000 // asks the server to sign its response
	flagPublicOnly    = 0x01000000 // resolution returns only public values
	flagRequestDigest = 0x00800000 // the response body starts with a digest of the request

	envelopeSize = 20

	// Requests have to fit in one UDP packet; responses can span several
	maxUDPPacket = 512

	// Real messages are nowhere near this; it just keeps a broken server from
	// making us allocate gigabytes
	maxMessage = 1 << 20

	mimeType = "application/x-hdl-message"
)

var responseCodes = map[uint32]string{
	1: "success", 2: "error", 3: "server busy", 4: "protocol error",
	5: "operation denied", 6: "recursion limit exceeded",
	100: "handle not found", 101: "handle already exists", 102: "invalid handle",
	200: "value not found", 201: "value already exists", 202: "invalid value",
	300: "expired site info", 301: "server not responsible for this handle",
	302: "service referral", 303: "prefix referral",
	400: "not authorized", 401: "access denied", 402: "authentication needed",
	403: "authentication failed", 404: "invalid credential",
	405: "authentication timed out", 406: "unable to authenticate",
	500: "session timeout", 501: "session failed", 502: "no session key",
	503: "sessions not supported", 504: "invalid session key",
	900: "trying", 901: "forwarded", 902: "queued",
}

// iface is one of a server's service interfaces: a protocol and port it
// answers on
type iface struct {
	proto string // "TCP", "UDP", "HTTP", or "HTTPS", as the registry spells them
	port  int
	query bool // answers resolution requests
	admin bool // accepts admin requests
}

func (i iface) String() string { return i.proto + " " + strconv.Itoa(i.port) }

// standardInterfaces are what every handle server listens on by default, and
// what the "handle" service publishes
var standardInterfaces = []iface{
	{proto: "TCP", port: 2641, query: true},
	{proto: "UDP", port: 2641, query: true},
	{proto: "HTTP", port: 8000, query: true},
}

// request is a message to send, minus the parts every request shares
type request struct {
	opCode uint32
	flags  uint32
	body   []byte
}

// resolutionRequest asks for all of a handle's public values
func resolutionRequest(handle string) request {
	body := putString(nil, handle)
	body = be32(body, 0) // index list: empty, meaning all
	body = be32(body, 0) // type list: empty, meaning all
	return request{opCode: opResolution, flags: flagPublicOnly, body: body}
}

// siteInfoRequest asks a server for its own copy of its site info, signed.
// The body is a handle, which the server ignores.
func siteInfoRequest() request {
	return request{opCode: opGetSiteInfo, flags: flagCertify, body: putString(nil, "")}
}

func (r request) encode() []byte {
	m := be32(nil, r.opCode)
	m = be32(m, 0) // response code
	m = be32(m, r.flags)
	m = append(m, 0xff, 0xff, 0, 0)                       // site info serial (unknown), recursion count, reserved
	m = be32(m, uint32(time.Now().Add(time.Hour).Unix())) // expiration
	m = be32(m, uint32(len(r.body)))
	m = append(m, r.body...)
	return be32(m, 0) // credential: none
}

// envelope builds a message envelope. It claims protocol 2.1, which every
// server speaks; servers answer in kind.
func envelope(requestID uint32, msgLen int) []byte {
	e := []byte{2, 1, 0, 0} // version 2.1, no flags
	e = be32(e, 0)          // session ID: none
	e = be32(e, requestID)
	e = be32(e, 0) // sequence number
	return be32(e, uint32(msgLen))
}

type envelopeInfo struct {
	requestID uint32
	seq       uint32
	msgLen    uint32
}

func parseEnvelope(b []byte) (envelopeInfo, error) {
	var e envelopeInfo
	if len(b) < envelopeSize {
		return e, errors.New("response is too short to be a handle protocol message")
	}

	// The top bits of byte 2 are flags, and the rest is the server's suggested
	// protocol version, which doesn't matter here. Of the flags, "compressed"
	// and "encrypted" would need support; "truncated" only means a UDP
	// response is split across packets.
	if b[2]&0xc0 != 0 {
		return e, errors.New("response is compressed or encrypted, which this client doesn't support")
	}

	e.requestID = binary.BigEndian.Uint32(b[8:])
	e.seq = binary.BigEndian.Uint32(b[12:])
	e.msgLen = binary.BigEndian.Uint32(b[16:])
	if e.msgLen > maxMessage {
		return e, fmt.Errorf("response claims to be %d bytes long", e.msgLen)
	}
	return e, nil
}

// response is a decoded message from a server
type response struct {
	opCode     uint32
	code       uint32
	flags      uint32
	body       []byte
	signed     []byte // the header and body, which is what a signature covers
	credential []byte // empty unless the server signed the response
}

func parseResponse(msg []byte) (*response, error) {
	r := &reader{b: msg}
	resp := &response{opCode: r.u32(), code: r.u32(), flags: r.u32()}
	r.take(8) // site info serial, recursion count, reserved, expiration
	resp.body = r.bytes()
	resp.signed = msg[:len(msg)-len(r.b)]
	// Some servers leave off the empty credential entirely
	if len(r.b) > 0 {
		resp.credential = r.bytes()
	}
	if r.err != nil {
		return nil, fmt.Errorf("malformed response: %w", r.err)
	}
	return resp, nil
}

// content returns a reader for the response body, past the request digest
// if the server included one
func (r *response) content() *reader {
	rd := &reader{b: r.body}
	if r.flags&flagRequestDigest != 0 {
		// One byte says which hash it is (MD5, SHA-1, SHA-256)
		sizes := map[byte]int{1: 16, 2: 20, 3: 32}
		alg := rd.u8()
		size, ok := sizes[alg]
		if !ok && rd.err == nil {
			rd.err = fmt.Errorf("unknown request digest type %d", alg)
		}
		rd.take(size)
	}
	return rd
}

// responseError is a response that wasn't successful
type responseError struct {
	code    uint32
	message string
}

func (e *responseError) Error() string {
	name := responseCodes[e.code]
	if name == "" {
		name = "unknown"
	}
	s := fmt.Sprintf("response code %d (%s)", e.code, name)
	if e.message != "" {
		s += ": " + e.message
	}
	return s
}

// err returns nil if the response was successful
func (r *response) err() error {
	if r.code == rcSuccess {
		return nil
	}
	// Error responses usually explain themselves in the body
	return &responseError{code: r.code, message: strings.TrimSpace(r.content().str())}
}

// value is one of a handle's values
type value struct {
	index int
	typ   string
	data  []byte
}

// parseResolution decodes a successful resolution response
func parseResolution(resp *response) ([]value, error) {
	r := resp.content()
	r.bytes() // the handle
	var values []value
	for range r.count() {
		v := value{index: int(r.u32())}
		r.take(10) // timestamp (4), TTL type (1), TTL (4), permissions (1)
		v.typ = r.str()
		v.data = r.bytes()
		for range r.count() {
			r.bytes() // referenced handle
			r.u32()   // and its index
		}
		values = append(values, v)
	}
	if r.err != nil {
		return nil, fmt.Errorf("malformed resolution response: %w", r.err)
	}
	return values, nil
}

var errUnsupported = errors.New("isn't supported by this client")

// client sends requests to one handle server
type client struct {
	host    string
	timeout time.Duration
}

func (c client) addr(port int) string { return net.JoinHostPort(c.host, strconv.Itoa(port)) }

// send sends req to the server over i, and returns the response, successful
// or not. Errors are for when there's no usable response at all.
func (c client) send(i iface, req request) (*response, error) {
	id := rand.Uint32()
	msg := req.encode()
	var raw []byte
	var err error
	switch i.proto {
	case "TCP":
		raw, err = c.sendTCP(i.port, id, msg)
	case "UDP":
		raw, err = c.sendUDP(i.port, id, msg)
	case "HTTP":
		raw, err = c.sendHTTP(i.port, id, msg)
	default:
		return nil, fmt.Errorf("%s %w", i.proto, errUnsupported)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil, fmt.Errorf("no answer within %s", c.timeout)
	}
	if err != nil {
		return nil, err
	}
	return parseResponse(raw)
}

func (c client) sendTCP(port int, id uint32, msg []byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", c.addr(port), c.timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.timeout))

	if _, err := conn.Write(append(envelope(id, len(msg)), msg...)); err != nil {
		return nil, err
	}
	var env [envelopeSize]byte
	if _, err := io.ReadFull(conn, env[:]); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	e, err := parseEnvelope(env[:])
	if err != nil {
		return nil, err
	}
	if e.requestID != id {
		return nil, fmt.Errorf("response is for request %d, not %d", e.requestID, id)
	}
	out := make([]byte, e.msgLen)
	if _, err := io.ReadFull(conn, out); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return out, nil
}

func (c client) sendUDP(port int, id uint32, msg []byte) ([]byte, error) {
	pkt := append(envelope(id, len(msg)), msg...)
	if len(pkt) > maxUDPPacket {
		return nil, errors.New("request is too big for UDP")
	}
	conn, err := net.Dial("udp", c.addr(port))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := conn.Write(pkt); err != nil {
		return nil, err
	}

	// A response too big for one packet comes in several, each with its own
	// envelope giving the whole message's length and the packet's place in
	// it. They can arrive in any order, or twice.
	parts := make(map[uint32][]byte)
	got := 0
	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		e, err := parseEnvelope(buf[:n])
		if err != nil {
			return nil, err
		}
		if e.requestID != id || parts[e.seq] != nil {
			continue
		}
		parts[e.seq] = bytes.Clone(buf[envelopeSize:n])
		got += n - envelopeSize
		if got < int(e.msgLen) {
			continue
		}

		var out []byte
		for seq := range uint32(len(parts)) {
			if parts[seq] == nil {
				return nil, errors.New("UDP response has a gap in its packets")
			}
			out = append(out, parts[seq]...)
		}
		return out[:e.msgLen], nil
	}
}

func (c client) sendHTTP(port int, id uint32, msg []byte) ([]byte, error) {
	u := url.URL{Scheme: "http", Host: c.addr(port), Path: "/"}
	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(append(envelope(id, len(msg)), msg...)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mimeType)
	// Without this, the server answers with its HTML proxy page
	req.Header.Set("Accept", mimeType)

	resp, err := (&http.Client{Timeout: c.timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %s", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, mimeType) {
		return nil, fmt.Errorf("got %q instead of a handle protocol message", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, envelopeSize+maxMessage))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	e, err := parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	if e.requestID != id {
		return nil, fmt.Errorf("response is for request %d, not %d", e.requestID, id)
	}
	if len(body)-envelopeSize < int(e.msgLen) {
		return nil, errors.New("response ends early")
	}
	return body[envelopeSize : envelopeSize+e.msgLen], nil
}

// reader decodes the protocol's big-endian fields. The first error sticks,
// and reads after it return zero values, so a whole structure can be decoded
// before checking err.
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.err = errors.New("message ends early")
		return nil
	}
	v := r.b[:n:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) u8() byte {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if b := r.take(2); b != nil {
		return binary.BigEndian.Uint16(b)
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

// bytes reads a length-prefixed byte string
func (r *reader) bytes() []byte { return r.take(int(r.u32())) }

func (r *reader) str() string { return string(r.bytes()) }

// count reads a list's length. Every list item takes at least a byte, so a
// count bigger than what's left is an error rather than a long, empty loop.
func (r *reader) count() int {
	n := int(r.u32())
	if n > len(r.b) {
		r.take(n)
		return 0
	}
	return n
}

func be32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }

func putString(b []byte, s string) []byte { return append(be32(b, uint32(len(s))), s...) }
