package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/tun/netstack"
)

// router dials for the proxies: into the tunnel for addresses a peer's
// AllowedIPs cover, straight out otherwise, so a split tunnel leaves the
// Wi-Fi network reachable.
type router struct {
	tnet    *netstack.Net // nil: everything goes direct (tests)
	dns     bool          // the config has DNS servers, for tnet's resolver
	allowed []netip.Prefix
	direct  net.Dialer
}

func (r *router) viaTunnel(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range r.allowed {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// lookup resolves host with the tunnel's DNS when there is one, and the
// system's when not or when it fails (a name only the LAN knows).
func (r *router) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	if r.tnet != nil && r.dns {
		if names, err := r.tnet.LookupContextHost(ctx, host); err == nil {
			var out []netip.Addr
			for _, s := range names {
				if ip, err := netip.ParseAddr(s); err == nil {
					out = append(out, ip)
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dial connects to a TCP HOST:PORT, trying each address of the host in
// turn.
func (r *router) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(ps, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("bad port in %q", address)
	}
	ips, err := r.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		ap := netip.AddrPortFrom(ip.Unmap(), uint16(port))
		var c net.Conn
		if r.tnet != nil && r.viaTunnel(ip) {
			c, err = r.tnet.DialContextTCPAddrPort(ctx, ap)
		} else {
			c, err = r.direct.DialContext(ctx, "tcp", ap.String())
		}
		if err == nil {
			return c, nil
		}
	}
	return nil, err
}

// dialTimeout bounds a proxy's dial, name lookup included.
const dialTimeout = 20 * time.Second

// serveSOCKS runs a SOCKS5 proxy on ln: no authentication, CONNECT only
// (RFC 1928).
func serveSOCKS(ln net.Listener, r *router) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			log.Printf("socks accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go socksConn(c, r)
	}
}

func socksConn(c net.Conn, r *router) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(dialTimeout)) // for the handshake
	br := bufio.NewReader(c)
	b := make([]byte, 256)
	if _, err := io.ReadFull(br, b[:2]); err != nil || b[0] != 5 {
		return
	}
	methods := b[1]
	if _, err := io.ReadFull(br, b[:methods]); err != nil {
		return
	}
	if !strings.Contains(string(b[:methods]), "\x00") {
		c.Write([]byte{5, 0xff}) // no acceptable method
		return
	}
	c.Write([]byte{5, 0})

	reply := func(code byte) { c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}) }
	if _, err := io.ReadFull(br, b[:4]); err != nil || b[0] != 5 {
		return
	}
	if b[1] != 1 {
		reply(7) // command not supported
		return
	}
	var host string
	switch b[3] {
	case 1, 4: // IPv4, IPv6
		n := 4
		if b[3] == 4 {
			n = 16
		}
		if _, err := io.ReadFull(br, b[:n]); err != nil {
			return
		}
		ip, _ := netip.AddrFromSlice(b[:n])
		host = ip.String()
	case 3: // domain name
		if _, err := io.ReadFull(br, b[:1]); err != nil {
			return
		}
		n := int(b[0])
		if _, err := io.ReadFull(br, b[:n]); err != nil {
			return
		}
		host = string(b[:n])
	default:
		reply(8) // address type not supported
		return
	}
	if _, err := io.ReadFull(br, b[:2]); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(b[:2]))))

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	rc, err := r.dial(ctx, "tcp", target)
	cancel()
	if err != nil {
		log.Printf("socks %s: %v", target, err)
		reply(socksCode(err))
		return
	}
	defer rc.Close()
	reply(0)
	c.SetDeadline(time.Time{})
	// a client may send its first bytes with the request
	if n := br.Buffered(); n > 0 {
		p, _ := br.Peek(n)
		if _, err := rc.Write(p); err != nil {
			return
		}
	}
	pipe(c, rc)
}

// socksCode picks the SOCKS5 reply for a failed dial.
func socksCode(err error) byte {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(err.Error(), "refused"):
		return 5 // connection refused (netstack's error is only text)
	case errors.Is(err, syscall.ENETUNREACH):
		return 3 // network unreachable
	case errors.As(err, &dnsErr), errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "timed out"):
		return 4 // host unreachable
	}
	return 1 // general failure
}

// httpProxy serves CONNECT tunnels and plain absolute-form requests
// ("GET http://host/path"), both dialled through r.
func httpProxy(r *router) http.Handler {
	fwd := &httputil.ReverseProxy{
		Rewrite: func(*httputil.ProxyRequest) {}, // the URL is already absolute
		Transport: &http.Transport{
			DialContext:         r.dial,
			MaxIdleConns:        4, // memory is short on the device
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: dialTimeout,
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			if !req.URL.IsAbs() || req.URL.Host == "" {
				http.Error(w, "this is a proxy: send absolute URLs or CONNECT", http.StatusBadRequest)
				return
			}
			fwd.ServeHTTP(w, req)
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), dialTimeout)
		rc, err := r.dial(ctx, "tcp", req.Host)
		cancel()
		if err != nil {
			log.Printf("connect %s: %v", req.Host, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer rc.Close()
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Time{})
		if _, err := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
			return
		}
		if n := brw.Reader.Buffered(); n > 0 {
			p, _ := brw.Reader.Peek(n)
			if _, err := rc.Write(p); err != nil {
				return
			}
		}
		pipe(c, rc)
	})
}

// The rest is miniscale-fwd's pipe, which has the reasons.

var halfIdle = 2 * time.Minute

var writeStall = 2 * time.Minute

// pipe copies both ways until both sides are done. Once one direction ends
// (and the other side is told with a half-close), the other direction gets
// an idle timeout, renewed with every read. A write that fails or stalls
// ends both directions.
func pipe(a, b net.Conn) {
	var half, broken atomic.Bool
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			if half.Load() {
				src.SetReadDeadline(time.Now().Add(halfIdle))
			}
			n, err := src.Read(buf)
			if n > 0 {
				dst.SetWriteDeadline(time.Now().Add(writeStall))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					broken.Store(true)
					break
				}
			}
			if err != nil {
				break
			}
		}
		if broken.Load() {
			now := time.Now()
			a.SetDeadline(now)
			b.SetDeadline(now)
		} else {
			closeWrite(dst)
			if !half.Swap(true) {
				a.SetReadDeadline(time.Now().Add(halfIdle))
				b.SetReadDeadline(time.Now().Add(halfIdle))
			}
		}
		done <- struct{}{}
	}
	go cp(b, a)
	go cp(a, b)
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}
