// miniscale-badge draws MiniScale's status icon next to the theme's Wi-Fi
// icon in the Onion menu's top bar: white when connected to the tailnet,
// grey while connecting, orange when it needs a login. It runs while
// MiniScale is on and draws only over the menu (not in games or apps).
//
// It stays small on purpose (it runs all the time next to games): it uses
// the fb package rather than the full UI with its fonts, and speaks just
// enough HTTP to read tailscaled's state.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"image"
	"image/color"
	"io"
	"log"
	"math"
	"net"
	"os"
	"runtime/debug"
	"time"

	"github.com/zakijss/minideck/fb"
	"github.com/zakijss/minideck/procs"
	"github.com/zakijss/minideck/version"
)

var (
	white  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	grey   = color.RGBA{0x8e, 0x8e, 0x93, 0xff}
	orange = color.RGBA{0xff, 0x9f, 0x0a, 0xff}
)

func main() {
	version.Flag("miniscale-badge")
	version.Note("miniscale-badge")
	sock := flag.String("socket", "/tmp/miniscale/tailscaled.sock", "tailscaled's socket")
	x := flag.Int("x", 474, "left edge of the icon (screen pixels); the default suits themes with Wi-Fi at about x=507")
	y := flag.Int("y", 18, "top edge of the icon")
	size := flag.Int("size", 26, "icon size")
	flag.Parse()
	debug.SetGCPercent(20) // tiny heap: collect early, stay small

	p, err := fb.OpenPatch(image.Rect(*x, *y, *x+*size, *y+*size))
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	state, drawn := "", ""
	var base color.RGBA
	menuPid := 0
	shown := false
	var guard <-chan time.Time
	var guardT *time.Ticker
	poll := time.NewTicker(time.Second)
	n := 0
	for {
		select {
		case <-poll.C:
			n++
			if n%3 == 1 {
				state = backend(*sock)
			}
			if menuPid == 0 || !procs.Exists(menuPid) {
				menuPid = procs.Pid("MainUI")
			}
			show := menuPid > 0 && !procs.GameRunning() && state != ""
			if show && !shown {
				// let the menu draw its top bar first
				time.Sleep(300 * time.Millisecond)
			}
			shown = show
			if show {
				// Draw on the top bar's colour, read from the top right corner
				// (the area under the icon may still hold the icon drawn before
				// a restart). Read it every time: at boot the menu may not have
				// drawn the bar yet, and themes can change.
				if c := p.ScreenPixel(fb.W-4, 3); c != base || state != drawn {
					base, drawn = c, state
					p.SnapshotColor(c)
					p.Set(drawIcon(p.Background(), state), 1)
				}
				if guardT == nil {
					guardT = time.NewTicker(8 * time.Millisecond)
					guard = guardT.C
				}
			} else {
				drawn = ""
				if guardT != nil {
					guardT.Stop()
					guardT, guard = nil, nil
				}
			}
		case <-guard:
			if !procs.Exists(menuPid) {
				continue
			}
			p.Guard()
		}
	}
}

// backend returns tailscaled's state ("Running", "Starting", "NeedsLogin"…),
// or "" when it isn't running.
func backend(sock string) string {
	starting := func() string {
		if _, err := os.Stat("/tmp/miniscale/tailscaled.pid"); err == nil {
			return "Starting"
		}
		return ""
	}
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return starting()
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "GET /localapi/v0/status?peers=false HTTP/1.0\r\nHost: local-tailscaled.sock\r\nSec-Tailscale: localapi\r\n\r\n")
	r := bufio.NewReader(c)
	for { // skip the status line and headers
		line, err := r.ReadString('\n')
		if err != nil {
			return "Starting"
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	var st struct{ BackendState string }
	if json.NewDecoder(r).Decode(&st) != nil || st.BackendState == "" {
		return "Starting"
	}
	return st.BackendState
}

// drawIcon draws three connected nodes (the MiniScale mark) on bg.
func drawIcon(bg *image.RGBA, state string) *image.RGBA {
	w := float64(bg.Bounds().Dx())
	col := grey
	switch state {
	case "Running":
		col = white
	case "NeedsLogin", "NeedsMachineAuth", "Stopped":
		col = orange
	}
	type pt struct{ x, y float64 }
	nodes := []pt{{w / 2, w * 0.24}, {w * 0.22, w * 0.74}, {w * 0.78, w * 0.74}}
	lw, r := w*0.075, w*0.14
	for py := 0; py < bg.Bounds().Dy(); py++ {
		for px := 0; px < bg.Bounds().Dx(); px++ {
			x, y := float64(px)+0.5, float64(py)+0.5
			cover := 0.0
			for i, a := range nodes {
				b := nodes[(i+1)%3]
				cover = math.Max(cover, lw/2-segDist(x, y, a.x, a.y, b.x, b.y)+0.5)
				cover = math.Max(cover, r-math.Hypot(x-a.x, y-a.y)+0.5)
			}
			blend(bg, px, py, col, math.Min(1, cover))
		}
	}
	return bg
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((px-ax)*dx+(py-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(px-(ax+t*dx), py-(ay+t*dy))
}

func blend(img *image.RGBA, x, y int, c color.RGBA, a float64) {
	if a <= 0 {
		return
	}
	i := img.PixOffset(x, y)
	p := img.Pix[i : i+4 : i+4]
	p[0] = uint8(float64(p[0]) + (float64(c.R)-float64(p[0]))*a)
	p[1] = uint8(float64(p[1]) + (float64(c.G)-float64(p[1]))*a)
	p[2] = uint8(float64(p[2]) + (float64(c.B)-float64(p[2]))*a)
	p[3] = 0xff
}
