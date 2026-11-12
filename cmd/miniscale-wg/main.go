// miniscale-wg runs a plain WireGuard tunnel in userspace and puts it
// behind local proxies, the way MiniScale runs Tailscale: the Miyoo has no
// /dev/net/tun and no iptables, so apps reach the tunnel through SOCKS5
// (127.0.0.1:1055) or an HTTP proxy (127.0.0.1:1056) instead of a network
// interface. The tunnel's TCP/IP stack is gVisor's netstack, in-process.
//
// The config is a wg-quick file. Addresses inside a peer's AllowedIPs go
// through the tunnel, the rest straight out, so split and full tunnels both
// work. Only TCP is proxied.
//
// With -via-socks there is no tunnel: only the HTTP proxy runs, dialling
// through a SOCKS5 proxy someone else serves. NetBird's client offers
// SOCKS5 only; this gives apps that speak HTTP proxy a way in too.
//
//	miniscale-wg -conf wg0.conf [-socks ADDR] [-http ADDR] [-status SOCKET] [-routes PATH]
//	miniscale-wg -via-socks ADDR [-http ADDR]
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/zakijss/minideck/version"
)

func main() {
	version.Flag("miniscale-wg")
	version.Note("miniscale-wg")
	conf := flag.String("conf", "", "wg-quick config file")
	socks := flag.String("socks", "127.0.0.1:1055", "SOCKS5 proxy address (loopback)")
	httpAddr := flag.String("http", "127.0.0.1:1056", "HTTP proxy address (loopback)")
	status := flag.String("status", "", "unix socket for GET /status (optional)")
	routes := flag.String("routes", "", "file listing what to send through the proxies, for the apps (optional)")
	viaSocks := flag.String("via-socks", "", "no tunnel: serve only the HTTP proxy, through this SOCKS5 proxy (loopback)")
	flag.Parse()
	debug.SetGCPercent(50) // small heap: collect earlier than Go's default

	// Problems the user can fix exit 2 with one line, which the app shows.
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "miniscale-wg:", err)
		os.Exit(2)
	}
	if (*conf == "") == (*viaSocks == "") {
		fail(errors.New("give one of -conf and -via-socks"))
	}
	if *viaSocks != "" {
		if err := serveViaSOCKS(*viaSocks, *httpAddr); err != nil {
			fail(err)
		}
		return
	}
	for _, a := range []string{*socks, *httpAddr} {
		if !loopback(a) {
			fail(fmt.Errorf("%s: proxies listen on loopback only", a))
		}
	}
	cfg, err := parseFile(*conf)
	if err != nil {
		fail(err)
	}

	// a port taken (by tailscaled, say) is worth saying plainly
	sl, err := net.Listen("tcp", *socks)
	if err != nil {
		fail(err)
	}
	hl, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		fail(err)
	}
	t, err := start(cfg)
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go resolveEndpoints(ctx, t.dev, cfg)

	go serveSOCKS(sl, t.router)
	hs := &http.Server{Handler: httpProxy(t.router.dial), ReadHeaderTimeout: dialTimeout}
	go hs.Serve(hl)
	var ss *http.Server
	if *status != "" {
		os.Remove(*status) // left by an earlier run that was killed
		ul, err := net.Listen("unix", *status)
		if err != nil {
			log.Fatal(err)
		}
		ss = &http.Server{Handler: statusHandler(t.dev, cfg), ReadHeaderTimeout: 5 * time.Second}
		go ss.Serve(ul)
	}
	if *routes != "" {
		if err := writeRoutes(*routes, t.router.allowed); err != nil {
			log.Printf("routes: %v", err) // the proxies work without it
		} else {
			defer os.Remove(*routes)
		}
	}
	log.Printf("up: %d peer(s), SOCKS5 %s, HTTP %s", len(cfg.peers), *socks, *httpAddr)

	<-ctx.Done()
	sl.Close()
	hs.Close()
	if ss != nil {
		ss.Close() // removes the socket file too
	}
	t.dev.Close()
	log.Print("stopped")
}

// serveViaSOCKS runs only the HTTP proxy, every connection dialled through
// the SOCKS5 proxy at upstream, until SIGTERM or SIGINT. The upstream owns
// the SOCKS port, so it isn't listened on here.
func serveViaSOCKS(upstream, httpAddr string) error {
	for _, a := range []string{upstream, httpAddr} {
		if !loopback(a) {
			return fmt.Errorf("%s: proxies listen on loopback only", a)
		}
	}
	h := viaSOCKS(upstream)
	hl, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	hs := &http.Server{Handler: h, ReadHeaderTimeout: dialTimeout}
	go hs.Serve(hl)
	log.Printf("up: HTTP %s via SOCKS5 %s", httpAddr, upstream)
	<-ctx.Done()
	hs.Close()
	log.Print("stopped")
	return nil
}

// viaSOCKS is the HTTP proxy dialling through the SOCKS5 proxy at upstream.
// Names go to it unresolved: it knows the tunnel's DNS.
func viaSOCKS(upstream string) http.Handler {
	nd := &net.Dialer{Timeout: dialTimeout}
	return httpProxy(func(ctx context.Context, network, addr string) (net.Conn, error) {
		// x/net's SOCKS conn has no CloseWrite, so pipe's half-close would
		// close it whole and lose the reply. After the handshake it's the
		// TCP conn underneath, unbuffered: hand that over instead.
		var raw net.Conn
		fwd := dialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := nd.DialContext(ctx, network, addr)
			raw = c
			return c, err
		})
		d, err := proxy.SOCKS5("tcp", upstream, nil, fwd)
		if err != nil {
			return nil, err
		}
		if _, err := d.(proxy.ContextDialer).DialContext(ctx, network, addr); err != nil {
			return nil, err
		}
		return raw, nil
	})
}

