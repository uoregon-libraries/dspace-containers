package main

// Site info and keys. A site is the set of servers that answer for a prefix.
// Its description comes from two places that ought to agree: the registry's
// HS_SITE values, which hdl.handle.net gives us as JSON, and each server's own
// copy, made by handle-setup.sh, which it gives us in the protocol's binary
// encoding. The key in either one is only a claim, though: the key a server
// actually has is whichever one its signatures verify with.

import (
	"crypto"
	"crypto/dsa" // deprecated, but older handle servers still have DSA keys
	"crypto/rsa"
	_ "crypto/sha1"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strings"
)

type siteInfo struct {
	primary bool
	servers []*serverInfo
}

type serverInfo struct {
	id         int
	address    net.IP
	key        crypto.PublicKey // nil if keyErr says why not
	keyErr     error
	interfaces []iface
}

var protocolNames = map[byte]string{0: "UDP", 1: "TCP", 2: "HTTP", 3: "HTTPS"}

// parseSiteInfo decodes the binary site info in a GET_SITEINFO response
func parseSiteInfo(r *reader) (*siteInfo, error) {
	r.u16() // data format version
	r.u16() // protocol version
	r.u16() // serial number
	site := &siteInfo{primary: r.u8()&0x80 != 0}
	r.u8()    // hash option
	r.bytes() // hash filter
	for range r.count() {
		r.bytes() // attribute name
		r.bytes() // and value
	}

	for range r.count() {
		s := &serverInfo{id: int(r.u32()), address: parseAddress(r.take(16))}
		s.key, s.keyErr = parsePublicKey(r.bytes())
		for range r.count() {
			service, proto, port := r.u8(), r.u8(), r.u32()
			name := protocolNames[proto]
			if name == "" {
				name = fmt.Sprintf("protocol-%d", proto)
			}
			s.interfaces = append(s.interfaces, iface{proto: name, port: int(port), query: service&2 != 0, admin: service&1 != 0})
		}
		site.servers = append(site.servers, s)
	}
	if r.err != nil {
		return nil, fmt.Errorf("malformed site info: %w", r.err)
	}
	return site, nil
}

// parseAddress decodes a 16-byte address. IPv4 addresses are zero-padded
// rather than IPv4-mapped.
func parseAddress(b []byte) net.IP {
	if len(b) != 16 {
		return nil
	}
	for _, c := range b[:12] {
		if c != 0 {
			return net.IP(b)
		}
	}
	return net.IPv4(b[12], b[13], b[14], b[15])
}

// parsePublicKey decodes a key in the protocol's encoding: a type name, two
// bytes of flags, and then the key's numbers
func parsePublicKey(b []byte) (crypto.PublicKey, error) {
	r := &reader{b: b}
	kind := r.str()
	r.u16() // flags
	num := func() *big.Int { return new(big.Int).SetBytes(r.bytes()) }

	var key crypto.PublicKey
	switch kind {
	case "RSA_PUB_KEY":
		e, n := num(), num()
		if !e.IsInt64() || e.Int64() > 1<<31-1 {
			return nil, errors.New("RSA key has an absurd exponent")
		}
		key = &rsa.PublicKey{N: n, E: int(e.Int64())}
	case "DSA_PUB_KEY":
		q, p, g, y := num(), num(), num(), num()
		key = &dsa.PublicKey{Parameters: dsa.Parameters{P: p, Q: q, G: g}, Y: y}
	default:
		return nil, fmt.Errorf("unsupported key type %q", kind)
	}
	if r.err != nil {
		return nil, fmt.Errorf("malformed %s: %w", kind, r.err)
	}
	return key, nil
}

// jsonSite is an HS_SITE value as hdl.handle.net's REST API gives it
type jsonSite struct {
	PrimarySite bool `json:"primarySite"`
	Servers     []struct {
		ServerID  int    `json:"serverId"`
		Address   string `json:"address"`
		PublicKey struct {
			Value json.RawMessage `json:"value"`
		} `json:"publicKey"`
		Interfaces []struct {
			Query    bool   `json:"query"`
			Admin    bool   `json:"admin"`
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
		} `json:"interfaces"`
	} `json:"servers"`
}

func siteFromJSON(raw json.RawMessage) (*siteInfo, error) {
	var js jsonSite
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, fmt.Errorf("malformed HS_SITE value: %w", err)
	}
	site := &siteInfo{primary: js.PrimarySite}
	for _, jsrv := range js.Servers {
		s := &serverInfo{id: jsrv.ServerID, address: net.ParseIP(jsrv.Address)}
		if s.address == nil {
			return nil, fmt.Errorf("HS_SITE value has a bad server address, %q", jsrv.Address)
		}
		s.key, s.keyErr = keyFromJSON(jsrv.PublicKey.Value)
		for _, i := range jsrv.Interfaces {
			s.interfaces = append(s.interfaces, iface{proto: i.Protocol, port: i.Port, query: i.Query, admin: i.Admin})
		}
		site.servers = append(site.servers, s)
	}
	return site, nil
}

