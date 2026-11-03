package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// fakeTailscaled serves a few LocalAPI calls on a Unix socket.
type fakeTailscaled struct {
	sock     string
	backend  string // BackendState; Running if empty
	patches  []map[string]any
	logins   int
	deleted  []string
	switched []string
	files    map[string]string
	newEmpty bool // NewProfile was called: list an empty profile p3
}

func startFake(t *testing.T) *fakeTailscaled {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeTailscaled{sock: filepath.Join(dir, "s"), files: map[string]string{"song.mp3": "abc"}}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/localapi/v0/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "local-tailscaled.sock" {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		backend := f.backend
		if backend == "" {
			backend = "Running"
		}
		w.Write([]byte(`{"Version":"1.104.4-t","BackendState":"` + backend + `","Self":{"ID":"nSelf","HostName":"miyoo","DNSName":"miyoo.tail-x.ts.net.","TailscaleIPs":["100.101.1.1","fd7a::1"]},
			"CurrentTailnet":{"Name":"alex@example.com","MagicDNSSuffix":"tail-x.ts.net"},
			"Peer":{"k1":{"ID":"n1","HostName":"nas","DNSName":"nas.tail-x.ts.net.","OS":"linux","TailscaleIPs":["100.101.1.2"],"Online":true,"ExitNodeOption":true},
			        "k2":{"ID":"n2","HostName":"Laptop","DNSName":"laptop.tail-x.ts.net.","OS":"macOS","TailscaleIPs":["100.101.1.3"],"Online":false,"LastSeen":"2026-10-30T10:00:00Z"}}}`))
	})
	mux.HandleFunc("/localapi/v0/prefs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			f.patches = append(f.patches, m)
		}
		w.Write([]byte(`{"WantRunning":true,"CorpDNS":true,"RunSSH":true,"Hostname":"miyoo"}`))
	})
	mux.HandleFunc("/localapi/v0/login-interactive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "want POST", http.StatusBadRequest)
			return
		}
		f.logins++
		w.WriteHeader(204)
	})
	mux.HandleFunc("/localapi/v0/profiles/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/localapi/v0/profiles/current":
			w.Write([]byte(`{"ID":"p1","Name":"alex@example.com","NodeID":"nSelf","UserProfile":{"LoginName":"alex@example.com"},"NetworkProfile":{"DomainName":"example.com"}}`))
		case r.URL.Path == "/localapi/v0/profiles/" && r.Method == "PUT":
			f.newEmpty = true
			w.WriteHeader(204)
		case r.URL.Path == "/localapi/v0/profiles/":
			empty := ""
			if f.newEmpty {
				empty = `,{"ID":"p3","Name":""}`
			}
			w.Write([]byte(`[{"ID":"p1","Name":"alex@example.com","NodeID":"nSelf","UserProfile":{"LoginName":"alex@example.com"},"NetworkProfile":{"DomainName":"example.com"}},
				{"ID":"p2","Name":"alex@work.example","NodeID":"nW","UserProfile":{"LoginName":"alex@work.example"},"NetworkProfile":{"MagicDNSName":"tail-w.ts.net"}}` + empty + `]`))
		case r.Method == "DELETE":
			f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/localapi/v0/profiles/"))
			w.WriteHeader(204)
		case r.Method == "POST":
			f.switched = append(f.switched, strings.TrimPrefix(r.URL.Path, "/localapi/v0/profiles/"))
			w.WriteHeader(204)
		default:
			w.WriteHeader(204)
		}
	})
	mux.HandleFunc("/localapi/v0/files/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/localapi/v0/files/")
		switch {
		case name == "":
			var out []WaitingFile
			for n, c := range f.files {
				out = append(out, WaitingFile{n, int64(len(c))})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == "DELETE":
			delete(f.files, name)
			w.WriteHeader(204)
		default:
			c, ok := f.files[name]
			if !ok {
				http.Error(w, "no such file", http.StatusNotFound)
				return
			}
			io.WriteString(w, c)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f
}

func TestLocalAPI(t *testing.T) {
	f := startFake(t)
	c := newLocalAPI(f.sock)
	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.BackendState != "Running" || st.Self.Name() != "miyoo" || len(st.Peer) != 2 || st.Self.IPv4() != "100.101.1.1" {
		t.Errorf("status %+v", st)
	}
	if _, err := c.EditPrefs(ctx, map[string]any{"RouteAll": true}); err != nil {
		t.Fatal(err)
	}
	if p := f.patches[0]; p["RouteAll"] != true || p["RouteAllSet"] != true || len(p) != 2 {
		t.Errorf("patch %v, want RouteAll and RouteAllSet only", p)
	}
	if err := c.LoginInteractive(ctx); err != nil || f.logins != 1 {
		t.Errorf("login: %v %d", err, f.logins)
	}
	ps, err := c.Profiles(ctx)
	if err != nil || len(ps) != 2 || ps[1].Tailnet() != "tail-w.ts.net" || ps[0].Tailnet() != "example.com" {
		t.Errorf("profiles %+v %v", ps, err)
	}
	if err := c.DeleteProfile(ctx, "p2"); err != nil || len(f.deleted) != 1 || f.deleted[0] != "p2" {
		t.Errorf("delete: %v %v", err, f.deleted)
	}
}

func TestSaveTaildropFiles(t *testing.T) {
	f := startFake(t)
	dir := t.TempDir()
	t.Setenv("MINIDECK_TAILDROP", dir)
	os.WriteFile(filepath.Join(dir, "song.mp3"), []byte("old"), 0o644)
	a := &app{api: newLocalAPI(f.sock)}
	n, err := a.saveFiles([]WaitingFile{{"song.mp3", 3}})
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "song (2).mp3"))
	if err != nil || string(b) != "abc" {
		t.Errorf("saved %q %v; the old song.mp3 must stay and the new one get (2)", b, err)
	}
	if len(f.files) != 0 {
		t.Error("not removed from the inbox")
	}
	// a saved file that looks like a temporary one is left alone
	os.WriteFile(filepath.Join(dir, "a.txt.part"), []byte("mine"), 0o644)
	f.files = map[string]string{"a.txt": "new"}
	if _, err := a.saveFiles([]WaitingFile{{"a.txt", 3}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt.part")); string(b) != "mine" {
		t.Errorf("a.txt.part overwritten: %q", b)
	}
	for _, name := range []string{"..", ".", "/"} {
		if _, err := a.saveFiles([]WaitingFile{{name, 1}}); err == nil {
			t.Errorf("saved a file called %q", name)
		}
	}
}