// dialFunc is a dial function as the proxy.Dialer x/net wants.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f dialFunc) Dial(network, addr string) (net.Conn, error) {
	return f(context.Background(), network, addr)
}

func (f dialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

// writeRoutes tells the apps what to send through the proxies: each
// AllowedIPs prefix, then "*" for every host name, since names are looked
// up with the tunnel's DNS and routed here. Written whole then renamed, so
// a reader never sees half a file.
func writeRoutes(path string, allowed []netip.Prefix) error {
	var b strings.Builder
	b.WriteString("# written by miniscale-wg; read by MiniDeck apps\n")
	for _, p := range allowed {
		fmt.Fprintf(&b, "%s\n", p)
	}
	b.WriteString("*\n")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	os.Chmod(tmp, 0o644) // WriteFile's mode is cut by the umask, and other apps read this
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// loopback reports whether a listen address is on the loopback interface.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

type tunnel struct {
	dev    *device.Device
	router *router
}

// start brings the WireGuard device up on a netstack TUN.
func start(cfg *config) (*tunnel, error) {
	var addrs []netip.Addr
	for _, p := range cfg.addresses {
		addrs = append(addrs, p.Addr())
	}
	tdev, tnet, err := netstack.CreateNetTUN(addrs, cfg.dns, cfg.mtu)
	if err != nil {
		return nil, err
	}
	logger := &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf:   func(f string, a ...any) { log.Printf("wireguard: "+f, a...) },
	}
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), logger)
	if err := dev.IpcSet(cfg.ipc()); err != nil {
		dev.Close()
		return nil, err
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, err
	}
	r := &router{tnet: tnet, dns: len(cfg.dns) > 0, direct: net.Dialer{Timeout: dialTimeout}}
	for _, p := range cfg.peers {
		r.allowed = append(r.allowed, p.allowedIPs...)
	}
	if r.fullTunnel() && !r.dns {
		log.Printf("warning: everything goes through the tunnel but the config has no DNS: names are looked up with the local network's DNS, outside it")
	}
	return &tunnel{dev, r}, nil
}

// resolveEndpoints gives peers whose endpoint is a name its address, with
// the system resolver. At boot Wi-Fi may not be up yet, so it retries,
// backing off to a minute between tries.
func resolveEndpoints(ctx context.Context, dev *device.Device, cfg *config) {
	for _, p := range cfg.peers {
		host, port, _ := net.SplitHostPort(p.endpoint)
		if host == "" {
			continue
		}
		if _, err := netip.ParseAddr(host); err == nil {
			continue // set by start already
		}
		go func() {
			wait := time.Second
			for logged := false; ; {
				ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
				if err == nil && len(ips) > 0 {
					ip := ips[0]
					for _, a := range ips {
						if a.Unmap().Is4() { // most Wi-Fi has no IPv6 route out
							ip = a
							break
						}
					}
					set := fmt.Sprintf("public_key=%s\nupdate_only=true\nendpoint=%s\n",
						hex.EncodeToString(p.publicKey), net.JoinHostPort(ip.Unmap().String(), port))
					if p.keepalive > 0 {
						set += fmt.Sprintf("persistent_keepalive_interval=%d\n", p.keepalive)
					}
					if err := dev.IpcSet(set); err != nil {
						log.Printf("endpoint %s: %v", p.endpoint, err)
					} else {
						log.Printf("endpoint %s is %s", host, ip.Unmap())
					}
					return
				}
				if !logged {
					log.Printf("endpoint %s: %v; retrying", host, err)
					logged = true
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				wait = min(wait*2, time.Minute)
			}
		}()
	}
}

type peerStatus struct {
	PublicKey     string   `json:"publicKey"`
	Endpoint      string   `json:"endpoint"`
	LastHandshake int64    `json:"lastHandshake"`
	Rx            int64    `json:"rx"`
	Tx            int64    `json:"tx"`
	AllowedIPs    []string `json:"allowedIPs"`
}

type statusJSON struct {
	Up        bool         `json:"up"`
	Addresses []string     `json:"addresses"`
	DNS       []string     `json:"dns"`
	Peers     []peerStatus `json:"peers"`
}

// statusHandler serves GET /status, from the device's own account of its
// peers (IpcGet).
func statusHandler(dev *device.Device, cfg *config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		s := statusJSON{Up: true, Addresses: []string{}, DNS: []string{}, Peers: []peerStatus{}}
		for _, a := range cfg.addresses {
			s.Addresses = append(s.Addresses, a.String())
		}
		for _, a := range cfg.dns {
			s.DNS = append(s.DNS, a.String())
		}
		get, err := dev.IpcGet()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.Peers = parseIpcPeers(get)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	})
	return mux
}

// parseIpcPeers reads the peers out of an IpcGet dump. The dump starts
// with the private key: it's skipped, as is everything else not reported.
func parseIpcPeers(get string) []peerStatus {
	peers := []peerStatus{}
	for _, line := range strings.Split(get, "\n") {
		k, v, _ := strings.Cut(line, "=")
		if k == "public_key" {
			key, _ := hex.DecodeString(v)
			peers = append(peers, peerStatus{PublicKey: base64.StdEncoding.EncodeToString(key), AllowedIPs: []string{}})
			continue
		}
		if len(peers) == 0 {
			continue
		}
		p := &peers[len(peers)-1]
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "endpoint":
			p.Endpoint = v
		case "last_handshake_time_sec":
			p.LastHandshake = n
		case "rx_bytes":
			p.Rx = n
		case "tx_bytes":
			p.Tx = n
		case "allowed_ip":
			p.AllowedIPs = append(p.AllowedIPs, v)
		}
	}
	return peers
}
