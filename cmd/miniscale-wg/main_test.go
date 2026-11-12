package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const (
	k1 = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="
	k2 = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
	k3 = "TrMvSoP4jYQlY6RIzBgbssQqY3vxI2Pi+y71lOWWXX0="
)

func TestParse(t *testing.T) {
	good := `# a comment
[Interface]
PrivateKey = ` + k1 + `
Address = 10.0.0.2/32, fd00::2/128
dns = 1.1.1.1, home.lan, 2606:4700::1111
PostUp = iptables -A FORWARD   # wg-quick only: ignored
Table = off

[Peer]
PublicKey = ` + k2 + `
PresharedKey = ` + k3 + `
Endpoint = vpn.example.com:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25

[peer]
publickey = ` + k3 + `
endpoint = 192.0.2.1:51820
allowedips = 10.1.0.0/16,192.168.5.7
`
	c, err := parse(strings.NewReader(good), "wg0.conf")
	if err != nil {
		t.Fatal(err)
	}
	if c.mtu != 1420 || len(c.addresses) != 2 || len(c.peers) != 2 {
		t.Fatalf("got %+v", c)
	}
	if got := fmt.Sprint(c.dns); got != "[1.1.1.1 2606:4700::1111]" {
		t.Errorf("dns %s", got)
	}
	if p := c.peers[0]; p.endpoint != "vpn.example.com:51820" || p.keepalive != 25 || p.presharedKey == nil || len(p.allowedIPs) != 2 {
		t.Errorf("peer 1 %+v", p)
	}
	if got := fmt.Sprint(c.peers[1].allowedIPs); got != "[10.1.0.0/16 192.168.5.7/32]" {
		t.Errorf("peer 2 allowed %s", got)
	}
	ipc := c.ipc()
	// the named endpoint and its keepalive wait for resolveEndpoints
	if strings.Contains(ipc, "vpn.example.com") || strings.Contains(ipc, "persistent_keepalive_interval") {
		t.Errorf("ipc has the unresolved peer's endpoint:\n%s", ipc)
	}
	if !strings.Contains(ipc, "endpoint=192.0.2.1:51820\n") {
		t.Errorf("ipc lacks peer 2's endpoint:\n%s", ipc)
	}

	iface := "[Interface]\nPrivateKey = " + k1 + "\nAddress = 10.0.0.2/32\n"
	peer := "[Peer]\nPublicKey = " + k2 + "\n"
	for _, bad := range []string{
		"",
		peer,
		iface,
		"[Interface]\nAddress = 10.0.0.2/32\n" + peer,
		"[Interface]\nPrivateKey = " + k1 + "\n" + peer,
		iface + "[Peer]\nAllowedIPs = 10.0.0.0/8\n",
		iface + "MTU = 9x\n" + peer,
		iface + "Address = 10.0.0/24\n" + peer,
		iface + peer + "Endpoint = host\n",
		iface + peer + "Endpoint = host:0\n",
		iface + peer + "AllowedIPs = 10.0.0.0/33\n",
		iface + peer + "PresharedKey = short\n",
		iface + "[Wat]\n" + peer,
		iface + "nonsense\n" + peer,
		"PrivateKey = " + k1 + "\n",
	} {
		if c, err := parse(strings.NewReader(bad), "x.conf"); err == nil {
			t.Errorf("%q: accepted as %+v", bad, c)
		} else if strings.Contains(err.Error(), "\n") {
			t.Errorf("%q: error on two lines: %v", bad, err)
		}
	}
	// a bad key's message doesn't repeat it
	_, err = parse(strings.NewReader("[Interface]\nPrivateKey = secretsecret\n"), "x.conf")
	if err == nil || strings.Contains(err.Error(), "secretsecret") {
		t.Errorf("bad key: %v", err)
	}
}