// A bad name doesn't stop the files after it, and names FAT refuses are
// changed into ones it takes.
func TestSaveTaildropKeepsGoing(t *testing.T) {
	f := startFake(t)
	f.files = map[string]string{"Notes: 1?.txt": "n", "song.mp3": "abc"}
	dir := t.TempDir()
	t.Setenv("MINIDECK_TAILDROP", dir)
	a := &app{api: newLocalAPI(f.sock)}
	n, err := a.saveFiles([]WaitingFile{{"gone.txt", 1}, {"Notes: 1?.txt", 1}, {"song.mp3", 3}})
	if n != 2 || err == nil || !strings.Contains(err.Error(), "gone.txt") || strings.Contains(err.Error(), "song") {
		t.Fatalf("%d %v; want 2 saved and only gone.txt reported", n, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Notes_ 1_.txt")); err != nil || string(b) != "n" {
		t.Errorf("Notes: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "song.mp3")); err != nil || string(b) != "abc" {
		t.Errorf("song: %q %v", b, err)
	}
}

func TestFATName(t *testing.T) {
	for in, want := range map[string]string{
		"photo.jpg":         "photo.jpg",
		`a\b:c*d?e"f<g>h|i`: "a_b_c_d_e_f_g_h_i",
		"tab\there\x7f":     "tab_here_",
		"trailing. . ":      "trailing",
		"...":               "file",
		"bad\xffutf8":       "bad_utf8",
		"café ☕.txt":        "café ☕.txt",
	} {
		if got := fatName(in); got != want {
			t.Errorf("fatName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A snapshot the poller began reading before an action doesn't replace the
// one read after it.
func TestStaleSnapshotDropped(t *testing.T) {
	a := &app{dir: t.TempDir(), sock: filepath.Join(t.TempDir(), "none")}
	old := a.fetchGen()
	old.boot = true // as things were before the action
	a.refresh()
	a.take(old)
	if a.snap.boot {
		t.Error("a snapshot from before the action was shown")
	}
	cur := a.fetchGen()
	cur.boot = true
	a.take(cur)
	if !a.snap.boot {
		t.Error("a current snapshot was dropped")
	}
}

// Cancelling the login to a new account goes back to the old one and
// removes the empty profile.
func TestAddAccountCancelled(t *testing.T) {
	f := startFake(t)
	f.backend = "NeedsLogin"
	t.Setenv("MINIDECK_UI_PNG", filepath.Join(t.TempDir(), "f.png"))
	scr, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	in := make(chan ui.Event)
	close(in) // leaving the login screen at once, as B does
	a := &app{scr: scr, input: in, api: newLocalAPI(f.sock), dir: t.TempDir(), sock: f.sock}
	a.snap = a.fetchGen()
	a.addAccount()
	if !slices.Equal(f.switched, []string{"p1"}) || !slices.Equal(f.deleted, []string{"p3"}) {
		t.Errorf("switched to %v, deleted %v; want back to p1 and p3 deleted", f.switched, f.deleted)
	}
}

// Turning on with a saved state this device can't open starts afresh for a
// new login instead of failing the same way every time.
func TestTurnOnUnreadableState(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `echo "$1" >> "` + calls + `"
case "$1" in
start) echo "could not decrypt state (different device or flash wiped): log in again"; exit 3 ;;
esac
`
	os.WriteFile(filepath.Join(dir, "miniscale.sh"), []byte(script), 0o644)
	os.WriteFile(filepath.Join(dir, "state.enc"), []byte("sealed elsewhere"), 0o644)
	t.Setenv("MINIDECK_UI_PNG", filepath.Join(t.TempDir(), "f.png"))
	scr, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	in := make(chan ui.Event)
	close(in)
	a := &app{scr: scr, input: in, dir: dir, sock: filepath.Join(dir, "none")}
	a.turnOn()
	b, _ := os.ReadFile(calls)
	if got := strings.Fields(string(b)); !slices.Equal(got, []string{"start", "fresh"}) {
		t.Errorf("miniscale.sh ran with %q, want start then fresh", got)
	}
	if err := a.script("start"); !isNoState(err) || err.Error() != "could not decrypt state (different device or flash wiped): log in again" {
		t.Errorf("script error %#v", err)
	}
}

func TestStates(t *testing.T) {
	for want, s := range map[state]snapshot{
		stOff:        {},
		stRunning:    {on: true, st: &Status{BackendState: "Running"}},
		stStopped:    {on: true, st: &Status{BackendState: "Stopped"}},
		stNeedsLogin: {on: true, st: &Status{BackendState: "NeedsLogin"}},
		stNeedsAuth:  {on: true, st: &Status{BackendState: "NeedsMachineAuth"}},
		stStarting:   {on: true, st: &Status{BackendState: "Starting"}},
	} {
		if got := s.state(); got != want {
			t.Errorf("%+v: %v, want %v", s.st, got, want)
		}
	}
}

func TestHostnames(t *testing.T) {
	for _, ok := range []string{"miyoo", "miyoo-plus-2", "A1"} {
		if checkHostname(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-miyoo", "miyoo-", "my miyoo", "miyoo.local", strings.Repeat("a", 64)} {
		if checkHostname(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConfDefaults(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "miniscale.conf"), []byte("# settings\nTS_HOSTNAME=\"handheld\"\nTS_SSH=false\n"), 0o644)
	if n, ssh := confDefaults(dir); n != "handheld" || ssh {
		t.Errorf("%q %v", n, ssh)
	}
	if n, ssh := confDefaults(t.TempDir()); n != "miyoo" || !ssh {
		t.Errorf("defaults %q %v", n, ssh)
	}
}

func TestSplitLink(t *testing.T) {
	lines := splitLink("https://login.tailscale.com/a/1a2b3c4d5e6f7a", 160)
	if strings.Join(lines, "") != "login.tailscale.com/a/1a2b3c4d5e6f7a" || len(lines) < 2 {
		t.Errorf("%q", lines)
	}
}

func TestAgo(t *testing.T) {
	if ago(time.Time{}) != "never" || ago(time.Now().Add(-3*time.Hour)) != "3 hours ago" {
		t.Error(ago(time.Now().Add(-3 * time.Hour)))
	}
}

func TestTaggedProfile(t *testing.T) {
	var p Profile
	p.Name = "miyoo.tail-x.ts.net"
	p.UserProfile.LoginName = "miyoo.tail-x.ts.net"
	p.NetworkProfile.MagicDNSName = "tail-x.ts.net"
	p.NetworkProfile.DomainName = "alex@example.com"
	if !p.Tagged() || p.Login() != "alex@example.com" || p.About() != "tagged device · tail-x.ts.net" {
		t.Errorf("tagged: %v %q %q", p.Tagged(), p.Login(), p.About())
	}
	p.UserProfile.LoginName = "alex@example.com"
	if p.Tagged() || p.Login() != "alex@example.com" || p.About() != "alex@example.com" {
		t.Errorf("person: %v %q %q", p.Tagged(), p.Login(), p.About())
	}
}
