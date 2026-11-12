package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNetworkOf(t *testing.T) {
	d := t.TempDir()
	if networkOf(d) != netTailscale {
		t.Fatal("default isn't Tailscale")
	}
	os.WriteFile(filepath.Join(d, "network"), []byte("wireguard\n"), 0o644)
	if networkOf(d) != netWireGuard {
		t.Fatal("network file not read")
	}
}

func TestWGConfigs(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "home.conf"), []byte("# mine\n[Interface]\nPrivateKey = x\n"), 0o600)
	os.WriteFile(filepath.Join(d, "miniscale.conf"), []byte("[Interface]\n"), 0o644) // ours, never offered
	os.WriteFile(filepath.Join(d, "other.conf"), []byte("SOCKS_PORT=1055\n"), 0o644)
	os.WriteFile(filepath.Join(d, "._home.conf"), []byte("[Interface]\n"), 0o644) // macOS litter
	got := wgConfigs(d, filepath.Join(d, "missing"))
	if len(got) != 1 || filepath.Base(got[0]) != "home.conf" {
		t.Fatalf("configs: %v", got)
	}
}

// The status comes over miniscale-wg's unix socket, and a peer counts as
// connected while its handshake is recent.
func TestWGState(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "wg.sock")
	if wgState(sock) != nil {
		t.Fatal("status without miniscale-wg")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wgStatus{Up: true, Addresses: []string{"10.0.0.2/32"},
			Peers: []wgPeer{{Endpoint: "vpn.example.com:51820", LastHandshake: now.Add(-30 * time.Second).Unix(), Rx: 3 << 20, Tx: 512}}})
	}))
	defer ln.Close()
	st := wgState(sock)
	if st == nil || len(st.Peers) != 1 || !st.Peers[0].connected(now) {
		t.Fatalf("status: %+v", st)
	}
	items := wgItems(st, true, false, now)
	if items[0].Label != "Connected" || items[0].Detail != "↓ 3.0 MB  ↑ 512 B" {
		t.Errorf("first row: %q %q", items[0].Label, items[0].Detail)
	}
	if (wgPeer{LastHandshake: now.Add(-5 * time.Minute).Unix()}).connected(now) || (wgPeer{}).connected(now) {
		t.Error("an old or missing handshake counts as connected")
	}
	if got := wgItems(nil, false, false, now)[0].Label; got != "No config yet" {
		t.Errorf("without a config: %q", got)
	}
}
