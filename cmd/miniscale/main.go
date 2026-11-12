// MiniScale is the Tailscale client's screen on the Miyoo: turn it on and
// off, see your devices and ping them, pick an exit node, change settings
// (DNS, subnet routes, Tailscale SSH, blocking incoming connections, the
// device name, starting at boot), receive files with Taildrop, and log in,
// log in again, log out or switch between accounts with a QR code.
//
// tailscaled itself is started and stopped by miniscale.sh, which keeps its
// state encrypted on the card; everything else goes through tailscaled's
// local API, like the tailscale command.
package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zakijss/minideck/appdir"
	"github.com/zakijss/minideck/ui"
	"github.com/zakijss/minideck/version"
)

// runDir is where miniscale.sh keeps tailscaled's socket and pid file.
func runDir() string {
	if d := os.Getenv("MINIDECK_RUN"); d != "" {
		return d
	}
	return "/tmp/miniscale"
}

func main() {
	version.Flag("miniscale")
	version.Note("miniscale")
	scr, err := ui.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer scr.Close()
	a := &app{scr: scr, input: ui.Input(), dir: appdir.Dir(), sock: filepath.Join(runDir(), "tailscaled.sock"),
		snaps: make(chan snapshot, 1), kick: make(chan struct{}, 1)}
	a.api = newLocalAPI(a.sock)
	polling := false
	for {
		if networkOf(a.dir) == netWireGuard {
			if !a.runWireGuard() {
				return
			}
			continue
		}
		a.snap = a.fetchGen()
		if !polling {
			go a.poll()
			polling = true
		}
		a.switching = false
		a.run()
		if !a.switching {
			return
		}
	}
}

// snapshot is everything the screens show, read in one go.
type snapshot struct {
	on       bool // tailscaled answers
	st       *Status
	prefs    *Prefs
	profiles []Profile
	cur      *Profile
	files    []WaitingFile
	boot     bool // starts at boot
	err      error
	gen      uint64 // app.gen when the reading began
}

type app struct {
	scr       *ui.Screen
	input     <-chan ui.Event
	loginHost string // the server the login screen names
	dir       string
	sock      string
	api       *localAPI
	snap      snapshot
	snaps     chan snapshot
	kick      chan struct{} // ask the poller for a fresh snapshot now
	// gen moves on with every refresh, that is after every action, so a
	// snapshot the poller began reading before an action is known to be
	// out of date (see take).
	gen atomic.Uint64

	tab   int
	lists [4]ui.List
	selID [4]string // keeps the same row selected as lists change
	note  string    // replaces the subtitle until the next press
	// switching is set when the network changed to WireGuard: run returns
	// and main shows the WireGuard screen
	switching bool
}

const (
	tabDevices = iota
	tabExit
	tabSettings
	tabAccount
)

var tabNames = [...]string{"Devices", "Exit node", "Settings", "Account"}

func (a *app) fetch() snapshot {
	var s snapshot
	_, err := os.Stat(filepath.Join(a.dir, "autostart"))
	s.boot = err == nil
	if _, err := os.Stat(a.sock); err != nil {
		return s
	}
	ctx := context.Background()
	st, err := a.api.Status(ctx)
	if err != nil {
		s.err = err
		return s
	}
	s.on, s.st = true, st
	s.prefs, _ = a.api.Prefs(ctx)
	s.profiles, _ = a.api.Profiles(ctx)
	s.cur, _ = a.api.CurrentProfile(ctx)
	if st.BackendState == "Running" {
		s.files, _ = a.api.WaitingFiles(ctx)
	}
	return s
}

// fetchGen is fetch, stamped with the generation it began in.
func (a *app) fetchGen() snapshot {
	g := a.gen.Load()
	s := a.fetch()
	s.gen = g
	return s
}

// poll reads a snapshot every two seconds, or at once when kicked.
func (a *app) poll() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-a.kick:
		}
		s := a.fetchGen()
		select {
		case <-a.snaps: // drop an unread older one
		default:
		}
		a.snaps <- s
	}
}

