package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zakijss/minideck/ui"
)

// peerDetails shows one device, updating as the status does; X pings it.
func (a *app) peerDetails(id string) {
	s := a.scr
	var l ui.List
	var ping string
	pings := make(chan string, 1)
	pinging := false
	for {
		p := a.peer(id)
		if p == nil {
			return // it left the tailnet
		}
		l.Items = peerItems(p, ping)
		if l.Sel >= len(l.Items) {
			l.Sel = len(l.Items) - 1
		}
		sub := p.IPv4()
		if p.Online {
			sub += " · online"
		} else {
			sub += " · offline"
		}
		hints := []ui.Hint{{Button: "X", Label: "Ping"}, {Button: "B", Label: "Back"}}
		if p.ExitNodeOption {
			label := "Use as exit node"
			if p.ExitNode {
				label = "Stop using"
			}
			hints = append([]ui.Hint{{Button: "Y", Label: label}}, hints...)
		}
		top := s.StaticFrame(p.Name(), sub, hints...)
		l.Draw(s, image.Rect(20, top, ui.W-26, ui.H-58))
		s.Present()
		select {
		case ev, ok := <-a.input:
			if !ok {
				return
			}
			if !ev.Pressed {
				continue
			}
			switch ev.Button {
			case ui.B, ui.Menu:
				return
			case ui.X:
				if !pinging && !ev.Repeat {
					pinging, ping = true, "…"
					ip := p.IPv4()
					go func() { pings <- a.pingText(ip) }()
				}
			case ui.Y:
				if p.ExitNodeOption && !ev.Repeat {
					if p.ExitNode {
						a.useExitNode("")
					} else {
						a.useExitNode(p.ID)
					}
				}
			default:
				l.Handle(ev)
			}
		case ping = <-pings:
			pinging = false
			a.refresh()
		case snap := <-a.snaps:
			a.take(snap)
		}
	}
}

// pingText pings three times and sums it up: "23 ms, direct" or the
// error.
func (a *app) pingText(ip string) string {
	var best *PingResult
	var lastErr error
	for range 3 {
		r, err := a.api.Ping(context.Background(), ip)
		if err != nil {
			lastErr = err
			continue
		}
		if best == nil || r.LatencySeconds < best.LatencySeconds {
			best = r
		}
	}
	if best == nil {
		return "no answer: " + lastErr.Error()
	}
	ms := best.LatencySeconds * 1000
	how := "direct"
	switch {
	case best.Endpoint == "" && best.DERPRegionCode != "":
		how = "relayed via " + strings.ToUpper(best.DERPRegionCode)
	case best.PeerRelay != "":
		how = "via a peer relay"
	case best.IsLocalIP:
		how = "direct, same network"
	}
	return fmt.Sprintf("%.0f ms, %s", ms, how)
}

