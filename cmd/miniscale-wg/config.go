package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// config is a wg-quick file, cut down to what a userspace tunnel uses.
type config struct {
	privateKey []byte
	addresses  []netip.Prefix
	dns        []netip.Addr
	mtu        int
	listenPort int
	peers      []peer
}

type peer struct {
	publicKey    []byte
	presharedKey []byte // nil when not set
	endpoint     string // HOST:PORT, "" when not set
	allowedIPs   []netip.Prefix
	keepalive    int
}

// parseFile reads the wg-quick file at path. Errors read "path:line: what",
// on one line, for the app to show.
func parseFile(path string) (*config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f, path)
}

func parse(r io.Reader, name string) (*config, error) {
	c := &config{mtu: 1420}
	var p *peer // the [Peer] being read; nil in [Interface]
	section := ""
	sc := bufio.NewScanner(r)
	n := 0
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%s:%d: %s", name, n, fmt.Sprintf(format, a...))
	}
	for sc.Scan() {
		n++
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch section {
			case "interface":
				p = nil
			case "peer":
				c.peers = append(c.peers, peer{})
				p = &c.peers[len(c.peers)-1]
			default:
				return nil, bad("unknown section %s", line)
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, bad("want KEY = VALUE")
		}
		kname := strings.TrimSpace(key) // as written, for messages
		key = strings.ToLower(kname)
		val = strings.TrimSpace(val)
		if section == "" {
			return nil, bad("%s outside [Interface] or [Peer]", kname)
		}
		var err error
		if p == nil {
			switch key {
			case "privatekey":
				c.privateKey, err = parseKey(val)
			case "address":
				for _, s := range list(val) {
					a, err := parsePrefix(s)
					if err != nil {
						return nil, bad("bad Address %q", s)
					}
					c.addresses = append(c.addresses, a)
				}
			case "dns":
				for _, s := range list(val) {
					// wg-quick takes non-IP entries as search domains,
					// which only matter to a system resolver: skip them.
					if a, err := netip.ParseAddr(s); err == nil {
						c.dns = append(c.dns, a.Unmap())
					}
				}
			case "mtu":
				c.mtu, err = strconv.Atoi(val)
				if err != nil || c.mtu < 576 || c.mtu > 65535 {
					return nil, bad("bad MTU %q", val)
				}
			case "listenport":
				c.listenPort, err = strconv.Atoi(val)
				if err != nil || c.listenPort < 0 || c.listenPort > 65535 {
					return nil, bad("bad ListenPort %q", val)
				}
			}
			// anything else is wg-quick's (PostUp, Table, SaveConfig...)
		} else {
			switch key {
			case "publickey":
				p.publicKey, err = parseKey(val)
			case "presharedkey":
				p.presharedKey, err = parseKey(val)
			case "endpoint":
				h, port, e := net.SplitHostPort(val)
				if pn, perr := strconv.Atoi(port); e != nil || h == "" || perr != nil || pn < 1 || pn > 65535 {
					return nil, bad("bad Endpoint %q, want HOST:PORT", val)
				}
				p.endpoint = val
			case "allowedips":
				for _, s := range list(val) {
					a, err := parsePrefix(s)
					if err != nil {
						return nil, bad("bad AllowedIPs %q", s)
					}
					p.allowedIPs = append(p.allowedIPs, a)
				}
			case "persistentkeepalive":
				if val == "off" {
					break
				}
				p.keepalive, err = strconv.Atoi(val)
				if err != nil || p.keepalive < 0 || p.keepalive > 65535 {
					return nil, bad("bad PersistentKeepalive %q", val)
				}
			}
		}
		if err != nil {
			// the message never carries the value: it may be a secret key
			return nil, bad("bad %s: %v", kname, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	switch {
	case c.privateKey == nil:
		return nil, fmt.Errorf("%s: no PrivateKey in [Interface]", name)
	case len(c.addresses) == 0:
		return nil, fmt.Errorf("%s: no Address in [Interface]", name)
	case len(c.peers) == 0:
		return nil, fmt.Errorf("%s: no [Peer]", name)
	}
	for i, p := range c.peers {
		if p.publicKey == nil {
			return nil, fmt.Errorf("%s: [Peer] %d has no PublicKey", name, i+1)
		}
	}
	return c, nil
}

// parseKey decodes a base64 WireGuard key.
func parseKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("not a base64 key of 32 bytes")
	}
	return k, nil
}

// parsePrefix takes a CIDR, or a bare IP as a single address.
func parsePrefix(s string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return p, err
	}
	return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()).Masked(), nil
}

// list splits a comma-separated value.
func list(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// ipc renders the config in WireGuard's configuration protocol. Peers whose
// endpoint is a name get it later, from resolveEndpoints.
func (c *config) ipc() string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(c.privateKey))
	if c.listenPort != 0 {
		fmt.Fprintf(&b, "listen_port=%d\n", c.listenPort)
	}
	for _, p := range c.peers {
		fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(p.publicKey))
		if p.presharedKey != nil {
			fmt.Fprintf(&b, "preshared_key=%s\n", hex.EncodeToString(p.presharedKey))
		}
		for _, a := range p.allowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", a)
		}
		ap, err := netip.ParseAddrPort(p.endpoint)
		if err == nil {
			fmt.Fprintf(&b, "endpoint=%s\n", ap)
		}
		// Keepalive waits for a named endpoint: set before the peer has
		// one, its first keepalive would go nowhere and none would follow.
		if p.keepalive > 0 && (err == nil || p.endpoint == "") {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.keepalive)
		}
	}
	return b.String()
}
