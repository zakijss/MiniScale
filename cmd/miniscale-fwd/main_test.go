package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	good := "# forwards\n\n8096  homeserver:8096   # Jellyfin\n4533 nas:4533\n  1 [fd7a::1]:22\n"
	p := filepath.Join(t.TempDir(), "forwards.conf")
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	fw, err := parse(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []forward{{8096, "homeserver:8096"}, {4533, "nas:4533"}, {1, "[fd7a::1]:22"}}
	if len(fw) != len(want) {
		t.Fatalf("got %v, want %v", fw, want)
	}
	for i := range want {
		if fw[i] != want[i] {
			t.Errorf("line %d: got %v, want %v", i, fw[i], want[i])
		}
	}

	for _, bad := range []string{
		"8096",
		"8096 nas:8096 extra",
		"0 nas:1",
		"65536 nas:1",
		"-1 nas:1",
		"port nas:1",
		"8096 nas",
		"8096 :8096",
		"8096 nas:",
		"8096 [fd7a::1",
	} {
		if err := os.WriteFile(p, []byte(bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if fw, err := parse(p); err == nil {
			t.Errorf("%q: accepted as %v", bad, fw)
		}
	}
}

// socksServer runs a minimal SOCKS5 proxy (no auth, CONNECT to an IPv4
// address) that passes half-closes on, as tailscaled's does.
func socksServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 256)
				if _, err := io.ReadFull(c, b[:2]); err != nil { // version, method count
					return
				}
				if _, err := io.ReadFull(c, b[:b[1]]); err != nil {
					return
				}
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, b[:10]); err != nil || b[3] != 1 { // CONNECT to IPv4:port
					return
				}
				addr := net.JoinHostPort(net.IP(b[4:8]).String(), strconv.Itoa(int(b[8])<<8|int(b[9])))
				r, err := net.Dial("tcp", addr)
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer r.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				up := make(chan struct{})
				go func() {
					io.Copy(r, c)
					r.(*net.TCPConn).CloseWrite()
					close(up)
				}()
				io.Copy(c, r)
				c.(*net.TCPConn).CloseWrite()
				<-up
			}()
		}
	}()
	return ln.Addr().String()
}

// A client that half-closes after its request still gets the reply.
func TestHalfCloseThroughSOCKS(t *testing.T) {
	// the target answers once the request has ended
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, _ := io.ReadAll(c)
		fmt.Fprintf(c, "got %q", req)
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serve(ln, target.Addr().String(), socksDialer(socksServer(t)))

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("hello"))
	c.(*net.TCPConn).CloseWrite()
	reply, err := io.ReadAll(c)
	if err != nil || string(reply) != `got "hello"` {
		t.Errorf("reply %q, %v; want the target's answer after the half-close", reply, err)
	}
}

// A side that stops reading doesn't hold the connection forever.
func TestPipeWriteStall(t *testing.T) {
	old := writeStall
	writeStall = 100 * time.Millisecond
	defer func() { writeStall = old }()
	client, a := net.Pipe()
	b, remote := net.Pipe() // nothing ever reads remote
	defer client.Close()
	defer remote.Close()
	done := make(chan struct{})
	go func() {
		pipe(a, b)
		close(done)
	}()
	go client.Write([]byte("data"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipe still blocked writing to a side that doesn't read")
	}
}