// refresh reads the state at once, after an action changed it.
func (a *app) refresh() {
	a.gen.Add(1)
	a.snap = a.fetchGen()
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// take shows a snapshot from the poller, unless its reading began before
// the last refresh: it may show things as they were before an action (a
// setting flipping back until the next poll, and pressing A again sending
// the same value).
func (a *app) take(s snapshot) {
	if s.gen < a.gen.Load() {
		return
	}
	// Files arriving are easy to miss in Settings: say so until the next
	// press.
	if n := len(s.files); n > len(a.snap.files) {
		a.note = plural(n, "file") + " waiting: Settings › Taildrop"
	}
	a.snap = s
}

func (a *app) run() {
	for {
		a.draw()
		select {
		case ev, ok := <-a.input:
			if !ok {
				return
			}
			if !ev.Pressed {
				continue
			}
			a.note = ""
			if !a.key(ev) || a.switching {
				return
			}
		case s := <-a.snaps:
			a.take(s)
		}
	}
}

// state sums up where things stand.
type state int

const (
	stOff        state = iota // tailscaled isn't running
	stStarting                // it is, but not ready
	stNeedsLogin              // logged out, or never logged in
	stNeedsAuth               // waiting for an admin to approve the device
	stStopped                 // logged in, disconnected
	stRunning
)

func (s snapshot) state() state {
	if !s.on || s.st == nil {
		return stOff
	}
	switch s.st.BackendState {
	case "Running":
		return stRunning
	case "Stopped":
		return stStopped
	case "NeedsLogin":
		return stNeedsLogin
	case "NeedsMachineAuth":
		return stNeedsAuth
	}
	if s.prefs != nil && s.prefs.LoggedOut {
		return stNeedsLogin
	}
	return stStarting
}

// ---- keys ----

func (a *app) key(ev ui.Event) bool {
	st := a.snap.state()
	switch ev.Button {
	case ui.B, ui.Menu:
		return false
	case ui.Select:
		if !ev.Repeat {
			a.scr.Appearance(a.input)
		}
		return true
	}
	if st == stOff {
		switch {
		case ev.Repeat:
		case ev.Button == ui.A:
			a.turnOn()
		case ev.Button == ui.Y:
			a.switching = a.useWireGuard()
		}
		return true
	}
	if st == stNeedsLogin || st == stNeedsAuth || st == stStarting {
		switch ev.Button {
		case ui.A:
			if !ev.Repeat && st != stStarting {
				if control, ok := a.pickServer(); ok {
					a.login(false, control)
				}
			}
		case ui.Y:
			if !ev.Repeat {
				a.accountsWhileLoggedOut()
			}
		case ui.X:
			if !ev.Repeat {
				a.turnOff()
			}
		}
		return true
	}
	switch ev.Button {
	case ui.L1:
		a.tab = (a.tab + len(tabNames) - 1) % len(tabNames)
	case ui.R1:
		a.tab = (a.tab + 1) % len(tabNames)
	case ui.X:
		if !ev.Repeat {
			a.setPref("WantRunning", st != stRunning, "")
		}
	case ui.A:
		if !ev.Repeat {
			a.activate()
		}
	case ui.Y:
		if !ev.Repeat && a.tab == tabAccount {
			a.removeProfile()
		}
	default:
		l := &a.lists[a.tab]
		switch ev.Button {
		case ui.L2:
			l.Sel = 0
		case ui.R2:
			l.Sel = max(0, len(l.Items)-1)
		default:
			l.Handle(ev)
		}
		if it := l.Selected(); it != nil {
			a.selID[a.tab] = rowID(it)
		}
	}
	return true
}

func rowID(it *ui.Item) string {
	if r, ok := it.Value.(row); ok {
		return r.kind + "\x00" + r.id
	}
	return it.Label
}

// row is what a list row does when chosen.
type row struct {
	kind string // peer, exit, pref, name, boot, files, profile, add, relogin, logout, off, appearance
	id   string
}

func (a *app) activate() {
	it := a.lists[a.tab].Selected()
	if it == nil {
		return
	}
	r, _ := it.Value.(row)
	p := a.snap.prefs
	switch r.kind {
	case "peer":
		if pe := a.peer(r.id); pe != nil {
			a.peerDetails(pe.ID)
		}
	case "exit":
		a.useExitNode(r.id)
	case "pref":
		if p == nil {
			return
		}
		cur := map[string]bool{"CorpDNS": p.CorpDNS, "RouteAll": p.RouteAll, "RunSSH": p.RunSSH,
			"ShieldsUp": p.ShieldsUp, "ExitNodeAllowLANAccess": p.ExitNodeAllowLANAccess}[r.id]
		a.setPref(r.id, !cur, "")
	case "name":
		a.rename()
	case "boot":
		a.setBoot(!a.snap.boot)
		a.refresh()
	case "files":
		a.taildrop()
	case "profile":
		if a.snap.cur == nil || r.id != a.snap.cur.ID {
			a.switchProfile(r.id)
		}
	case "add":
		a.addAccount()
	case "relogin":
		a.login(true, "")
	case "logout":
		a.logout()
	case "off":
		a.turnOff()
	case "appearance":
		a.scr.Appearance(a.input)
	case "network":
		a.switching = a.useWireGuard()
	}
}

func (a *app) peer(id string) *Peer {
	if a.snap.st == nil {
		return nil
	}
	for _, p := range a.snap.st.Peer {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// ---- actions ----

// busy draws a waiting screen while f runs.
func (a *app) busy(what string, f func() error) error {
	s := a.scr
	top := s.StaticFrame("MiniScale", what)
	s.Loading(top+150, what)
	s.Present()
	return f()
}

// scriptError is miniscale.sh failing: the last line it printed, and its
// exit status.
type scriptError struct {
	msg  string
	code int
}

func (e *scriptError) Error() string { return e.msg }

// exitNoState is miniscale.sh start's exit status when this device can't
// open the saved state: the card was used in another Miyoo, or the device
// key in the internal flash is gone.
const exitNoState = 3

func isNoState(err error) bool {
	var se *scriptError
	return errors.As(err, &se) && se.code == exitNoState
}

// script runs miniscale.sh with args.
func (a *app) script(args ...string) error {
	cmd := exec.Command("sh", append([]string{filepath.Join(a.dir, "miniscale.sh")}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		if msg == "" {
			msg = err.Error()
		}
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return &scriptError{msg, code}
	}
	return nil
}

// saveState has miniscale.sh save the (encrypted) state on the card, after
// a change it should survive a power-off.
func (a *app) saveState() {
	go a.script("save")
}

// saveNow saves the state at once, after a login or a logout. A failed save
// is shown: the change would be undone at the next start, and a new login
// lost.
func (a *app) saveNow() {
	if err := a.script("save"); err != nil {
		a.scr.Notice(a.input, "State not saved", "MiniScale couldn't save its state on the SD card ("+err.Error()+"). After a restart, this device may need to log in again.\n\nminiscale.log in the app's folder has the details.")
	}
}

// setBoot turns starting at boot on or off.
func (a *app) setBoot(on bool) {
	arg := "off"
	if on {
		arg = "on"
	}
	if err := a.script("autostart", arg); err != nil {
		a.scr.Notice(a.input, "Couldn't change starting at boot", err.Error()+tryAgain)
	}
}

// tryAgain ends a notice whose error is raw, so it says what to do next.
const tryAgain = "\n\nTry again."

// noStateNotice explains why turning on leads to a new login.
const noStateNotice = "This Miyoo can't open its saved login: the SD card was used in another Miyoo, or this one's device key was reset. Log in again with a QR code to rejoin your tailnet.\n\nThe old login is kept in the app's folder as state.enc.unreadable."

func (a *app) turnOn() {
	_, err := os.Stat(filepath.Join(a.dir, "state.enc"))
	how := "start"
	if err != nil {
		how = "fresh" // never logged in: start empty, then log in
	}
	// tailscaled is running but not answering: start alone would find it
	// already running, so stop it first.
	restart := a.snap.err != nil
	start := func() error {
		if restart {
			a.script("stop")
		}
		e := a.script(how)
		a.refresh()
		return e
	}
	what := "Turning on…"
	if restart {
		what = "Restarting…"
	}
	err = a.busy(what, start)
	if how == "start" && isNoState(err) && a.snap.state() == stOff {
		// Starting would fail the same way every time: start afresh
		// instead (miniscale.sh keeps the old file aside) and log in.
		a.scr.Notice(a.input, "Log in again", noStateNotice)
		how = "fresh"
		err = a.busy("Turning on…", start)
	}
	switch a.snap.state() {
	case stRunning, stStopped:
		a.setBoot(true)
		a.refresh()
		a.note = "On: " + a.selfIP()
	case stNeedsLogin:
		if control, ok := a.pickServer(); ok {
			a.login(false, control)
		}
	case stOff:
		if err == nil {
			err = errors.New("tailscaled didn't start")
		}
		a.scr.Notice(a.input, "Couldn't turn on", err.Error()+"\n\nminiscale.log in the app's folder has the details.")
	}
}

func (a *app) turnOff() {
	if !a.scr.Confirm(a.input, "Turn MiniScale off?", "This device leaves your tailnet and won't join at boot until you turn it on again. Apps that use your tailnet stop working.", "Turn off") {
		return
	}
	var bootErr error
	err := a.busy("Turning off…", func() error {
		bootErr = a.script("autostart", "off")
		e := a.script("stop")
		a.refresh()
		return e
	})
	switch {
	case a.snap.state() != stOff:
		msg := "tailscaled is still running." + tryAgain
		if err != nil {
			msg = err.Error()
		}
		a.scr.Notice(a.input, "Couldn't turn off", msg)
		return
	case err != nil:
		// stop ends everything even when the save fails
		a.scr.Notice(a.input, "State not saved", "MiniScale is off, but it couldn't save its state on the SD card: a login or setting changed since the last save is lost.\n\nminiscale.log in the app's folder has the details.")
	case bootErr != nil:
		a.scr.Notice(a.input, "Couldn't change starting at boot", bootErr.Error()+tryAgain)
	}
	a.note = "Off"
}

func (a *app) setPref(name string, v any, what string) {
	if _, err := a.api.EditPrefs(context.Background(), map[string]any{name: v}); err != nil {
		a.scr.Notice(a.input, "Couldn't change that", err.Error()+tryAgain)
		return
	}
	a.saveState()
	a.refresh()
	if what != "" {
		a.note = what
	}
}

func (a *app) useExitNode(id string) {
	change := map[string]any{"ExitNodeID": id, "ExitNodeIP": ""}
	if _, err := a.api.EditPrefs(context.Background(), change); err != nil {
		a.scr.Notice(a.input, "Couldn't change the exit node", err.Error()+tryAgain)
		return
	}
	a.saveState()
	a.refresh()
	if id == "" {
		a.note = "No exit node"
	} else if p := a.peer(id); p != nil {
		a.note = "Exit node: " + p.Name()
	}
}

func (a *app) rename() {
	cur := ""
	if a.snap.prefs != nil {
		cur = a.snap.prefs.Hostname
	}
	if cur == "" && a.snap.st != nil && a.snap.st.Self != nil {
		cur = a.snap.st.Self.HostName
	}
	for {
		name, ok := a.scr.Prompt(a.input, "Device name", cur, "letters, digits and hyphens", false)
		if !ok {
			return
		}
		name = strings.TrimSpace(name)
		if err := checkHostname(name); err != nil {
			a.scr.Notice(a.input, "Can't use that name", err.Error())
			cur = name
			continue
		}
		a.setPref("Hostname", name, "Renamed to "+name)
		return
	}
}

// checkHostname explains what is wrong with n as a device name, or returns
// nil. The message is shown as is.
func checkHostname(n string) error {
	if n == "" || len(n) > 63 {
		return errors.New("a name is 1 to 63 characters long")
	}
	for i, c := range n {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
		if !ok {
			return fmt.Errorf("only letters, digits and hyphens: %q isn't one", c)
		}
		if c == '-' && (i == 0 || i == len(n)-1) {
			return errors.New("a name can't start or end with a hyphen")
		}
	}
	return nil
}

func (a *app) switchProfile(id string) {
	err := a.busy("Switching account…", func() error {
		e := a.api.SwitchProfile(context.Background(), id)
		time.Sleep(time.Second)
		a.refresh()
		return e
	})
	if err != nil {
		a.scr.Notice(a.input, "Couldn't switch", err.Error()+tryAgain)
		return
	}
	a.saveState()
	if a.snap.state() == stNeedsLogin {
		// a saved account logs in again with its own server
		control := ""
		if p := a.snap.prefs; p != nil && !isTailscale(p.ControlURL) {
			control = p.ControlURL
		}
		a.login(false, control)
		return
	}
	if a.snap.cur != nil {
		a.note = "Now using " + a.snap.cur.Login()
	}
}

// addAccount starts an empty profile and logs in to it. A login that
// doesn't finish goes back to the account in use before: left on the empty
// profile, the device would be logged out and off the tailnet.
func (a *app) addAccount() {
	prev := ""
	if a.snap.cur != nil {
		prev = a.snap.cur.ID
	}
	known := make(map[string]bool)
	for _, p := range a.snap.profiles {
		known[p.ID] = true
	}
	control, ok := a.pickServer()
	if !ok {
		return
	}
	if err := a.api.NewProfile(context.Background()); err != nil {
		a.scr.Notice(a.input, "Couldn't add an account", err.Error()+tryAgain)
		return
	}
	a.refresh()
	if a.login(false, control) {
		return
	}
	note := a.note
	err := a.busy("Switching back…", func() error {
		e := a.dropNewProfile(prev, known)
		time.Sleep(time.Second)
		a.refresh()
		return e
	})
	if err != nil {
		a.scr.Notice(a.input, "Couldn't switch back", err.Error()+"\n\nPress Y to pick one of your accounts.")
		return
	}
	a.saveState()
	if a.snap.cur != nil {
		note += ": back to " + a.snap.cur.Login()
	}
	a.note = note
}

// dropNewProfile switches back to the profile prev and deletes the empty
// profiles that aren't in known: what an unfinished login to a new account
// leaves behind.
func (a *app) dropNewProfile(prev string, known map[string]bool) error {
	if prev == "" {
		return errors.New("the account in use before isn't known")
	}
	ctx := context.Background()
	if err := a.api.SwitchProfile(ctx, prev); err != nil {
		return err
	}
	ps, err := a.api.Profiles(ctx)
	if err != nil {
		return nil // back on the old account; a leftover empty one does no harm
	}
	for _, p := range ps {
		if p.ID == "" || p.ID == prev || known[p.ID] || p.NodeID != "" || p.UserProfile.LoginName != "" {
			continue
		}
		if err := a.api.DeleteProfile(ctx, p.ID); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) logout() {
	who := "this account"
	if a.snap.cur != nil && a.snap.cur.Login() != "" {
		who = a.snap.cur.Login()
	}
	if !a.scr.Confirm(a.input, "Log out?", "This device leaves the tailnet of "+who+". You can log in again with a QR code.", "Log out") {
		return
	}
	err := a.busy("Logging out…", func() error {
		e := a.api.Logout(context.Background())
		a.refresh()
		return e
	})
	if err != nil {
		a.scr.Notice(a.input, "Couldn't log out", err.Error()+tryAgain)
		return
	}
	a.setBoot(false) // nothing to join at boot
	a.saveNow()
	a.note = "Logged out"
}

// removeProfile forgets another account (not the one in use).
func (a *app) removeProfile() {
	it := a.lists[tabAccount].Selected()
	if it == nil {
		return
	}
	r, _ := it.Value.(row)
	if r.kind != "profile" || (a.snap.cur != nil && r.id == a.snap.cur.ID) {
		return
	}
	if !a.scr.Confirm(a.input, "Remove this account?", it.Label+" is removed from this device. You can add it again later.", "Remove") {
		return
	}
	if err := a.api.DeleteProfile(context.Background(), r.id); err != nil {
		a.scr.Notice(a.input, "Couldn't remove it", err.Error()+tryAgain)
		return
	}
	a.saveState()
	a.refresh()
}

// accountsWhileLoggedOut lets you pick another saved account instead of
// logging in again.
func (a *app) accountsWhileLoggedOut() {
	var items []ui.Item
	var ids []string
	for _, p := range a.snap.profiles {
		if p.NodeID == "" && p.UserProfile.LoginName == "" {
			continue // the empty one being logged in
		}
		items = append(items, ui.Item{Label: p.Login(), Sub: p.About(), Icon: "person"})
		ids = append(ids, p.ID)
	}
	if len(items) == 0 {
		a.note = "No other accounts on this device"
		return
	}
	if i, ok := a.scr.Choose(a.input, "Use another account", items); ok {
		a.switchProfile(ids[i])
	}
}

func (a *app) selfIP() string {
	if a.snap.st != nil && a.snap.st.Self != nil {
		return a.snap.st.Self.IPv4()
	}
	return ""
}

// ---- drawing ----

func (a *app) subtitle() string {
	if a.note != "" {
		return a.note
	}
	s := a.snap
	switch s.state() {
	case stOff:
		if s.err != nil {
			return "Not answering"
		}
		return "Off"
	case stStarting:
		return "Starting…"
	case stNeedsLogin:
		return "Not logged in"
	case stNeedsAuth:
		return "Waiting for approval"
	}
	parts := []string{"Connected"}
	if s.state() == stStopped {
		parts[0] = "Disconnected"
	}
	if ip := a.selfIP(); ip != "" {
		parts = append(parts, ip)
	}
	if s.st.CurrentTailnet != nil && s.st.CurrentTailnet.Name != "" {
		parts = append(parts, s.st.CurrentTailnet.Name)
	}
	return strings.Join(parts, " · ")
}

// hints is the footer. SELECT opens Appearance in every state, but the
// footer under the tabs has no room for its hint (with Y Remove the labels
// are already shortened); there the Settings tab's Appearance row shows it.
func (a *app) hints() []ui.Hint {
	switch a.snap.state() {
	case stOff:
		on := "Turn on"
		if a.snap.err != nil {
			on = "Restart"
		}
		return []ui.Hint{{Button: "A", Label: on}, {Button: "Y", Label: "WireGuard"}, ui.AppearanceHint, {Button: "B", Label: "Exit"}}
	case stNeedsLogin, stNeedsAuth:
		return []ui.Hint{{Button: "A", Label: "Log in"}, {Button: "Y", Label: "Account"}, {Button: "X", Label: "Turn off"}, ui.AppearanceHint, {Button: "B", Label: "Exit"}}
	case stStarting:
		return []ui.Hint{{Button: "X", Label: "Turn off"}, ui.AppearanceHint, {Button: "B", Label: "Exit"}}
	}
	conn := "Disconnect"
	if a.snap.state() == stStopped {
		conn = "Connect"
	}
	h := []ui.Hint{{Button: "A", Label: "Select"}, {Button: "X", Label: conn}}
	if a.tab == tabAccount {
		if it := a.lists[tabAccount].Selected(); it != nil {
			if r, _ := it.Value.(row); r.kind == "profile" && (a.snap.cur == nil || r.id != a.snap.cur.ID) {
				h = append(h, ui.Hint{Button: "Y", Label: "Remove"})
			}
		}
	}
	return append(h, ui.Hint{Button: "L1/R1", Label: "Tab"}, ui.Hint{Button: "B", Label: "Exit"})
}

func (a *app) draw() {
	s := a.scr
	top := s.StaticFrame("MiniScale", ui.Fit(ui.Small, a.subtitle(), ui.W-60), a.hints()...)
	switch a.snap.state() {
	case stOff:
		if a.snap.err != nil {
			// the cause is at the end; the URL before it doesn't wrap
			why := a.snap.err.Error()
			why = strings.TrimSpace(why[strings.LastIndex(why, ": ")+1:])
			a.card(top, "Tailscale isn't answering", "It may still be starting. If this stays, press A to restart it.\n\nThe error: "+why+".")
			break
		}
		a.card(top, "MiniScale is off", "Turn it on to reach the devices on your tailnet from this Miyoo. The first time, you log in with a QR code.")
	case stStarting:
		s.Loading(top+120, "Starting…")
	case stNeedsLogin:
		body := "Log in to your tailnet: press A and scan the QR code with your phone."
		if n := len(a.snap.profiles); n > 1 {
			body += " Or press Y to use another account saved on this device."
		}
		a.card(top, "Not logged in", body)
	case stNeedsAuth:
		body := "This device is logged in, but your tailnet needs an admin to approve it in the Tailscale admin console (Machines)."
		if p := a.snap.prefs; p != nil && !isTailscale(p.ControlURL) {
			body = "This device is logged in, but " + serverHost(p.ControlURL) + " needs its admin to approve it (with Headscale: headscale nodes register)."
		}
		a.card(top, "Waiting for approval", body)
	default:
		a.drawTabs(top)
		r := image.Rect(20, top+46, ui.W-26, ui.H-58)
		l := &a.lists[a.tab]
		l.Items = a.items(a.tab)
		keepSelection(l, a.selID[a.tab])
		if it := l.Selected(); it != nil {
			a.selID[a.tab] = rowID(it)
		}
		l.Draw(s, r)
	}
	s.Present()
}

func keepSelection(l *ui.List, id string) {
	for i := range l.Items {
		if rowID(&l.Items[i]) == id {
			l.Sel = i
			return
		}
	}
	if l.Sel >= len(l.Items) {
		l.Sel = max(0, len(l.Items)-1)
	}
}

func (a *app) drawTabs(top int) {
	s := a.scr
	gap := 8
	w := (ui.W - 40 - gap*(len(tabNames)-1)) / len(tabNames)
	for i, n := range tabNames {
		x := 20 + i*(w+gap)
		s.Capsule(image.Rect(x, top, x+w, top+34), n, i == a.tab, ui.Text)
	}
}

func (a *app) card(top int, title, body string) {
	s := a.scr
	lines := ui.Wrap(ui.Body, body, ui.W-90)
	r := image.Rect(20, top+10, ui.W-20, top+46+ui.LineHeight(ui.Heading)+len(lines)*ui.LineHeight(ui.Body))
	s.Panel(r, 18)
	y := r.Min.Y + 16
	s.Text(r.Min.X+22, y, title, ui.Heading, ui.Text)
	y += ui.LineHeight(ui.Heading) + 6
	for _, l := range lines {
		s.Text(r.Min.X+22, y, l, ui.Body, ui.Muted)
		y += ui.LineHeight(ui.Body)
	}
}

func onOff(b bool) string {
	if b {
		return "On"
	}
	return "Off"
}

func (a *app) items(tab int) []ui.Item {
	s := a.snap
	switch tab {
	case tabDevices:
		return a.deviceItems()
	case tabExit:
		return a.exitItems()
	case tabSettings:
		p := s.prefs
		if p == nil {
			p = &Prefs{}
		}
		name := p.Hostname
		if name == "" && s.st.Self != nil {
			name = s.st.Self.HostName
		}
		files := "None waiting"
		if n := len(s.files); n > 0 {
			files = plural(n, "file") + " waiting"
		}
		ver := s.st.Version
		if i := strings.IndexByte(ver, '-'); i > 0 {
			ver = ver[:i]
		}
		return []ui.Item{
			{Label: "Use Tailscale DNS", Sub: "MagicDNS and your tailnet's DNS", Detail: onOff(p.CorpDNS), Value: row{"pref", "CorpDNS"}},
			{Label: "Use subnet routes", Sub: "Reach subnets your devices share", Detail: onOff(p.RouteAll), Value: row{"pref", "RouteAll"}},
			{Label: "Tailscale SSH", Sub: "SSH in from your tailnet", Detail: onOff(p.RunSSH), Value: row{"pref", "RunSSH"}},
			{Label: "Block incoming connections", Sub: "Shields up: nothing gets in", Detail: onOff(p.ShieldsUp), Value: row{"pref", "ShieldsUp"}},
			{Label: "Device name", Sub: "How it shows in your tailnet", Detail: name, Value: row{"name", ""}, Chevron: true},
			{Label: "Start at boot", Sub: "Join your tailnet at power-on", Detail: onOff(s.boot), Value: row{"boot", ""}},
			{Label: "Network", Sub: "Switch to a plain WireGuard server", Detail: "Tailscale", Value: row{"network", ""}, Chevron: true},
			{Label: "Taildrop", Sub: "Receive files into Media/Taildrop", Detail: files, Value: row{"files", ""}, Chevron: true},
			{Label: "About", Sub: "Tailscale " + ver + " · MiniScale " + version.String(), Value: row{"about", ""}},
			appearanceRow(),
		}
	case tabAccount:
		return a.accountItems()
	}
	return nil
}

// appearanceRow is the Appearance row of the Settings tab.
func appearanceRow() ui.Item {
	it := ui.AppearanceItem()
	it.Value = row{"appearance", ""}
	return it
}

func (a *app) deviceItems() []ui.Item {
	st := a.snap.st
	var out []ui.Item
	peers := sortedPeers(st)
	if self := st.Self; self != nil {
		it := ui.Item{Label: self.Name() + " (this Miyoo)", Sub: strings.Join(self.TailscaleIPs, " · "), Icon: "star", Value: row{"self", ""}}
		if len(peers) > 0 {
			on := 0
			for _, p := range peers {
				if p.Online {
					on++
				}
			}
			it.Detail = fmt.Sprintf("%d of %d online", on, len(peers))
		}
		out = append(out, it)
	}
	for _, p := range peers {
		sub := []string{p.IPv4(), osName(p.OS)}
		detail := ""
		switch {
		case p.Expired:
			detail = "expired"
		case p.Online:
			detail = "online"
		default:
			sub = append(sub, "seen "+ago(p.LastSeen))
		}
		if p.ExitNode {
			detail = "exit node"
		}
		tint := ui.Good
		if !p.Online {
			tint = ui.Faint
		}
		out = append(out, ui.Item{Label: p.Name(), Sub: strings.Join(nonEmpty(sub), " · "), Detail: detail,
			Icon: "server", Tint: tint, Chevron: true, Value: row{"peer", p.ID}})
	}
	if len(peers) == 0 {
		out = append(out, ui.Item{Label: "No other devices yet", Sub: "Devices you add to your tailnet show up here"})
	}
	return out
}

func (a *app) exitItems() []ui.Item {
	st := a.snap.st
	using := a.snap.prefs != nil && a.snap.prefs.ExitNodeID != ""
	none := ui.Item{Label: "None", Sub: "This Wi-Fi's own internet", Icon: "wifi", Value: row{"exit", ""}}
	if !using {
		none.Detail = "in use"
	}
	out := []ui.Item{none}
	for _, p := range sortedPeers(st) {
		if !p.ExitNodeOption {
			continue
		}
		it := ui.Item{Label: p.Name(), Sub: osName(p.OS), Icon: "server", Tint: ui.Good, Value: row{"exit", p.ID}}
		if !p.Online {
			it.Sub, it.Tint = "Offline", ui.Faint
		}
		if a.snap.prefs != nil && a.snap.prefs.ExitNodeID == p.ID {
			it.Detail = "in use"
		}
		out = append(out, it)
	}
	if len(out) == 1 {
		out = append(out, ui.Item{Label: "No exit nodes offered", Sub: "Offer one: tailscale set --advertise-exit-node"})
	}
	lan := false
	if a.snap.prefs != nil {
		lan = a.snap.prefs.ExitNodeAllowLANAccess
	}
	out = append(out, ui.Item{Label: "Allow local network access", Sub: "Reach this Wi-Fi's devices", Detail: onOff(lan), Value: row{"pref", "ExitNodeAllowLANAccess"}})
	return out
}

func (a *app) accountItems() []ui.Item {
	var out []ui.Item
	cur := a.snap.cur
	ps := slices.Clone(a.snap.profiles)
	slices.SortStableFunc(ps, func(x, y Profile) int { return strings.Compare(x.Login(), y.Login()) })
	for _, p := range ps {
		if p.NodeID == "" && p.UserProfile.LoginName == "" {
			continue
		}
		it := ui.Item{Label: p.Login(), Sub: p.About(), Icon: "person", Value: row{"profile", p.ID}}
		if cur != nil && p.ID == cur.ID {
			it.Detail = "in use"
			if u := a.keyExpiry(); u != "" {
				it.Sub += " · " + u
			}
		} else {
			it.Detail = "switch"
		}
		out = append(out, it)
	}
	return append(out,
		ui.Item{Label: "Log in again", Sub: "Re-authenticate with a QR code", Icon: "check", Value: row{"relogin", ""}},
		ui.Item{Label: "Add another account", Sub: "Another tailnet, or your own Headscale server", Icon: "plus", Value: row{"add", ""}},
		ui.Item{Label: "Log out", Sub: "Leave this tailnet", Icon: "cross", Value: row{"logout", ""}},
		ui.Item{Label: "Turn MiniScale off", Sub: "Stop Tailscale and free its memory", Icon: "cross", Value: row{"off", ""}},
	)
}

func (a *app) keyExpiry() string {
	st := a.snap.st
	if st == nil || st.Self == nil || st.Self.KeyExpiry == nil {
		return "key never expires"
	}
	d := time.Until(*st.Self.KeyExpiry)
	if d < 0 {
		return "key expired"
	}
	return "key expires in " + days(d)
}

func days(d time.Duration) string {
	if d < 48*time.Hour {
		return plural(int(d.Hours()), "hour")
	}
	return plural(int(d.Hours()/24), "day")
}

func sortedPeers(st *Status) []*Peer {
	var ps []*Peer
	for _, p := range st.Peer {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(x, y *Peer) int {
		if x.Online != y.Online {
			if x.Online {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(x.Name()), strings.ToLower(y.Name()))
	})
	return ps
}

func osName(os string) string {
	switch strings.ToLower(os) {
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	case "macos":
		return "macOS"
	case "ios":
		return "iOS"
	case "android":
		return "Android"
	case "freebsd":
		return "FreeBSD"
	}
	return os
}

func nonEmpty(ss []string) []string {
	return slices.DeleteFunc(ss, func(s string) bool { return s == "" })
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	}
	return plural(int(d.Hours()/24), "day") + " ago"
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}
