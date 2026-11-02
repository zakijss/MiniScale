package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// localAPI is a client for tailscaled's local API (the one the tailscale
// command uses), over its Unix socket: only the calls and fields this app
// needs. Every call has a time limit, so a hung tailscaled can't freeze
// the screen.
type localAPI struct {
	hc *http.Client
}

func newLocalAPI(sock string) *localAPI {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns: 2,
	}
	return &localAPI{hc: &http.Client{Transport: tr}}
}

// errStatus is a non-2xx answer; Msg is tailscaled's explanation.
type errStatus struct {
	Code int
	Msg  string
}

func (e *errStatus) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("tailscaled said %d", e.Code)
}

func (c *localAPI) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://local-tailscaled.sock/localapi/v0/"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &errStatus{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// ---- status ----

// Status is tailscaled's view of this device and its tailnet.
type Status struct {
	Version        string
	BackendState   string // NoState, NeedsLogin, NeedsMachineAuth, Stopped, Starting, Running
	AuthURL        string
	TailscaleIPs   []string
	Self           *Peer
	Health         []string
	CurrentTailnet *struct {
		Name            string
		MagicDNSSuffix  string
		MagicDNSEnabled bool
	}
	Peer map[string]*Peer
	User map[string]User
}

// Peer is a device on the tailnet, this one included (Status.Self).
type Peer struct {
	ID             string // the stable node ID
	HostName       string
	DNSName        string
	OS             string
	UserID         int64
	TailscaleIPs   []string
	Tags           []string
	PrimaryRoutes  []string
	CurAddr        string // direct address, "" when relayed
	Relay          string // DERP region code
	RxBytes        int64
	TxBytes        int64
	LastSeen       time.Time
	Online         bool
	ExitNode       bool // in use as this device's exit node
	ExitNodeOption bool // offers to be one
	Active         bool
	KeyExpiry      *time.Time
	Expired        bool
	ShareeNode     bool
}

// User is a tailnet user.
type User struct {
	ID          int64
	LoginName   string
	DisplayName string
}

// Status reads the current status.
func (c *localAPI) Status(ctx context.Context) (*Status, error) {
	var st Status
	return &st, c.do(ctx, "GET", "status", nil, &st)
}

// Name is the peer's short name: the first label of its MagicDNS name, as
// the Tailscale apps show it, else its host name.
func (p *Peer) Name() string {
	if n, _, _ := strings.Cut(p.DNSName, "."); n != "" {
		return n
	}
	return p.HostName
}

// IPv4 is the peer's Tailscale IPv4 address, or its first address if it
// has none.
func (p *Peer) IPv4() string {
	for _, ip := range p.TailscaleIPs {
		if !strings.Contains(ip, ":") {
			return ip
		}
	}
	if len(p.TailscaleIPs) > 0 {
		return p.TailscaleIPs[0]
	}
	return ""
}

// ---- prefs ----

// Prefs are the settings tailscaled keeps for the current profile.
type Prefs struct {
	ControlURL             string
	RouteAll               bool
	ExitNodeID             string
	ExitNodeIP             string
	ExitNodeAllowLANAccess bool
	CorpDNS                bool
	RunSSH                 bool
	WantRunning            bool
	LoggedOut              bool
	ShieldsUp              bool
	Hostname               string
}

// Prefs reads the current prefs.
func (c *localAPI) Prefs(ctx context.Context) (*Prefs, error) {
	var p Prefs
	return &p, c.do(ctx, "GET", "prefs", nil, &p)
}

// EditPrefs changes the given prefs: {"RouteAll": true} is sent as
// {"RouteAllSet": true, "RouteAll": true}, as tailscaled expects.
func (c *localAPI) EditPrefs(ctx context.Context, change map[string]any) (*Prefs, error) {
	masked := map[string]any{}
	for k, v := range change {
		masked[k] = v
		masked[k+"Set"] = true
	}
	var p Prefs
	return &p, c.do(ctx, "PATCH", "prefs", masked, &p)
}

// ---- login and accounts ----

// LoginInteractive asks for a login link; it shows up in Status.AuthURL.
func (c *localAPI) LoginInteractive(ctx context.Context) error {
	return c.do(ctx, "POST", "login-interactive", nil, nil)
}

