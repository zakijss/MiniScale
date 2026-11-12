package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zakijss/minideck/qr"
	"github.com/zakijss/minideck/ui"
)

// NetBird runs as its own daemon (bin/netbird, in userspace mode), started
// by miniscale.sh; the screen talks to it through the daemon's JSON socket,
// which serves the same calls as the netbird command.

const netNetBird = "netbird"

// nbCloud is NetBird's own management server, the default.
const nbCloud = "https://api.netbird.io:443"

type nbClient struct {
	http *http.Client
}

func newNBClient(sock string, timeout time.Duration) *nbClient {
	return &nbClient{&http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}}
}

// call posts body to the daemon's method and decodes the answer into out.
func (c *nbClient) call(method string, body, out any) error {
	b, _ := json.Marshal(body)
	resp, err := c.http.Post("http://netbird/daemon.DaemonService/"+method, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		var e struct{ Message string }
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			return errors.New(e.Message)
		}
		return fmt.Errorf("NetBird answered HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// i64 is a protobuf int64, which the JSON gateway writes as a string.
type i64 int64

func (n *i64) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	*n = i64(v)
	return err
}

// nbStatus is the daemon's StatusResponse, the parts shown.
type nbStatus struct {
	Status     string `json:"status"` // Idle, Connecting, Connected, NeedsLogin, LoginFailed, SessionExpired
	FullStatus struct {
		ManagementState struct {
			URL       string `json:"URL"`
			Connected bool   `json:"connected"`
			Error     string `json:"error"`
		} `json:"managementState"`
		LocalPeerState struct {
			IP   string `json:"IP"`
			FQDN string `json:"fqdn"`
		} `json:"localPeerState"`
		Peers []nbPeer `json:"peers"`
	} `json:"fullStatus"`
}

type nbPeer struct {
	IP         string `json:"IP"`
	FQDN       string `json:"fqdn"`
	ConnStatus string `json:"connStatus"`
	Relayed    bool   `json:"relayed"`
	BytesRx    i64    `json:"bytesRx"`
	BytesTx    i64    `json:"bytesTx"`
}

func (c *nbClient) status() (*nbStatus, error) {
	var st nbStatus
	return &st, c.call("Status", map[string]any{"getFullPeerStatus": true}, &st)
}

// needsLogin reports whether the daemon waits for a login.
func (st *nbStatus) needsLogin() bool {
	switch st.Status {
	case "NeedsLogin", "LoginFailed", "SessionExpired":
		return true
	}
	return false
}

// nbName is a peer's short name: "nas" for nas.netbird.cloud.
func nbName(fqdn, ip string) string {
	if n, _, _ := strings.Cut(fqdn, "."); n != "" {
		return n
	}
	return ip
}

// nbItems are the rows of the NetBird screen; st is nil while it's off.
func nbItems(st *nbStatus, boot bool) []ui.Item {
	var out []ui.Item
	switch {
	case st == nil:
		out = append(out, ui.Item{Label: "Off", Sub: "Press A to turn NetBird on", Icon: "wifi", Tint: ui.Faint, Value: row{"nb-on", ""}})
	case st.needsLogin():
		sub := "Scan a QR code, or type a setup key"
		if st.Status == "SessionExpired" {
			sub = "The session expired: log in again"
		}
		out = append(out, ui.Item{Label: "Log in", Sub: sub, Icon: "person", Value: row{"nb-login", ""}, Chevron: true})
	default:
		lp := st.FullStatus.LocalPeerState
		it := ui.Item{Label: st.Status, Sub: nonEmptyJoin(lp.IP, lp.FQDN), Icon: "wifi", Value: row{"nb-toggle", ""}}
		if st.Status != "Connected" {
			it.Tint = ui.Faint
		}
		if st.Status == "Idle" {
			it.Label, it.Sub = "Disconnected", "Press A to connect"
		}
		on := 0
		for _, p := range st.FullStatus.Peers {
			if p.ConnStatus == "Connected" {
				on++
			}
		}
		if n := len(st.FullStatus.Peers); n > 0 {
			it.Detail = fmt.Sprintf("%d of %d online", on, n)
		}
		out = append(out, it)
		peers := slices.Clone(st.FullStatus.Peers)
		slices.SortStableFunc(peers, func(x, y nbPeer) int {
			if (x.ConnStatus == "Connected") != (y.ConnStatus == "Connected") {
				if x.ConnStatus == "Connected" {
					return -1
				}
				return 1
			}
			return strings.Compare(nbName(x.FQDN, x.IP), nbName(y.FQDN, y.IP))
		})
		for _, p := range peers {
			pi := ui.Item{Label: nbName(p.FQDN, p.IP), Sub: p.IP, Icon: "server", Detail: strings.ToLower(p.ConnStatus)}
			if p.ConnStatus == "Connected" {
				pi.Detail = "direct"
				if p.Relayed {
					pi.Detail = "relayed"
				}
			} else {
				pi.Tint = ui.Faint
			}
			out = append(out, pi)
		}
		if m := st.FullStatus.ManagementState.URL; m != "" {
			out = append(out, ui.Item{Label: "Server", Sub: serverHost(m), Icon: "gear"})
		}
		out = append(out, ui.Item{Label: "Log out", Sub: "This device leaves your NetBird network", Icon: "cross", Value: row{"nb-logout", ""}})
	}
	return append(out,
		ui.Item{Label: "Start at boot", Detail: onOff(boot), Icon: "check", Value: row{"boot", ""}},
		ui.Item{Label: "Switch network", Sub: "Tailscale, Headscale or WireGuard", Icon: "gear", Value: row{"network", ""}, Chevron: true},
		ui.Item{Label: "Turn NetBird off", Sub: "Stop it and free its memory", Icon: "cross", Value: row{"nb-off", ""}},
		ui.Item{Label: "Appearance", Icon: "palette", Value: row{"appearance", ""}},
	)
}

// runNetBird is the app while MiniScale uses NetBird. It reports whether
// the network was switched (go on) rather than the app left.
func (a *app) runNetBird() bool {
	nb := newNBClient(filepath.Join(runDir(), "netbird-json.sock"), 3*time.Second)
	var list ui.List
	var st *nbStatus
	read := func() {
		st = nil
		if s, err := nb.status(); err == nil {
			st = s
		}
	}
	read()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		_, err := os.Stat(filepath.Join(a.dir, "autostart"))
		boot := err == nil
		list.Items = nbItems(st, boot)
		list.Sel = min(list.Sel, len(list.Items)-1)
		a.drawNB(&list, st)
		select {
		case <-tick.C:
			read()
			continue
		case ev, ok := <-a.input:
			if !ok {
				return false
			}
			if !ev.Pressed || ev.Repeat && ev.Button != ui.Up && ev.Button != ui.Down {
				continue
			}
			a.note = ""
			switch ev.Button {
			case ui.B, ui.Menu:
				return false
			case ui.Select:
				a.scr.Appearance(a.input)
			case ui.X:
				switch {
				case st == nil:
					a.nbTurnOn()
				case st.Status == "Connected" || st.Status == "Connecting":
					a.nbCall(nb, "Down", "Disconnecting…", "Disconnected")
				case !st.needsLogin():
					a.nbCall(nb, "Up", "Connecting…", "Connecting")
				}
			case ui.L2:
				list.Sel = 0
			case ui.R2:
				list.Sel = len(list.Items) - 1
			case ui.A:
				it := list.Selected()
				if it == nil {
					break
				}
				r, _ := it.Value.(row)
				switch r.kind {
				case "nb-on":
					a.nbTurnOn()
				case "nb-login":
					a.nbLogin(st)
				case "nb-toggle":
					if st != nil && (st.Status == "Connected" || st.Status == "Connecting") {
						a.nbCall(nb, "Down", "Disconnecting…", "Disconnected")
					} else {
						a.nbCall(nb, "Up", "Connecting…", "Connecting")
					}
				case "nb-logout":
					if a.scr.Confirm(a.input, "Log out of NetBird?", "This device leaves your NetBird network. You can log in again later.", "Log out") {
						a.nbCall(nb, "Logout", "Logging out…", "Logged out")
						a.setBoot(false)
						a.saveNow()
					}
				case "nb-off":
					a.nbTurnOff()
				case "boot":
					a.setBoot(!boot)
				case "network":
					if a.switchNetwork(netNetBird) {
						return true
					}
				case "appearance":
					a.scr.Appearance(a.input)
				}
			default:
				list.Handle(ev)
			}
			read()
		}
	}
}