func peerItems(p *Peer, ping string) []ui.Item {
	conn := "Idle"
	switch {
	case p.CurAddr != "":
		conn = "Direct: " + p.CurAddr
	case p.Relay != "" && p.Active:
		conn = "Relayed via " + strings.ToUpper(p.Relay)
	case p.Relay != "":
		conn = "Idle (home relay " + strings.ToUpper(p.Relay) + ")"
	}
	seen := "now"
	if !p.Online {
		seen = ago(p.LastSeen)
	}
	items := []ui.Item{
		{Label: "Tailscale addresses", Sub: strings.Join(p.TailscaleIPs, " · ")},
		{Label: "Name", Sub: strings.TrimSuffix(p.DNSName, ".")},
		{Label: "System", Sub: osName(p.OS), Detail: "seen " + seen},
		{Label: "Connection", Sub: conn},
	}
	if ping != "" {
		items = append(items, ui.Item{Label: "Ping", Sub: ping})
	}
	if p.RxBytes > 0 || p.TxBytes > 0 {
		items = append(items, ui.Item{Label: "Data", Sub: "received " + size(p.RxBytes) + " · sent " + size(p.TxBytes)})
	}
	if len(p.PrimaryRoutes) > 0 {
		items = append(items, ui.Item{Label: "Shares subnets", Sub: strings.Join(p.PrimaryRoutes, ", ")})
	}
	if p.ExitNodeOption {
		d := "offered"
		if p.ExitNode {
			d = "in use"
		}
		items = append(items, ui.Item{Label: "Exit node", Detail: d})
	}
	if p.KeyExpiry != nil {
		items = append(items, ui.Item{Label: "Key expiry", Sub: p.KeyExpiry.Local().Format("Jan 2, 2006")})
	}
	if len(p.Tags) > 0 {
		items = append(items, ui.Item{Label: "Tags", Sub: strings.Join(p.Tags, ", ")})
	}
	return items
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---- Taildrop ----

// taildropDir is where received files are saved.
func taildropDir() string {
	if d := os.Getenv("MINIDECK_TAILDROP"); d != "" {
		return d
	}
	return "/mnt/SDCARD/Media/Taildrop"
}

// taildrop lists the files waiting and saves them to Media/Taildrop.
func (a *app) taildrop() {
	s := a.scr
	var l ui.List
	note := ""
	for {
		files := a.snap.files
		l.Items = l.Items[:0]
		for _, f := range files {
			l.Items = append(l.Items, ui.Item{Label: f.Name, Detail: size(f.Size), Icon: "disc"})
		}
		if l.Sel >= len(l.Items) {
			l.Sel = max(0, len(l.Items)-1)
		}
		sub := note
		if sub == "" {
			sub = "Saved to " + strings.TrimPrefix(taildropDir(), "/mnt/SDCARD/")
		}
		if len(files) == 0 {
			top := s.StaticFrame("Taildrop", sub, ui.Hint{Button: "B", Label: "Back"})
			a.card(top, "No files waiting", "Send files to this Miyoo from Tailscale on your phone or computer (Share, then Tailscale, then this device). They show up here, and A saves them to Media/Taildrop on the card.")
		} else {
			top := s.StaticFrame("Taildrop", sub, ui.Hint{Button: "A", Label: "Save all"},
				ui.Hint{Button: "Y", Label: "Delete"}, ui.Hint{Button: "B", Label: "Back"})
			l.Draw(s, image.Rect(20, top, ui.W-26, ui.H-58))
		}
		s.Present()
		select {
		case ev, ok := <-a.input:
			if !ok {
				return
			}
			if !ev.Pressed {
				continue
			}
			note = ""
			switch ev.Button {
			case ui.B, ui.Menu:
				return
			case ui.A:
				if len(files) > 0 && !ev.Repeat {
					var n int
					err := a.busy("Saving "+plural(len(files), "file")+"…", func() (err error) {
						n, err = a.saveFiles(files)
						return err
					})
					a.refresh()
					note = "Saved " + plural(n, "file")
					if err != nil {
						s.Notice(a.input, "Not all files were saved", err.Error())
					}
				}
			case ui.Y:
				if it := l.Selected(); it != nil && !ev.Repeat &&
					s.Confirm(a.input, "Delete this file?", it.Label+" is removed without being saved.", "Delete") {
					if err := a.api.DeleteFile(context.Background(), it.Label); err != nil {
						s.Notice(a.input, "Couldn't delete it", err.Error())
					}
					a.refresh()
				}
			default:
				l.Handle(ev)
			}
		case snap := <-a.snaps:
			a.take(snap)
		}
	}
}

// saveFiles copies each waiting file to the Taildrop folder (as "name
// (2).ext" if the name is taken) and removes it from the inbox. A file that
// can't be saved stays in the inbox and the others are still saved; the
// error names the ones that failed.
func (a *app) saveFiles(files []WaitingFile) (int, error) {
	dir := taildropDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	var failed []string
	for _, f := range files {
		if err := a.saveFile(dir, f.Name); err != nil {
			failed = append(failed, f.Name+": "+err.Error())
			continue
		}
		n++
	}
	if len(failed) == 0 {
		return n, nil
	}
	const shown = 4 // as many as the notice has room for
	if len(failed) > shown {
		failed = append(failed[:shown], fmt.Sprintf("and %d more", len(failed)-shown))
	}
	return n, errors.New(strings.Join(failed, "\n"))
}

func (a *app) saveFile(dir, name string) error {
	base := filepath.Base(name)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return fmt.Errorf("unusable file name %q", name)
	}
	base = fatName(base)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	rc, err := a.api.OpenFile(ctx, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	dst := freeName(dir, base)
	// a unique temporary name: dst+".part" could be a file saved earlier
	out, err := os.CreateTemp(dir, ".taildrop-*.part")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return a.api.DeleteFile(ctx, name)
}

// fatName makes a sender's file name one the card's FAT file system takes.
// FAT refuses \ / : * ? " < > | and control characters, which become "_",
// and drops dots and spaces at the end of a name, which go here too so the
// saved name is the one shown.
func fatName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError || strings.ContainsRune(`\/:*?"<>|`, r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	s := strings.TrimRight(b.String(), ". ")
	if s == "" {
		return "file"
	}
	return s
}

// freeName is dir/name, or dir/"name (2).ext" and so on if taken.
func freeName(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
}