// Logout logs the current profile out.
func (c *localAPI) Logout(ctx context.Context) error {
	return c.do(ctx, "POST", "logout", nil, nil)
}

// Profile is an account saved on this device.
type Profile struct {
	ID             string
	Name           string
	NodeID         string
	ControlURL     string
	NetworkProfile struct {
		MagicDNSName string
		DomainName   string
	}
	UserProfile User
}

// Tailnet names the profile's tailnet the way the Tailscale apps do.
func (p Profile) Tailnet() string {
	if d := p.NetworkProfile.DomainName; d != "" {
		return d
	}
	return strings.TrimSuffix(p.NetworkProfile.MagicDNSName, ".")
}

// Tagged reports whether the device joined as a tagged device. Such a
// device belongs to the tailnet rather than to a person, and tailscaled
// reports the device itself as the profile's user.
func (p Profile) Tagged() bool {
	l := p.UserProfile.LoginName
	if l == "" || strings.Contains(l, "@") {
		return false
	}
	suffix := strings.TrimSuffix(p.NetworkProfile.MagicDNSName, ".")
	return l == "tagged-devices" || l == p.Name || (suffix != "" && strings.HasSuffix(l, "."+suffix))
}

// Login names the account as people know it: the login name, or for a
// tagged device the tailnet it belongs to.
func (p Profile) Login() string {
	if p.Tagged() {
		if t := p.Tailnet(); t != "" {
			return t
		}
	}
	if p.UserProfile.LoginName != "" {
		return p.UserProfile.LoginName
	}
	return p.Name
}

// About is the second line under an account: its tailnet, or for a tagged
// device that it is one.
func (p Profile) About() string {
	if p.Tagged() {
		if d := strings.TrimSuffix(p.NetworkProfile.MagicDNSName, "."); d != "" {
			return "tagged device · " + d
		}
		return "tagged device"
	}
	return p.Tailnet()
}

// Profiles lists the saved accounts.
func (c *localAPI) Profiles(ctx context.Context) ([]Profile, error) {
	var ps []Profile
	return ps, c.do(ctx, "GET", "profiles/", nil, &ps)
}

// CurrentProfile is the account in use.
func (c *localAPI) CurrentProfile(ctx context.Context) (*Profile, error) {
	var p Profile
	return &p, c.do(ctx, "GET", "profiles/current", nil, &p)
}

// SwitchProfile changes to the account with the given ID.
func (c *localAPI) SwitchProfile(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "profiles/"+url.PathEscape(id), nil, nil)
}

// DeleteProfile removes a saved account.
func (c *localAPI) DeleteProfile(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "profiles/"+url.PathEscape(id), nil, nil)
}

// NewProfile starts an empty profile (and switches to it), for adding an
// account; log in next.
func (c *localAPI) NewProfile(ctx context.Context) error {
	return c.do(ctx, "PUT", "profiles/", nil, nil)
}

// ---- ping ----

// PingResult is the answer to one disco ping.
type PingResult struct {
	Err            string
	LatencySeconds float64
	Endpoint       string // direct address, when direct
	DERPRegionCode string // relay, when relayed
	PeerRelay      string
	IsLocalIP      bool
}

// Ping sends one disco ping to ip, waiting at most 10 s.
func (c *localAPI) Ping(ctx context.Context, ip string) (*PingResult, error) {
	var r PingResult
	q := url.Values{"ip": {ip}, "type": {"disco"}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := c.do(ctx, "POST", "ping?"+q.Encode(), nil, &r)
	if err == nil && r.Err != "" {
		err = errors.New(r.Err)
	}
	return &r, err
}

// ---- Taildrop ----

// WaitingFile is a Taildrop file received and not yet saved.
type WaitingFile struct {
	Name string
	Size int64
}

// WaitingFiles lists the files waiting in the Taildrop inbox.
func (c *localAPI) WaitingFiles(ctx context.Context) ([]WaitingFile, error) {
	var fs []WaitingFile
	return fs, c.do(ctx, "GET", "files/", nil, &fs)
}

// OpenFile streams a received file; close it when done.
func (c *localAPI) OpenFile(ctx context.Context, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://local-tailscaled.sock/localapi/v0/files/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &errStatus{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	return resp.Body, nil
}

// DeleteFile removes a waiting file from the inbox.
func (c *localAPI) DeleteFile(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "files/"+url.PathEscape(name), nil, nil)
}
