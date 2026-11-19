package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestGuardEvery(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var never time.Time
	for _, c := range []struct {
		name             string
		hotUntil, anchor time.Time
		want             time.Duration
	}{
		{"right after a press", now.Add(time.Second), never, fastGuard},
		{"press moment just over", now, never, slowGuard},
		{"never pressed, no repaint seen", never, never, slowGuard},
		{"just after the menu's repaint", never, now.Add(-10 * time.Millisecond), slowGuard},
		{"mid-second", never, now.Add(-500 * time.Millisecond), slowGuard},
		{"repaint nearly due", never, now.Add(-(tickEvery - early)), fastGuard},
		{"repaint due", never, now.Add(-tickEvery), fastGuard},
		{"repaint a little late", never, now.Add(-(tickEvery + late - time.Millisecond)), fastGuard},
		{"repaint never came", never, now.Add(-(tickEvery + late)), slowGuard},
	} {
		if got := guardEvery(now, c.hotUntil, c.anchor); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStoppedState(t *testing.T) {
	for _, c := range []struct {
		stat string
		want bool
	}{
		{"733 (MainUI) S 1 733 733 0 -1", false},
		{"733 (MainUI) T 1 733 733 0 -1", true},
		{"733 (MainUI) t 1 733 733 0 -1", true},
		{"733 (MainUI) R 1 733 733 0 -1", false},
		{"9 (odd) T name) S 1 9 9", false}, // the last ")" ends the name
		{"9 (odd) S name) T 1 9 9", true},
		{"733 (MainUI)", false},
		{"", false},
	} {
		if got := stoppedState([]byte(c.stat)); got != c.want {
			t.Errorf("%q: got %v, want %v", c.stat, got, c.want)
		}
	}
}

func TestStateOf(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	recent := strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	old := strconv.FormatInt(now.Add(-5*time.Minute).Unix(), 10)
	for _, c := range []struct{ net, body, want string }{
		{"tailscale", `{"BackendState":"Running"}`, "Running"},
		{"tailscale", `{"BackendState":"NeedsLogin"}`, "NeedsLogin"},
		{"tailscale", `junk`, ""},
		{"wireguard", `{"up":true,"peers":[{"lastHandshake":0},{"lastHandshake":` + recent + `}]}`, "Running"},
		{"wireguard", `{"up":true,"peers":[{"lastHandshake":` + old + `}]}`, "Starting"},
		{"wireguard", `{"up":true,"peers":[]}`, "Starting"},
		{"wireguard", `junk`, ""},
		{"netbird", `{"status":"Connected"}`, "Running"},
		{"netbird", `{"status":"Connecting"}`, ""},
		{"netbird", `{"status":"Idle"}`, "Stopped"},
		{"netbird", `{"status":"SessionExpired"}`, "NeedsLogin"},
	} {
		if got := stateOf(c.net, []byte(c.body), now); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.net, c.body, got, c.want)
		}
	}
}

func TestBackend(t *testing.T) {
	dir, err := os.MkdirTemp("", "bd") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "wg.sock")

	// no socket: off, unless the daemon behind the pid file is alive
	if got := backend("wireguard", sock); got != "" {
		t.Errorf("no socket, no pid file: got %q", got)
	}
	os.WriteFile(filepath.Join(dir, "wg.pid"), []byte("999999999\n"), 0o644)
	if got := backend("wireguard", sock); got != "" {
		t.Errorf("a crashed daemon's pid file: got %q", got)
	}
	if _, err := os.Stat("/proc/self"); err == nil { // procs reads /proc
		os.WriteFile(filepath.Join(dir, "wg.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
		if got := backend("wireguard", sock); got != "Starting" {
			t.Errorf("daemon alive, no socket yet: got %q", got)
		}
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hs := strconv.FormatInt(time.Now().Unix(), 10)
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"up":true,"peers":[{"lastHandshake":` + hs + `}]}`))
	}))
	if got := backend("wireguard", sock); got != "Running" {
		t.Errorf("connected: got %q", got)
	}
}