func (a *app) drawNB(list *ui.List, st *nbStatus) {
	s := a.scr
	sub := "NetBird · off"
	switch {
	case a.note != "":
		sub = a.note
	case st == nil:
	case st.needsLogin():
		sub = "NetBird · not logged in"
	default:
		sub = "NetBird · " + strings.ToLower(st.Status)
		if ip := st.FullStatus.LocalPeerState.IP; ip != "" {
			sub += " · " + strings.TrimSuffix(ip, "/16")
		}
	}
	hints := []ui.Hint{{Button: "A", Label: "Select"}}
	switch {
	case st == nil:
		hints = append(hints, ui.Hint{Button: "X", Label: "Turn on"})
	case st.Status == "Connected" || st.Status == "Connecting":
		hints = append(hints, ui.Hint{Button: "X", Label: "Disconnect"})
	case !st.needsLogin():
		hints = append(hints, ui.Hint{Button: "X", Label: "Connect"})
	}
	hints = append(hints, ui.Hint{Button: "B", Label: "Exit"})
	top := s.StaticFrame("MiniScale", sub, hints...)
	list.Draw(s, image.Rect(20, top, ui.W-26, ui.H-56))
	s.Present()
}

// nbCall runs a daemon call that takes no arguments behind a waiting
// screen.
func (a *app) nbCall(nb *nbClient, method, what, done string) {
	slow := newNBClient(filepath.Join(runDir(), "netbird-json.sock"), 45*time.Second)
	if err := a.busy(what, func() error { return slow.call(method, map[string]any{}, nil) }); err != nil {
		a.scr.Notice(a.input, "NetBird said no", err.Error()+tryAgain)
		return
	}
	a.note = done
}

