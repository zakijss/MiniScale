package main

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/zakijss/minideck/qr"
	"github.com/zakijss/minideck/ui"
)

// TestScreens draws the app's screens to DIR/miniscale-*.png for review.
// It runs only with MINIDECK_GALLERY=DIR set.
func TestScreens(t *testing.T) {
	dir := os.Getenv("MINIDECK_GALLERY")
	if dir == "" {
		t.Skip("set MINIDECK_GALLERY=DIR to draw the screens")
	}
	os.MkdirAll(dir, 0o755)
	f := startFake(t)
	api := newLocalAPI(f.sock)
	appDirTmp := t.TempDir()
	os.WriteFile(filepath.Join(appDirTmp, "autostart"), nil, 0o644)
	newApp := func(name string) *app {
		t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, "miniscale-"+name+".png"))
		scr, err := ui.Open()
		if err != nil {
			t.Fatal(err)
		}
		a := &app{scr: scr, api: api, dir: appDirTmp, sock: f.sock}
		a.snap = a.fetch()
		return a
	}
	for i, n := range []string{"devices", "exit", "settings", "account"} {
		a := newApp("" + n)
		a.tab = i
		a.draw()
	}
	a := newApp("off")
	a.snap = snapshot{}
	a.draw()
	a = newApp("notanswering")
	a.snap = snapshot{err: errors.New("Get \"http://local-tailscaled.sock/localapi/v0/status\": context deadline exceeded")}
	a.draw()
	a = newApp("taildrop-note")
	a.note = "2 files waiting: Settings › Taildrop"
	a.draw()
	a = newApp("loggedout")
	a.snap.st.BackendState = "NeedsLogin"
	a.draw()
	a = newApp("relogin-notice")
	done := make(chan ui.Event)
	close(done)
	a.scr.Notice(done, "Log in again", noStateNotice)
	a = newApp("login")
	code, _ := qr.Encode("https://login.tailscale.com/a/1a2b3c4d5e6f7a")
	a.drawLogin("https://login.tailscale.com/a/1a2b3c4d5e6f7a", code, true)
	a = newApp("peer")
	st, _ := api.Status(context.Background())
	var l ui.List
	l.Items = peerItems(st.Peer["k1"], "23 ms, direct")
	top := a.scr.StaticFrame("nas", "100.101.1.2 · online", ui.Hint{Button: "X", Label: "Ping"}, ui.Hint{Button: "B", Label: "Back"})
	l.Draw(a.scr, imageRect(top))
	a.scr.Present()
}

func imageRect(top int) image.Rectangle { return image.Rect(20, top, ui.W-26, ui.H-58) }
