package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zakijss/minideck/ui"
)

// MiniScale joins either a tailnet (Tailscale or Headscale) or a plain
// WireGuard network. miniscale.sh keeps the choice in the app's "network"
// file and runs tailscaled or miniscale-wg; either way other apps reach the
// network through the same SOCKS5 and HTTP proxies.

const (
	netTailscale = "tailscale"
	netWireGuard = "wireguard"
)

// networkOf is the network miniscale.sh starts, from the app's folder.
func networkOf(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "network"))
	if strings.TrimSpace(string(b)) == netWireGuard {
		return netWireGuard
	}
	return netTailscale
}

// wgStatus is miniscale-wg's GET /status.
type wgStatus struct {
	Up        bool     `json:"up"`
	Addresses []string `json:"addresses"`
	DNS       []string `json:"dns"`
	Peers     []wgPeer `json:"peers"`
}

type wgPeer struct {
	PublicKey     string   `json:"publicKey"`
	Endpoint      string   `json:"endpoint"`
	LastHandshake int64    `json:"lastHandshake"` // Unix seconds, 0 never
	Rx            int64    `json:"rx"`
	Tx            int64    `json:"tx"`
	AllowedIPs    []string `json:"allowedIPs"`
}

// connected reports whether a peer has answered lately: WireGuard renews
// the handshake every two minutes while traffic flows, and a session dies
// after three without one.
func (p wgPeer) connected(now time.Time) bool {
	return p.LastHandshake > 0 && now.Sub(time.Unix(p.LastHandshake, 0)) < 3*time.Minute
}

// wgState reads the status over miniscale-wg's socket; nil when it isn't
// running.
func wgState(sock string) *wgStatus {
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
	resp, err := c.Get("http://wg/status")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var st wgStatus
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&st) != nil {
		return nil
	}
	return &st
}

// wgConfigs lists the WireGuard configs that could be imported: .conf files
// with an [Interface] in the app's folder, the card's top folder and the
// Taildrop folder.
func wgConfigs(dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(n), ".conf") || strings.HasPrefix(n, ".") ||
				n == "miniscale.conf" || n == "forwards.conf" {
				continue
			}
			if p := filepath.Join(d, n); looksLikeWG(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// looksLikeWG reports whether the file has an [Interface] section, as a
// WireGuard config does.
func looksLikeWG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 0; sc.Scan() && n < 200; n++ {
		if strings.EqualFold(strings.TrimSpace(sc.Text()), "[Interface]") {
			return true
		}
	}
	return false
}

// wgItems are the rows of the WireGuard screen.
func wgItems(st *wgStatus, imported, boot bool, now time.Time) []ui.Item {
	var out []ui.Item
	switch {
	case !imported:
		out = append(out, ui.Item{Label: "No config yet", Sub: "Import a .conf from your WireGuard server", Icon: "cross", Tint: ui.Faint})
	case st == nil:
		out = append(out, ui.Item{Label: "Off", Sub: "Press A to connect", Icon: "wifi", Tint: ui.Faint, Value: row{"wg-on", ""}})
	default:
		for _, p := range st.Peers {
			it := ui.Item{Label: "Waiting for the server", Icon: "wifi", Tint: ui.Faint, Value: row{"wg-off", ""}}
			if p.connected(now) {
				it.Label, it.Tint = "Connected", ui.Text
			}
			it.Sub = nonEmptyJoin(p.Endpoint, "handshake "+ago(unixOrZero(p.LastHandshake)))
			it.Detail = "↓ " + bytesText(p.Rx) + "  ↑ " + bytesText(p.Tx)
			out = append(out, it)
		}
		if len(st.Addresses) > 0 {
			out = append(out, ui.Item{Label: "This device", Sub: strings.Join(st.Addresses, ", "), Icon: "server"})
		}
		out = append(out, ui.Item{Label: "Proxies for other apps", Sub: "SOCKS5 on port 1055 · HTTP on 1056", Icon: "list"})
	}
	return append(out,
		ui.Item{Label: "Import a config", Sub: "A .conf on the card or sent with Taildrop", Icon: "plus", Value: row{"wg-import", ""}},
		ui.Item{Label: "Start at boot", Detail: onOff(boot), Icon: "check", Value: row{"boot", ""}},
		ui.Item{Label: "Use Tailscale instead", Sub: "Tailscale or Headscale", Icon: "gear", Value: row{"use-tailscale", ""}},
		ui.Item{Label: "Appearance", Icon: "palette", Value: row{"appearance", ""}},
	)
}

func unixOrZero(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0)
}

func nonEmptyJoin(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), " · ")
}

