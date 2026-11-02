// check-handle checks that a handle server is set up and working, by talking
// to it the way the rest of the handle system does: with the handle protocol,
// over TCP and UDP on port 2641 and HTTP on port 8000. (curl can query the
// server's REST API, but nothing that resolves handles uses that.)
//
// Given a handle that exists, it:
//
//   - looks up the handle's prefix in the global registry, via hdl.handle.net,
//     for the servers registered to answer for it
//   - resolves the handle on each interface of each server it tests
//   - gets each server's own site info and compares it with the registry's
//   - has each server sign its site info, which shows which private key it
//     really has. Site info only says which public key it's supposed to
//     have, and the two can disagree.
//   - resolves the handle through hdl.handle.net, which is what users get
//
// Usage:
//
//     check-handle [-server <host>] [-timeout <duration>] <prefix>/<suffix>
//
// Without -server, it tests the servers registered for the prefix: the check
// for after handle.net has registered a server. With -server, it tests that
// server on TCP 2641, UDP 2641, and HTTP 8000 instead: the check for before
// then. Until a server is registered, its differences from the registry are
// warnings rather than failures.
//
// Failures are marked FAIL, and make the exit status 1. Warnings are marked
// WARN.

package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

func main() {
	server := flag.String("server", "", "test this server (host name or IP address) on TCP 2641, UDP 2641, and HTTP 8000,\ninstead of the servers registered for the handle's prefix")
	timeout := flag.Duration("timeout", 5*time.Second, "how long to wait for each response")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprint(out, "usage: check-handle [-server <host>] [-timeout <duration>] <prefix>/<suffix>\n\n")
		fmt.Fprint(out, "The handle must exist.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	handle := flag.Arg(0)
	prefix, suffix, _ := strings.Cut(handle, "/")
	if prefix == "" || suffix == "" {
		fmt.Fprintf(os.Stderr, "check-handle: %q isn't a handle: it should look like <prefix>/<suffix>\n", handle)
		os.Exit(2)
	}

	c := &checker{handle: handle, timeout: *timeout, explained: make(map[string]bool)}
	c.checkRegistry(prefix)

	var targets []*target
	if *server != "" {
		targets = append(targets, &target{host: *server, interfaces: standardInterfaces})
	} else {
		for _, site := range c.sites {
			for _, s := range site.servers {
				var query []iface
				for _, i := range s.interfaces {
					if i.query {
						query = append(query, i)
					}
				}
				targets = append(targets, &target{host: s.address.String(), interfaces: query, registered: s})
			}
		}
	}
	if len(targets) == 0 {
		c.section("Servers")
		c.fail("no registered servers to test (use -server to test one directly)")
	}

	// The global check compares with what a registered server says, if one
	// was tested
	var direct []string
	registered := false
	for _, t := range targets {
		urls := c.checkServer(t)
		if t.registered != nil && direct == nil {
			direct = urls
		}
		registered = registered || t.registered != nil
	}
	c.checkGlobal(direct, registered)

	os.Exit(c.summarize())
}

// checker runs the checks, and reports on them as it goes
type checker struct {
	handle   string
	timeout  time.Duration
	sites    []*siteInfo // what the registry has for the prefix
	sections int
	fails    int
	warns    int

	explained map[string]bool // hints already given
}

// target is a server to test
type target struct {
	host       string
	interfaces []iface
	registered *serverInfo // its entry in the registry, if it has one
}

func (c *checker) checkRegistry(prefix string) {
	c.section("Registry: 0.NA/%s, via hdl.handle.net", prefix)
	sites, err := registeredSites(prefix, c.timeout)
	switch {
	case err != nil:
		c.fail("%s", err)
	case sites == nil:
		c.warn("prefix %s isn't registered, so there's nothing to compare servers with", prefix)
	}

	c.sites = sites
	for _, site := range sites {
		for _, s := range site.servers {
			mirror := ""
			if !site.primary {
				mirror = " (mirror)"
			}
			c.info("server %d at %s%s: %s; %s", s.id, s.address, mirror, describeInterfaces(s.interfaces), keyOrErr(s))
		}
	}
}

// checkServer checks one server, and returns the handle's URLs as the server
// gave them
func (c *checker) checkServer(t *target) []string {
	c.section("Server %s", t.host)
	cl := client{host: t.host, timeout: c.timeout}

	// Resolve on every interface before reporting: whether a dead interface
	// matters depends on the site info, which has to come over one that works
	type result struct {
		answered bool
		urls     []string
		err      error
	}
	results := make([]result, len(t.interfaces))
	var working *iface
	for n, i := range t.interfaces {
		resp, err := cl.send(i, resolutionRequest(c.handle))
		if err != nil {
			results[n].err = err
			continue
		}
		if working == nil {
			working = &t.interfaces[n]
		}
		results[n].answered = true
		results[n].urls, results[n].err = urlsOf(resp)
	}

	var self *serverInfo
	var signed *response
	var siteErr error
	if working != nil {
		self, signed, siteErr = c.selfInfo(cl, *working)
		if t.registered == nil && self != nil {
			t.registered = c.findRegistered(t.host, self)
		}
	}

	// Resolvers try whatever the registry lists. Until the server is
	// registered, what it lists itself is what will be.
	listed := func(i iface) bool {
		switch {
		case t.registered != nil:
			return advertises(t.registered, i)
		case self != nil:
			return advertises(self, i)
		}
		return true
	}
	var urls []string
	if len(t.interfaces) == 0 {
		c.fail("the registry lists no interfaces that answer queries")
	}
	for n, i := range t.interfaces {
		r := results[n]
		switch {
		case errors.Is(r.err, errUnsupported):
			c.info("%s: not checked: %s", i, r.err)
		case !r.answered && !listed(i):
			c.info("%s: no answer, but it isn't listed, so nothing will try it", i)
		case r.err != nil:
			c.fail("%s: %s", i, explain(r.err))
		default:
			c.ok("%s: %s -> %s", i, c.handle, strings.Join(r.urls, ", "))
			if urls == nil {
				urls = r.urls
			}
		}
	}
	if working == nil {
		return urls
	}

	if siteErr != nil {
		c.fail("site info: %s", siteErr)
		return urls
	}
	c.ok("site info: server %d at %s: %s; %s", self.id, self.address, describeInterfaces(self.interfaces), keyOrErr(self))
	c.compareWithRegistry(t, self)
	c.checkSignature(signed, t, self)
	return urls
}

// selfInfo gets the server's own site info, signed, and picks out its entry
func (c *checker) selfInfo(cl client, i iface) (*serverInfo, *response, error) {
	resp, err := cl.send(i, siteInfoRequest())
	if err == nil {
		err = resp.err()
	}
	if err != nil {
		return nil, nil, err
	}
	site, err := parseSiteInfo(resp.content())
	if err != nil {
		return nil, nil, err
	}

	switch len(site.servers) {
	case 0:
		return nil, nil, errors.New("it lists no servers")
	case 1:
		return site.servers[0], resp, nil
	}
	ips := hostIPs(cl.host)
	for _, s := range site.servers {
		if slices.ContainsFunc(ips, s.address.Equal) {
			return s, resp, nil
		}
	}
	return nil, nil, fmt.Errorf("it lists %d servers, none at %s", len(site.servers), cl.host)
}

// findRegistered finds the registry's entry for a server, by the address we
// reached it at, or the one it gives for itself
func (c *checker) findRegistered(host string, self *serverInfo) *serverInfo {
	ips := append(hostIPs(host), self.address)
	for _, site := range c.sites {
		for _, s := range site.servers {
			if slices.ContainsFunc(ips, s.address.Equal) {
				return s
			}
		}
	}
	return nil
}

// compareWithRegistry reports where a server's site info and the registry
// disagree. Keys are left to checkSignature, which can tell which key is
// right.
func (c *checker) compareWithRegistry(t *target, self *serverInfo) {
	switch {
	case c.sites == nil:
		return
	case t.registered == nil:
		c.warn("the registry doesn't list this server (%s)\nexpected until handle.net installs its sitebndl.zip", self.address)
		return
	}

	reg := t.registered
	if !self.address.Equal(reg.address) {
		c.warn("the server's site info gives its address as %s, but the registry has %s", self.address, reg.address)
	}
	if !sameInterfaces(self.interfaces, reg.interfaces) {
		c.warn("the server's site info lists %s, but the registry has %s", describeInterfaces(self.interfaces), describeInterfaces(reg.interfaces))
	}
}

// checkSignature works out whose key signed the server's site info
func (c *checker) checkSignature(resp *response, t *target, self *serverInfo) {
	sig, err := resp.signature()
	if err != nil {
		c.fail("signature: %s", err)
		return
	}

	selfKey := self.key
	bySelf := selfKey != nil && sig.verify(selfKey)

	// A registered server should sign with its own registered key. An
	// unregistered one might have another server's keys, copied over to
	// replace it.
	candidates := []*serverInfo{t.registered}
	if t.registered == nil {
		candidates = nil
		for _, site := range c.sites {
			candidates = append(candidates, site.servers...)
		}
	}
	var signer *serverInfo
	for _, s := range candidates {
		if s.key != nil && sig.verify(s.key) {
			signer = s
			break
		}
	}

	switch {
	case signer != nil:
		c.ok("signature: made with the key registered for %s (%s)", signer.address, describeKey(signer.key))
		if selfKey != nil && !sameKey(selfKey, signer.key) {
			c.warn("but the server's own site info has %s, so its key files don't all match", describeKey(selfKey))
		}
	case bySelf && t.registered != nil:
		c.fail("signature: made with the key in the server's site info (%s), not the registered %s\n"+
			"handles still resolve, since resolvers rarely ask for signatures, but anything that\n"+
			"checks them will reject this server: handle.net needs its sitebndl.zip, or it needs\n"+
			"the registered keys",
			describeKey(selfKey), keyOrErr(t.registered))
	case bySelf && c.sites != nil:
		c.warn("signature: made with a key that isn't registered (%s)\n"+
			"fine if handle.net will install this server's sitebndl.zip, but if you meant to\n"+
			"reuse another server's keys, they didn't make it here", describeKey(selfKey))
	case bySelf:
		c.ok("signature: made with the key in the server's site info (%s)", describeKey(selfKey))
	case c.sites != nil:
		c.fail("signature: made with neither the registered key nor the one in the server's site info\n" +
			"its private key (privkey.bin) doesn't go with either")
	default:
		c.fail("signature: not made with the key in the server's site info\n" +
			"its private key (privkey.bin) doesn't go with its public key")
	}
}

// checkGlobal resolves the handle through hdl.handle.net, the way users do.
// What it gets comes from the registered server, so unless that's one of the
// servers tested, failures here aren't theirs.
func (c *checker) checkGlobal(direct []string, registered bool) {
	c.section("Global resolution: hdl.handle.net")
	if c.sites == nil {
		c.info("skipped: the registry has nothing for this prefix")
		return
	}

	urls, err := globalURLs(c.handle, c.timeout)
	if err == nil && len(urls) == 0 {
		err = errors.New("the handle has no URL value")
	}
	switch {
	case err != nil && registered:
		c.fail("%s", explain(err))
	case err != nil:
		c.warn("%s\nthis is the registered server's answer, not the tested one's", explain(err))
	default:
		c.ok("%s -> %s", c.handle, strings.Join(urls, ", "))
		if direct != nil && !slices.Equal(urls, direct) {
			c.warn("that's not what the registered server gave (%s)", strings.Join(direct, ", "))
		}
	}
}

// summarize prints the totals and returns the exit status
func (c *checker) summarize() int {
	fmt.Println()
	switch {
	case c.fails > 0:
		fmt.Printf("%s, %s\n", plural(c.fails, "failure"), plural(c.warns, "warning"))
		return 1
	case c.warns > 0:
		fmt.Printf("No failures, %s\n", plural(c.warns, "warning"))
	default:
		fmt.Println("All checks passed")
	}
	return 0
}

func (c *checker) section(format string, args ...any) {
	if c.sections > 0 {
		fmt.Println()
	}
	c.sections++
	fmt.Printf(format+"\n", args...)
}

// line prints a result. Lines after the first are a hint or explanation,
// which is indented to match, and left out if it's been given already.
func (c *checker) line(tag, format string, args ...any) {
	msg, more, _ := strings.Cut(fmt.Sprintf(format, args...), "\n")
	fmt.Printf("  %-4s  %s\n", tag, msg)
	if more != "" && !c.explained[more] {
		c.explained[more] = true
		fmt.Printf("        %s\n", strings.ReplaceAll(more, "\n", "\n        "))
	}
}

func (c *checker) ok(format string, args ...any)   { c.line("ok", format, args...) }
func (c *checker) info(format string, args ...any) { c.line("", format, args...) }

func (c *checker) warn(format string, args ...any) {
	c.warns++
	c.line("WARN", format, args...)
}

func (c *checker) fail(format string, args ...any) {
	c.fails++
	c.line("FAIL", format, args...)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// urlsOf returns the URL values in a resolution response
func urlsOf(resp *response) ([]string, error) {
	if err := resp.err(); err != nil {
		return nil, err
	}
	values, err := parseResolution(resp)
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, v := range values {
		if v.typ == "URL" {
			urls = append(urls, string(v.data))
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("the handle has no URL value")
	}
	return urls, nil
}

// explain adds a hint to errors with a likely cause
func explain(err error) string {
	var re *responseError
	if errors.As(err, &re) {
		switch re.code {
		case rcHandleNotFound:
			return err.Error() + "\nif the handle exists, the server isn't reading the database it's in"
		case rcServerNotResp:
			return err.Error() + "\nthe server doesn't think it serves this prefix: check HANDLE_PREFIX in .env"
		}
	}
	return err.Error()
}

func keyOrErr(s *serverInfo) string {
	if s.key == nil {
		return "unusable key: " + s.keyErr.Error()
	}
	return describeKey(s.key)
}

func hostIPs(host string) []net.IP {
	ips, _ := net.LookupIP(host)
	return ips
}