func (a *app) nbTurnOn() {
	if err := a.busy("Starting NetBird…", func() error { return a.script("start") }); err != nil {
		a.scr.Notice(a.input, "Couldn't start NetBird", err.Error()+"\n\nminiscale.log in the app's folder has the details.")
		return
	}
	a.note = "NetBird is on"
}

func (a *app) nbTurnOff() {
	if !a.scr.Confirm(a.input, "Turn NetBird off?", "Apps that use your NetBird network stop working until you turn it on again. It won't start at boot.", "Turn off") {
		return
	}
	err := a.busy("Turning off…", func() error {
		a.script("autostart", "off")
		return a.script("stop")
	})
	if err != nil {
		a.scr.Notice(a.input, "Couldn't turn off", err.Error()+tryAgain)
		return
	}
	a.note = "NetBird is off"
}

// nbLogin logs in with a QR code (NetBird's SSO) or a setup key, to
// NetBird's cloud or your own server, then connects.
func (a *app) nbLogin(st *nbStatus) {
	server := nbCloud
	if st != nil && st.FullStatus.ManagementState.URL != "" {
		server = st.FullStatus.ManagementState.URL
	}
	where := []ui.Item{{Label: "NetBird cloud", Sub: "app.netbird.io", Icon: "server"},
		{Label: "Your own server", Sub: "A self-hosted NetBird", Icon: "gear"}}
	if !isNBCloud(server) {
		where[1].Sub = serverHost(server)
		where[0], where[1] = where[1], where[0]
	}
	i, ok := a.scr.Choose(a.input, "Log in to", where)
	if !ok {
		return
	}
	if where[i].Label == "NetBird cloud" {
		server = nbCloud
	} else {
		cur := ""
		if !isNBCloud(server) {
			cur = server
		}
		for {
			typed, ok := a.scr.Prompt(a.input, "Server address", cur, "netbird.example.com", false)
			if !ok {
				return
			}
			u, err := controlAddress(typed)
			if err == nil {
				server = u
				break
			}
			a.scr.Notice(a.input, "Check the address", err.Error())
			cur = typed
		}
	}
	how := []ui.Item{{Label: "Scan a QR code", Sub: "Log in on your phone", Icon: "person"},
		{Label: "Type a setup key", Sub: "From the NetBird dashboard", Icon: "list"}}
	j, ok := a.scr.Choose(a.input, "Log in with", how)
	if !ok {
		return
	}
	name, _ := confDefaults(a.dir)
	req := map[string]any{"managementUrl": server, "hostname": name}
	if j == 1 {
		key, ok := a.scr.Prompt(a.input, "Setup key", "", "From Setup Keys in the dashboard", true)
		if !ok || strings.TrimSpace(key) == "" {
			return
		}
		req["setupKey"] = strings.TrimSpace(key)
	}
	sock := filepath.Join(runDir(), "netbird-json.sock")
	var resp struct {
		NeedsSSOLogin           bool   `json:"needsSSOLogin"`
		UserCode                string `json:"userCode"`
		VerificationURIComplete string `json:"verificationURIComplete"`
	}
	err := a.busy("Asking NetBird…", func() error { return newNBClient(sock, 45*time.Second).call("Login", req, &resp) })
	if err != nil {
		a.scr.Notice(a.input, "Couldn't log in", err.Error()+tryAgain)
		return
	}
	if resp.NeedsSSOLogin {
		if !a.nbWaitSSO(sock, resp.VerificationURIComplete, resp.UserCode, name) {
			return
		}
	}
	if err := a.busy("Connecting…", func() error { return newNBClient(sock, 60*time.Second).call("Up", map[string]any{}, nil) }); err != nil {
		a.scr.Notice(a.input, "Logged in, but couldn't connect", err.Error()+tryAgain)
	}
	a.saveNow()
	a.setBoot(true)
	a.note = "Logged in to " + serverHost(server)
}

