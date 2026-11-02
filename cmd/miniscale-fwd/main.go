// miniscale-fwd forwards local TCP ports to hosts on a tailnet through
// tailscaled's SOCKS5 proxy, so apps without proxy support (players,
// clients, browsers) can reach tailnet services at 127.0.0.1:PORT.
//
// Config file, one forward per line ("#" starts a comment):
//
//	8096  homeserver:8096        # Jellyfin
//	4533  nas:4533     # Navidrome
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"github.com/zakijss/minideck/version"
)

// forward is one line of the config: a local port and the tailnet
// HOST:PORT it leads to.
type forward struct {
	local  int
	target string
}

// parse reads the config file at path.
func parse(path string) ([]forward, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []forward
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"LOCALPORT HOST:PORT\"", path, n)
		}
		p, err := strconv.Atoi(fields[0])
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("%s:%d: bad local port %q", path, n, fields[0])
		}
		if h, port, err := net.SplitHostPort(fields[1]); err != nil || h == "" || port == "" {
			return nil, fmt.Errorf("%s:%d: bad target %q", path, n, fields[1])
		}
		out = append(out, forward{p, fields[1]})
	}
	return out, sc.Err()
}

func main() {
	version.Flag("miniscale-fwd")
	version.Note("miniscale-fwd")
	conf := flag.String("c", "forwards.conf", "config file")
	socks := flag.String("socks", "127.0.0.1:1055", "tailscaled SOCKS5 address")
	flag.Parse()

	fwds, err := parse(*conf)
	if err != nil {
		log.Fatal(err)
	}
	if len(fwds) == 0 {
		log.Printf("no forwards in %s; exiting", *conf)
		return
	}
	dial := socksDialer(*socks)

	open := 0
	for _, fw := range fwds {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", fw.local))
		if err != nil {
			log.Printf("listen %d: %v", fw.local, err)
			continue
		}
		log.Printf("127.0.0.1:%d -> %s", fw.local, fw.target)
		go serve(ln, fw.target, dial)
		open++
	}
	if open == 0 {
		log.Fatal("no port could be opened")
	}
	select {}
}

// dialFunc connects to a tailnet HOST:PORT.
type dialFunc func(ctx context.Context, target string) (net.Conn, error)

// socksDialer connects through the SOCKS5 proxy at addr. It hands back the
// TCP connection to the proxy itself rather than x/net/proxy's wrapper,
// which hides CloseWrite: closeWrite would then close the connection
// outright, and a client that half-closes once it has sent its request
// (nc -N, some HTTP clients) would never get the reply. After the
// handshake the proxy only relays bytes, and the handshake reads exactly
// its own bytes, so the bare connection carries the target's stream.
func socksDialer(addr string) dialFunc {
	return func(ctx context.Context, target string) (net.Conn, error) {
		var raw net.Conn
		toProxy := ctxDialer(func(ctx context.Context, network, address string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, address)
			raw = c
			return c, err
		})
		d, err := proxy.SOCKS5("tcp", addr, nil, toProxy)
		if err != nil {
			return nil, err
		}
		if _, err := d.(proxy.ContextDialer).DialContext(ctx, "tcp", target); err != nil {
			return nil, err // the proxy connection is closed by then
		}
		return raw, nil
	}
}

// ctxDialer makes a function a proxy.Dialer and a proxy.ContextDialer.
type ctxDialer func(ctx context.Context, network, address string) (net.Conn, error)

func (f ctxDialer) Dial(network, address string) (net.Conn, error) {
	return f(context.Background(), network, address)
}

func (f ctxDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

// serve accepts connections on ln and pipes each to target through dial.
func serve(ln net.Listener, target string, dial dialFunc) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			log.Printf("accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go func() {
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			r, err := dial(ctx, target)
			cancel()
			if err != nil {
				log.Printf("dial %s: %v", target, err)
				return
			}
			defer r.Close()
			pipe(c, r)
		}()
	}
}

// halfIdle is how long a connection may stay quiet after one side has
// finished sending. Without it, a peer that never closes its side would
// keep the connection (and two goroutines) forever in this always-on
// process.
var halfIdle = 2 * time.Minute

// writeStall is how long a write may wait for the receiving side to take
// the data, for the same reason: a peer that stops reading would block a
// copy forever.
var writeStall = 2 * time.Minute

// pipe copies both ways until both sides are done. Once one direction ends
// (and the other side is told with a half-close), the other direction gets
// an idle timeout, renewed with every read. A write that fails or stalls
// ends both directions: the connection can't carry anything more.
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
			// wake the other copy at once, wherever it is blocked
			now := time.Now()
			a.SetDeadline(now)
			b.SetDeadline(now)
		} else {
			closeWrite(dst)
			if !half.Swap(true) {
				// the other copy may be blocked in a read with no deadline
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

// closeWrite half-closes a connection when it supports it, so the other
// side sees EOF while replies can still flow back.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}