// bytesText writes a byte count: "512 B", "3.4 MB".
func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// runWireGuard is the app while MiniScale joins a WireGuard network. It
// reports whether to go on to the Tailscale screens (the network was
// switched) rather than exit.
func (a *app) runWireGuard() bool {
	sock := filepath.Join(runDir(), "wg.sock")
	var list ui.List
	var st *wgStatus
	read := func() { st = wgState(sock) }
	read()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		_, err := os.Stat(filepath.Join(a.dir, "wireguard.enc"))
		imported := err == nil
		_, err = os.Stat(filepath.Join(a.dir, "autostart"))
		list.Items = wgItems(st, imported, err == nil, time.Now())
		list.Sel = min(list.Sel, len(list.Items)-1)
		a.drawWG(&list, st, imported)
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
				if st != nil {
					a.wgTurnOff()
				} else if imported {
					a.wgTurnOn()
				}
			case ui.A:
				it := list.Selected()
				if it == nil {
					break
				}
				r, _ := it.Value.(row)
				switch r.kind {
				case "wg-on":
					a.wgTurnOn()
				case "wg-off":
					a.wgTurnOff()
				case "wg-import":
					a.wgImport(st != nil)
				case "boot":
					a.setBoot(!(func() bool { _, e := os.Stat(filepath.Join(a.dir, "autostart")); return e == nil })())
				case "use-tailscale":
					if a.scr.Confirm(a.input, "Use Tailscale?", "MiniScale disconnects from WireGuard and joins your tailnet instead. Your WireGuard config stays saved.", "Switch") {
						if err := a.busy("Switching…", func() error { return a.script("network", netTailscale) }); err != nil {
							a.scr.Notice(a.input, "Couldn't switch", err.Error()+tryAgain)
							break
						}
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

func (a *app) drawWG(list *ui.List, st *wgStatus, imported bool) {
	s := a.scr
	sub := "WireGuard · off"
	switch {
	case a.note != "":
		sub = a.note
	case !imported:
		sub = "WireGuard · no config yet"
	case st != nil:
		sub = "WireGuard · on"
		for _, p := range st.Peers {
			if p.connected(time.Now()) {
				sub = "WireGuard · connected"
			}
		}
	}
	hints := []ui.Hint{{Button: "A", Label: "Select"}}
	if st != nil {
		hints = append(hints, ui.Hint{Button: "X", Label: "Disconnect"})
	} else if imported {
		hints = append(hints, ui.Hint{Button: "X", Label: "Connect"})
	}
	hints = append(hints, ui.Hint{Button: "B", Label: "Exit"})
	top := s.StaticFrame("MiniScale", sub, hints...)
	list.Draw(s, image.Rect(20, top, ui.W-26, ui.H-56))
	s.Present()
}

func (a *app) wgTurnOn() {
	err := a.busy("Connecting…", func() error { return a.script("start") })
	if err != nil {
		a.scr.Notice(a.input, "Couldn't connect", err.Error()+"\n\nminiscale.log in the app's folder has the details.")
		return
	}
	a.setBoot(true)
	a.note = "Connected to WireGuard"
}

func (a *app) wgTurnOff() {
	if !a.scr.Confirm(a.input, "Disconnect?", "Apps that use your WireGuard network stop working until you connect again. It won't connect at boot.", "Disconnect") {
		return
	}
	err := a.busy("Disconnecting…", func() error {
		a.script("autostart", "off")
		return a.script("stop")
	})
	if err != nil {
		a.scr.Notice(a.input, "Couldn't disconnect", err.Error()+tryAgain)
		return
	}
	a.note = "Disconnected"
}

// wgImport takes a .conf from the card, keeps it encrypted, and offers to
// remove the plain file, which holds the private key.
func (a *app) wgImport(running bool) {
	files := wgConfigs(a.dir, "/mnt/SDCARD", taildropDir())
	if len(files) == 0 {
		a.scr.Notice(a.input, "No config found", "Copy the .conf your WireGuard server made for this device (wg-quick format) to the SD card's top folder or to App/MiniScale, or send it with Taildrop, then import it here.")
		return
	}
	items := make([]ui.Item, len(files))
	for i, f := range files {
		items[i] = ui.Item{Label: filepath.Base(f), Sub: filepath.Dir(f), Icon: "list"}
	}
	i, ok := a.scr.Choose(a.input, "Import which config?", items)
	if !ok {
		return
	}
	f := files[i]
	if err := a.busy("Importing…", func() error { return a.script("wg-import", f) }); err != nil {
		a.scr.Notice(a.input, "Couldn't import it", err.Error())
		return
	}
	if a.scr.Confirm(a.input, "Remove the plain file?", filepath.Base(f)+" holds your private key unencrypted. MiniScale now keeps its own encrypted copy, which only this Miyoo can open.", "Remove") {
		if err := os.Remove(f); err != nil {
			a.scr.Notice(a.input, "Couldn't remove it", err.Error())
		}
	}
	a.note = "Imported " + filepath.Base(f)
	if running { // the new config takes effect on a restart
		a.busy("Reconnecting…", func() error { a.script("stop"); return a.script("start") })
	}
}

// useWireGuard switches MiniScale to a WireGuard network, from the
// Tailscale screens.
func (a *app) useWireGuard() bool {
	if !a.scr.Confirm(a.input, "Use WireGuard?", "MiniScale leaves your tailnet and connects to a plain WireGuard server instead, through the same proxies, so your apps keep working. Your Tailscale login stays saved.", "Switch") {
		return false
	}
	if err := a.busy("Switching…", func() error { return a.script("network", netWireGuard) }); err != nil {
		a.scr.Notice(a.input, "Couldn't switch", err.Error()+tryAgain)
		return false
	}
	return true
}