func isNBCloud(u string) bool {
	h := serverHost(u)
	return u == "" || h == "api.netbird.io" || h == "api.netbird.io:443" || h == "app.netbird.io"
}

// nbWaitSSO shows the login link as a QR code while the daemon waits for
// the login to finish. It reports whether it did.
func (a *app) nbWaitSSO(sock, link, code, name string) bool {
	done := make(chan error, 1)
	go func() {
		done <- newNBClient(sock, 10*time.Minute).call("WaitSSOLogin", map[string]any{"userCode": code, "hostname": name}, nil)
	}()
	qc, _ := qr.Encode(link)
	a.loginHost = "NetBird"
	for {
		a.drawLogin(link, qc, false)
		select {
		case err := <-done:
			if err != nil {
				a.scr.Notice(a.input, "The login didn't finish", err.Error()+tryAgain)
				return false
			}
			return true
		case ev, ok := <-a.input:
			if !ok {
				return false
			}
			if ev.Pressed && (ev.Button == ui.B || ev.Button == ui.Menu) {
				a.note = "Login cancelled"
				return false
			}
		case <-time.After(time.Second): // the "Waiting for you..." dots
		}
	}
}

// switchNetwork asks which network to use instead of cur, and switches
// miniscale.sh to it. It reports whether it did.
func (a *app) switchNetwork(cur string) bool {
	all := []struct {
		id    string
		item  ui.Item
		about string
	}{
		{netTailscale, ui.Item{Label: "Tailscale", Sub: "Tailscale, or your own Headscale server", Icon: "server"},
			"MiniScale joins your tailnet again. Your other logins stay saved."},
		{netWireGuard, ui.Item{Label: "WireGuard", Sub: "A plain WireGuard server, from a .conf", Icon: "wifi"},
			"MiniScale connects to a WireGuard server instead, through the same proxies, so your apps keep working. Your other logins stay saved."},
		{netNetBird, ui.Item{Label: "NetBird", Sub: "NetBird cloud or your own server · uses more memory", Icon: "list"},
			"MiniScale joins a NetBird network instead. NetBird needs about twice Tailscale's memory, so close big apps if the Miyoo slows down. Your other logins stay saved."},
	}
	var items []ui.Item
	var ids, abouts []string
	for _, n := range all {
		if n.id != cur {
			items = append(items, n.item)
			ids = append(ids, n.id)
			abouts = append(abouts, n.about)
		}
	}
	i, ok := a.scr.Choose(a.input, "Use which network?", items)
	if !ok || !a.scr.Confirm(a.input, "Use "+items[i].Label+"?", abouts[i], "Switch") {
		return false
	}
	if err := a.busy("Switching…", func() error { return a.script("network", ids[i]) }); err != nil {
		a.scr.Notice(a.input, "Couldn't switch", err.Error()+tryAgain)
		return false
	}
	return true
}
