package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// The daemon's JSON gateway writes int64s as strings; the status reads
// them, and the rows put connected peers first.
func TestNBStatus(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nb.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var asked map[string]any
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/daemon.DaemonService/Status" {
			http.Error(w, `{"code":5,"message":"no such call"}`, 404)
			return
		}
		json.NewDecoder(r.Body).Decode(&asked)
		w.Write([]byte(`{"status":"Connected","fullStatus":{"managementState":{"URL":"https://api.netbird.io:443","connected":true},
			"localPeerState":{"IP":"100.90.1.2/16","fqdn":"miyoo.netbird.cloud"},
			"peers":[{"IP":"100.90.1.9","fqdn":"zeta.netbird.cloud","connStatus":"Idle","bytesRx":"0"},
			         {"IP":"100.90.1.5","fqdn":"nas.netbird.cloud","connStatus":"Connected","relayed":true,"bytesRx":"1048576","bytesTx":"12"}]}}`))
	}))
	c := newNBClient(sock, 0)
	st, err := c.status()
	if err != nil {
		t.Fatal(err)
	}
	if asked["getFullPeerStatus"] != true {
		t.Errorf("asked %v", asked)
	}
	if st.FullStatus.Peers[1].BytesRx != 1<<20 || st.needsLogin() {
		t.Fatalf("status: %+v", st)
	}
	items := nbItems(st, true)
	if items[0].Label != "Connected" || items[0].Detail != "1 of 2 online" {
		t.Errorf("first row %q %q", items[0].Label, items[0].Detail)
	}
	if items[1].Label != "nas" || items[1].Detail != "relayed" || items[2].Label != "zeta" {
		t.Errorf("peers: %q %q, %q", items[1].Label, items[1].Detail, items[2].Label)
	}
	if err := c.call("Nope", nil, nil); err == nil || err.Error() != "no such call" {
		t.Errorf("error: %v", err)
	}

	if got := nbItems(&nbStatus{Status: "NeedsLogin"}, false)[0].Label; got != "Log in" {
		t.Errorf("logged out: %q", got)
	}
	if got := nbItems(nil, false)[0].Label; got != "Off" {
		t.Errorf("off: %q", got)
	}
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "network"), []byte("netbird\n"), 0o644)
	if networkOf(d) != netNetBird {
		t.Error("networkOf netbird")
	}
	if !isNBCloud(nbCloud) || isNBCloud("https://nb.example.com") {
		t.Error("isNBCloud")
	}
}
