// miniscale-badge draws MiniScale's status icon next to the theme's Wi-Fi
// icon in the Onion menu's top bar: white when connected to the tailnet,
// grey while connecting, orange when it needs a login. It runs while
// MiniScale is on and draws only over the menu (not in games or apps).
//
// It stays small on purpose (it runs all the time next to games): it uses
// the fb package rather than the full UI with its fonts, and speaks just
// enough HTTP to read tailscaled's state.
//
// The menu paints over the icon when a button is pressed and, while idle,
// about once a second (its clock and battery). The icon has to be put back
// within a few milliseconds or it visibly blinks, but checking every 8 ms
// all the time cost about 3% of a core with the menu just sitting there. So
// it is checked every 8 ms only for a moment after a press and just before
// the next once-a-second repaint is due, and every 100 ms otherwise.
package main

import (
	"bufio"
	"encoding/binary"
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

const (
	fastGuard = 8 * time.Millisecond    // check rate while the menu is likely to repaint
	slowGuard = 100 * time.Millisecond  // check rate the rest of the time
	hot       = 1500 * time.Millisecond // fast checks after a press
	// The menu's idle repaint comes about a second after the last one
	// (measured: every 1.015 s). Checks go fast from early before it is due
	// until it has been seen, or until late after (a theme without a clock
	// may not repaint at all; then they stay slow until the next one).
	tickEvery = time.Second
	early     = 120 * time.Millisecond
	late      = 300 * time.Millisecond
	// A repaint this soon after a press is the press's own, not the
	// once-a-second one, so it doesn't move the anchor.
	pressEcho = 250 * time.Millisecond
)

// guardEvery returns how often to check the icon at now. The menu may
// repaint because of a press until hotUntil, and repainted on its own at
// anchor (zero: not seen yet).
func guardEvery(now, hotUntil, anchor time.Time) time.Duration {
	if now.Before(hotUntil) {
		return fastGuard
	}
	if !anchor.IsZero() {
		if d := now.Sub(anchor); d >= tickEvery-early && d < tickEvery+late {
			return fastGuard
		}
	}
	return slowGuard
}

// buttons signals (without blocking) each time a button goes down or up.
// It reads the device in a goroutine that sleeps in read(), so it costs
// nothing while no one touches the buttons. Without the device the channel
// never delivers and ok is false.
func buttons() (ch <-chan struct{}, ok bool) {
	c := make(chan struct{}, 1)
	f, err := os.Open("/dev/input/event0")
	if err != nil {
		return c, false
	}
	go func() {
		defer f.Close()
		// struct input_event on 32-bit ARM: timeval (2 x int32), type u16,
		// code u16, value s32 = 16 bytes.
		buf := make([]byte, 16)
		for {
			if _, err := io.ReadFull(f, buf); err != nil {
				return
			}
			if binary.LittleEndian.Uint16(buf[8:]) != 1 { // EV_KEY
				continue
			}
			select {
			case c <- struct{}{}:
			default:
			}
		}
	}()
	return c, true
}

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
	trace := os.Getenv("MINISCALE_BADGE_TRACE") != ""

	p, err := fb.OpenPatch(image.Rect(*x, *y, *x+*size, *y+*size))
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	// A pixel in the middle of the icon's top node: always the icon's own
	// colour, so anything else there means the menu has drawn over it.
	probeX, probeY := *size/2, *size/4
	var probeC color.RGBA

	state, drawn := "", ""
	var base color.RGBA
	menuPid := 0
	shown := false

	press, haveButtons := buttons()
	var hotUntil, anchor, lastPress time.Time
	if !haveButtons {
		// can't tell when a press repaints the menu: check often all the time
		hotUntil = time.Now().Add(100 * 365 * 24 * time.Hour)
	}

	var guard <-chan time.Time
	var guardT *time.Ticker
	var every time.Duration
	// setGuard starts the checks, or changes their rate, to suit now.
	setGuard := func() {
		want := guardEvery(time.Now(), hotUntil, anchor)
		if guardT == nil {
			guardT = time.NewTicker(want)
			guard = guardT.C
		} else if want != every {
			guardT.Reset(want)
		}
		every = want
	}
	stopGuard := func() {
		if guardT != nil {
			guardT.Stop()
			guardT, guard, every = nil, nil, 0
		}
	}

	poll := time.NewTicker(time.Second)
	n := 0
	for {
		select {
		case <-press:
			lastPress = time.Now()
			if haveButtons {
				hotUntil = lastPress.Add(hot)
			}
			if guardT != nil {
				setGuard()
			}
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
				if haveButtons {
					hotUntil = time.Now().Add(hot)
				}
				anchor = time.Time{}
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
					img := drawIcon(p.Background(), state)
					p.Set(img, 1)
					probeC = img.RGBAAt(probeX, probeY)
				}
				setGuard()
			} else {
				drawn = ""
				stopGuard()
			}
		case <-guard:
			if !procs.Exists(menuPid) {
				continue
			}
			now := time.Now()
			if p.ScreenPixel(*x+probeX, *y+probeY) != probeC {
				if now.Sub(lastPress) > pressEcho {
					anchor = now
				}
				if trace {
					since := "no press yet"
					if !lastPress.IsZero() {
						since = now.Sub(lastPress).Round(time.Millisecond).String() + " after a press"
					}
					log.Printf("trace: painted over (checking every %v, %s)", every, since)
				}
			}
			p.Guard()
			setGuard()
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