// keyFromJSON decodes a key in the REST API's format, which is like a JWK
func keyFromJSON(raw json.RawMessage) (crypto.PublicKey, error) {
	var k struct {
		Kty  string `json:"kty"`
		N, E string // RSA
		P, Q string // DSA
		G, Y string
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("malformed key: %w", err)
	}
	var err error
	num := func(s string) *big.Int {
		b, e := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
		if e != nil || len(b) == 0 {
			err = errors.New("malformed key")
		}
		return new(big.Int).SetBytes(b)
	}

	var key crypto.PublicKey
	switch k.Kty {
	case "RSA":
		e := num(k.E)
		if !e.IsInt64() || e.Int64() > 1<<31-1 {
			return nil, errors.New("RSA key has an absurd exponent")
		}
		key = &rsa.PublicKey{N: num(k.N), E: int(e.Int64())}
	case "DSA":
		key = &dsa.PublicKey{Parameters: dsa.Parameters{P: num(k.P), Q: num(k.Q), G: num(k.G)}, Y: num(k.Y)}
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
	return key, err
}

func sameKey(a, b crypto.PublicKey) bool {
	switch a := a.(type) {
	case *rsa.PublicKey:
		b, ok := b.(*rsa.PublicKey)
		return ok && a.Equal(b)
	case *dsa.PublicKey:
		b, ok := b.(*dsa.PublicKey)
		return ok && a.Y.Cmp(b.Y) == 0 && a.P.Cmp(b.P) == 0 && a.Q.Cmp(b.Q) == 0 && a.G.Cmp(b.G) == 0
	}
	return false
}

// describeKey gives a key's type and size, and a short fingerprint so keys
// can be told apart in the output
func describeKey(key crypto.PublicKey) string {
	h := sha256.New()
	var desc string
	switch k := key.(type) {
	case *rsa.PublicKey:
		h.Write(k.N.Bytes())
		h.Write(big.NewInt(int64(k.E)).Bytes())
		desc = fmt.Sprintf("RSA-%d", k.N.BitLen())
	case *dsa.PublicKey:
		for _, n := range []*big.Int{k.P, k.Q, k.G, k.Y} {
			h.Write(n.Bytes())
		}
		desc = fmt.Sprintf("DSA-%d", k.P.BitLen())
	default:
		return "unknown key type"
	}
	return fmt.Sprintf("%s key %x", desc, h.Sum(nil)[:4])
}

// describeInterfaces lists a server's interfaces, noting the ones that don't
// answer queries
func describeInterfaces(list []iface) string {
	var parts []string
	for _, i := range list {
		s := i.String()
		if !i.query {
			s += " (admin only)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func sameInterfaces(a, b []iface) bool {
	sorted := func(list []iface) []string {
		var s []string
		for _, i := range list {
			s = append(s, fmt.Sprintf("%s query=%t admin=%t", i, i.query, i.admin))
		}
		slices.Sort(s)
		return s
	}
	return slices.Equal(sorted(a), sorted(b))
}

// advertises reports whether s claims to answer queries on i's protocol and
// port
func advertises(s *serverInfo, i iface) bool {
	return slices.ContainsFunc(s.interfaces, func(j iface) bool {
		return j.query && j.proto == i.proto && j.port == i.port
	})
}

// signature is a signed response's signature, ready to check against keys
type signature struct {
	hash   crypto.Hash
	sig    []byte
	signed []byte
}

// signature extracts the signature from a response to a certified request.
// The credential is a version, reserved bytes, options, the signer (a handle
// and index, both empty for a server's own signature), a type, and then the
// signed info: a hash name and the signature itself. The signature covers the
// message header and body.
func (r *response) signature() (*signature, error) {
	if len(r.credential) == 0 {
		return nil, errors.New("the response isn't signed")
	}
	rd := &reader{b: r.credential}
	rd.take(4) // version, reserved, options
	rd.bytes() // signer handle
	rd.u32()   // and index
	typ := rd.str()
	info := &reader{b: rd.bytes()}
	hashName := info.str()
	sig := &signature{sig: info.bytes(), signed: r.signed}
	if err := errors.Join(rd.err, info.err); err != nil {
		return nil, fmt.Errorf("malformed signature: %w", err)
	}
	if typ != "HS_SIGNED" {
		return nil, fmt.Errorf("unsupported credential type %q", typ)
	}

	switch strings.ToUpper(strings.ReplaceAll(hashName, "-", "")) {
	case "SHA1":
		sig.hash = crypto.SHA1
	case "SHA256":
		sig.hash = crypto.SHA256
	default:
		return nil, fmt.Errorf("unsupported signature hash %q", hashName)
	}
	return sig, nil
}

// verify reports whether key made the signature
func (s *signature) verify(key crypto.PublicKey) bool {
	h := s.hash.New()
	h.Write(s.signed)
	digest := h.Sum(nil)

	switch k := key.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(k, s.hash, digest, s.sig) == nil
	case *dsa.PublicKey:
		// Java's DSA signatures are DER-encoded
		var rs struct{ R, S *big.Int }
		if rest, err := asn1.Unmarshal(s.sig, &rs); err != nil || len(rest) > 0 {
			return false
		}
		return dsa.Verify(k, digest, rs.R, rs.S)
	}
	return false
}
