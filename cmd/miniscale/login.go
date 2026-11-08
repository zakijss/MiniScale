package main

import (
	"bufio"
	"context"
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zakijss/minideck/qr"
	"github.com/zakijss/minideck/ui"
)

// login shows the login link as a QR code until the device is logged in.
// With reauth, the device is logged in already and re-authenticates (the
// same as tailscale up --force-reauth); otherwise it logs in, or adds the
// account just created with NewProfile. It reports whether the login
// finished (false when it was cancelled, timed out or couldn't start).
func (a *app) login(reauth bool) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !reauth {
		// Join as soon as the login completes, named and set up as in
		// miniscale.conf the first time.
		change := map[string]any{"WantRunning": true}
		if p := a.snap.prefs; p == nil || p.Hostname == "" {
			name, ssh := confDefaults(a.dir)
			change["Hostname"], change["RunSSH"] = name, ssh
		}
		if _, err := a.api.EditPrefs(ctx, change); err != nil {
			a.scr.Notice(a.input, "Couldn't start the login", err.Error()+tryAgain)
			return false
		}
	}
	if err := a.api.LoginInteractive(ctx); err != nil {
		a.scr.Notice(a.input, "Couldn't start the login", err.Error()+tryAgain)
		return false
	}

	// Watch the status; the link comes from Tailscale's servers a moment
	// after the request.
	type update struct {
		st  *Status
		err error
	}
	ups := make(chan update, 1)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			st, err := a.api.Status(ctx)
			select {
			case ups <- update{st, err}:
			case <-ctx.Done():
				return
			}
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()

	var link string
	var code *qr.QR
	sawLink := false
	deadline := time.After(10 * time.Minute)
	for {
		a.drawLogin(link, code, reauth)
		select {
		case ev, ok := <-a.input:
			if !ok {
				return false
			}
			if ev.Pressed && (ev.Button == ui.B || ev.Button == ui.Menu) {
				a.refresh()
				a.note = "Login cancelled"
				return false
			}
		case <-deadline:
			a.refresh()
			a.note = "The login link expired: try again"
			return false
		case u := <-ups:
			if u.err != nil || u.st == nil {
				continue
			}
			st := u.st
			if st.AuthURL != "" && st.AuthURL != link {
				link = st.AuthURL
				code, _ = qr.Encode(link)
				sawLink = true
			}
			done := false
			switch st.BackendState {
			case "Running", "Stopped":
				// a first login is done once it runs; a re-login once the
				// link it showed has been used
				done = !reauth || (sawLink && st.AuthURL == "")
			case "NeedsMachineAuth":
				done = true
			}
			if done {
				a.saveNow()
				a.setBoot(true)
				a.refresh()
				if st.BackendState == "NeedsMachineAuth" {
					a.note = "Logged in: waiting for an admin to approve this device"
				} else if a.snap.cur != nil {
					a.note = "Logged in as " + a.snap.cur.Login()
				} else {
					a.note = "Logged in"
				}
				return true
			}
		}
	}
}

func (a *app) drawLogin(link string, code *qr.QR, reauth bool) {
	s := a.scr
	title := "Log in"
	if reauth {
		title = "Log in again"
	}
	top := s.StaticFrame(title, "Scan with your phone's camera", ui.Hint{Button: "B", Label: "Cancel"})
	area := image.Rect(20, top, ui.W-20, ui.H-62)
	if code == nil {
		s.Loading(area.Min.Y+120, "Asking Tailscale for a login link…")
		s.Present()
		return
	}
	// The code on white with a four-module quiet zone, as large as fits
	// beside the instructions, in whole pixels per module.
	n := code.Size + 8
	px := min(area.Dy(), 300) / n
	side := n * px
	qx, qy := area.Min.X+8, area.Min.Y+(area.Dy()-side)/2
	s.Fill(image.Rect(qx-4, qy-4, qx+side+4, qy+side+4), ui.Ink)
	s.Fill(image.Rect(qx, qy, qx+side, qy+side), ui.Text)
	for y := range code.Size {
		for x := range code.Size {
			if code.Modules[y][x] {
				mx, my := qx+(x+4)*px, qy+(y+4)*px
				s.Fill(image.Rect(mx, my, mx+px, my+px), ui.Ink)
			}
		}
	}
	// instructions on the right
	tx := qx + side + 22
	tw := area.Max.X - tx
	y := qy + 4
	for _, l := range ui.Wrap(ui.Body, "Point your phone's camera at the code and log in to Tailscale.", tw) {
		s.Text(tx, y, l, ui.Body, ui.Text)
		y += ui.LineHeight(ui.Body)
	}
	y += 10
	for _, l := range ui.Wrap(ui.Small, "Or open this link on any device:", tw) {
		s.Text(tx, y, l, ui.Small, ui.Muted)
		y += ui.LineHeight(ui.Small)
	}
	for _, l := range splitLink(link, tw) {
		s.Text(tx, y, l, ui.Small, ui.Accent)
		y += ui.LineHeight(ui.Small)
	}
	y += 12
	dots := strings.Repeat(".", int(time.Now().Unix()%3)+1)
	s.Text(tx, y, "Waiting for you"+dots, ui.Small, ui.Muted)
	s.Present()
}

// splitLink breaks a URL into lines that fit w, after a slash where it can.
func splitLink(link string, w int) []string {
	var out []string
	link = strings.TrimPrefix(link, "https://")
	for link != "" {
		n := len(link)
		for n > 1 && ui.Measure(ui.Small, link[:n]) > w {
			n--
		}
		if n < len(link) {
			if i := strings.LastIndexByte(link[:n], '/'); i > 0 {
				n = i + 1
			}
		}
		out = append(out, link[:n])
		link = link[n:]
	}
	return out
}

// confDefaults reads the device name and the Tailscale SSH setting from
// miniscale.conf (shell syntax: TS_HOSTNAME=miyoo, TS_SSH=true).
func confDefaults(dir string) (name string, ssh bool) {
	name, ssh = "miyoo", true
	f, err := os.Open(filepath.Join(dir, "miniscale.conf"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "TS_HOSTNAME":
			if checkHostname(v) == nil {
				name = v
			}
		case "TS_SSH":
			ssh = v == "true"
		}
	}
	return
}