func TestViaTunnel(t *testing.T) {
	split := &router{allowed: []netip.Prefix{netip.MustParsePrefix("10.8.0.0/24"), netip.MustParsePrefix("fd00::/64")}}
	full := &router{allowed: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}}
	for _, tc := range []struct {
		r    *router
		ip   string
		want bool
	}{
		{split, "10.8.0.5", true},
		{split, "::ffff:10.8.0.5", true},
		{split, "10.8.1.5", false},
		{split, "192.168.1.10", false},
		{split, "fd00::7", true},
		{split, "fd01::7", false},
		{full, "192.168.1.10", true},
		{full, "2001:db8::1", true},
	} {
		if got := tc.r.viaTunnel(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// echoServer echoes each connection back, on loopback.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// roundTrip sends a line on c, half-closes, and wants it back.
func roundTrip(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	c.(interface{ CloseWrite() error }).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSOCKS(t *testing.T) {
	echo := echoServer(t)
	ln := listen(t)
	go serveSOCKS(ln, &router{})

	// by hand, with the domain address type, to see the raw reply
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, port, _ := net.SplitHostPort(echo)
	var pn int
	fmt.Sscan(port, &pn)
	host := "localhost"
	req := append([]byte{5, 1, 0, 5, 1, 0, 3, byte(len(host))}, host...)
	req = append(req, byte(pn>>8), byte(pn))
	c.Write(req)
	reply := make([]byte, 12)
	if _, err := io.ReadFull(c, reply); err != nil || reply[1] != 0 || reply[3] != 0 {
		t.Fatalf("handshake %v, %v", reply, err)
	}
	roundTrip(t, c)

	// refused: the echo server's port once it's closed
	dead := listen(t)
	deadAddr := dead.Addr().String()
	dead.Close()
	d, _ := proxy.SOCKS5("tcp", ln.Addr().String(), nil, proxy.Direct)
	if _, err := d.Dial("tcp", deadAddr); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("dead port: %v", err)
	}
	// BIND isn't supported
	c2, _ := net.Dial("tcp", ln.Addr().String())
	defer c2.Close()
	c2.Write([]byte{5, 1, 0, 5, 2, 0, 1, 127, 0, 0, 1, 0, 80})
	io.ReadFull(c2, reply)
	if reply[3] != 7 {
		t.Errorf("BIND reply %v", reply)
	}
}

func TestHTTPConnect(t *testing.T) {
	echo := echoServer(t)
	ln := listen(t)
	go http.Serve(ln, httpProxy(&router{}))

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo, echo)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	roundTrip(t, c)

	// a plain absolute-form request
	srv := listen(t)
	go http.Serve(srv, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "plain "+r.URL.Path) }))
	purl, _ := http.NewRequest("GET", "http://"+ln.Addr().String(), nil)
	cl := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl.URL)}}
	res, err := cl.Get("http://" + srv.Addr().String() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "plain /x" {
		t.Errorf("plain: %q", body)
	}
}

func key(t *testing.T) (priv, pub string) {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString
	return enc(k.Bytes()), enc(k.PublicKey().Bytes())
}

// TestTunnel runs two WireGuard devices over loopback UDP: a "server" with
// an HTTP server on its netstack, and miniscale-wg's own start() as the
// client. A request through the SOCKS5 proxy has to cross the tunnel.
func TestTunnel(t *testing.T) {
	sPriv, sPub := key(t)
	cPriv, cPub := key(t)

	// the server
	scfg, err := parse(strings.NewReader(`[Interface]
PrivateKey = `+sPriv+`
Address = 10.9.0.1/24
[Peer]
PublicKey = `+cPub+`
AllowedIPs = 10.9.0.2/32
`), "server.conf")
	if err != nil {
		t.Fatal(err)
	}
	stun, snet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.9.0.1")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	sdev := device.NewDevice(stun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "server: "))
	defer sdev.Close()
	if err := sdev.IpcSet(scfg.ipc()); err != nil {
		t.Fatal(err)
	}
	if err := sdev.Up(); err != nil {
		t.Fatal(err)
	}
	get, _ := sdev.IpcGet()
	var port string
	for _, l := range strings.Split(get, "\n") {
		if v, ok := strings.CutPrefix(l, "listen_port="); ok {
			port = v
		}
	}
	sl, err := snet.ListenTCP(&net.TCPAddr{IP: net.IPv4(10, 9, 0, 1), Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	go http.Serve(sl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "through the tunnel from "+r.RemoteAddr)
	}))

	// the client, as main sets it up
	ccfg, err := parse(strings.NewReader(`[Interface]
PrivateKey = `+cPriv+`
Address = 10.9.0.2/32
[Peer]
PublicKey = `+sPub+`
Endpoint = 127.0.0.1:`+port+`
AllowedIPs = 10.9.0.0/24
PersistentKeepalive = 25
`), "client.conf")
	if err != nil {
		t.Fatal(err)
	}
	tun, err := start(ccfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.dev.Close()
	ln := listen(t)
	go serveSOCKS(ln, tun.router)

	d, _ := proxy.SOCKS5("tcp", ln.Addr().String(), nil, proxy.Direct)
	cl := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.(proxy.ContextDialer).DialContext(ctx, network, addr)
		}},
	}
	res, err := cl.Get("http://10.9.0.1/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.HasPrefix(string(body), "through the tunnel from 10.9.0.2:") {
		t.Fatalf("got %q", body)
	}

	// status, after a handshake
	rec := httptest.NewRecorder()
	statusHandler(tun.dev, ccfg).ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var s statusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	if !s.Up || len(s.Peers) != 1 || s.Peers[0].PublicKey != sPub || s.Peers[0].LastHandshake == 0 ||
		s.Peers[0].Rx == 0 || s.Peers[0].Endpoint != "127.0.0.1:"+port || fmt.Sprint(s.Peers[0].AllowedIPs) != "[10.9.0.0/24]" {
		t.Errorf("status %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), cPriv) {
		t.Error("status shows the private key")
	}
}
