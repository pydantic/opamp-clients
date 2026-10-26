// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package opamp is an OpAMP client over plain HTTP or WebSocket.
//
// Everything OpAMP (what to send and when, backoff, heartbeats, full reports, instance UIDs,
// agent_disconnect, and the requests and responses themselves: headers, framing, Retry-After, size
// limits) is the shared core, run as WebAssembly in wazero (pure Go). By default the core speaks
// HTTP and WebSocket itself too (core mode), and this package only gives it Go's TCP connections
// and crypto/tls; in platform mode (below) this package carries out the core's requests with
// net/http and gorilla/websocket instead. Either way it feeds back what happened, so it behaves
// exactly like the other languages' clients on the same core. The endpoint's scheme says the
// transport: http:// and https:// plain HTTP, ws:// and wss:// WebSocket.
//
// # Callbacks
//
// The notification callback is called one at a time, on the client's goroutine, never before
// Start returns and never after the Stopped notification; a panic in it stops the client (not the
// process) and is reported once, as Stopped's Err and Stop's error. Every method may be called
// from it and from any goroutine; Stop called from it returns at once, and the client stops once
// the callback has returned. What the callback asks before it returns (its answer to a remote
// config, say) reaches the core before anything the client takes in next: what the server says, a
// connection that opened, a timer. docs/sdk-contract.md has the contract every SDK here keeps.
//
// # Time, stopping and the instance UID
//
// The client keeps no time of its own: how long a request, a WebSocket connection being opened,
// a closing handshake or a download may take is the core's (Config.RequestTimeout,
// ConnectTimeout, DownloadTimeout), and an effect ends with its result or the core's cancel,
// which the client carries out at once. Stop ends when the core says it has stopped, within
// Config.StopTimeout: the goodbye and the closing handshake are done or given up by then.
//
// With no Config.InstanceUID, the core makes one (a UUIDv7, from the clock and the operating
// system's randomness) and, with a state file, keeps it across restarts; Client.InstanceUID says
// which is in use.
//
// Package files are downloaded without following redirects: the client reports each response,
// whatever its status, with a redirect's Location resolved by net/url, and the core decides.
//
// # Core mode (the default) and platform mode
//
// Config.Mode says who speaks HTTP and WebSocket. The client runs in core mode unless the
// application asks for platform mode: by naming it in the mode option, or by handing the client
// an HTTP client of its own, where the SDK takes one (an application that brings its own client
// has asked for the platform's stack). An option only one mode honours is never silently ignored:
// it selects that mode, or the client refuses to start with an error that says what to set
// instead.
//
// Here platform mode is net/http's: Mode set to ModePlatform, and Config.HTTPClient selects it
// too. Start refuses ModeCore with an HTTPClient, a TLS.Config that names the server (ServerName)
// in core mode, and NoSpliceDownloads in platform mode.
//
// In core mode the shared core speaks HTTP/1.1 (chunked bodies, kept connections), WebSocket,
// HTTP CONNECT and SOCKS5 itself (in the same WebAssembly module), so the bytes
// on the wire are the same in every language's client. Go only opens TCP connections, starts TLS
// on them with crypto/tls and Config.TLS (the certificate verified for the host, SNI unless the
// host is an address; TLS inside TLS to an https:// proxy), resolves names (only for a SOCKS5
// proxy with Proxy.ResolveLocally), tells the core which proxy to use, moves the bytes with
// backpressure, and writes package files (made new, synced before they are handed over; with the
// core tap on, journaled as in platform mode). The core keeps every time and decides redirects.
// Package bodies go from the connection straight to their file unless Config.NoSpliceDownloads is
// set.
//
//	cfg := opamp.Config{Endpoint: "wss://opamp.example.com/v1/opamp"}
//
// In platform mode net/http carries OpAMP requests and package downloads (HTTP/2 where the server
// offers it) and gorilla/websocket the WebSocket. Choose it to bring your own *http.Client, or
// when the connection must be made as net/http makes it: by net/http's reading of the
// environment's proxies (below), over HTTP/2, or with a tls.Config that names the server
// (TLS.Config.ServerName).
//
//	cfg := opamp.Config{Endpoint: "wss://opamp.example.com/v1/opamp", Mode: opamp.ModePlatform}
//
// # TLS and proxies
//
// Config.TLS and Config.Proxy apply to everything the client connects to, in both modes: OpAMP
// over HTTP, the WebSocket, and package downloads. TLS.CAPEM is the CA to trust instead of the
// system's (set IncludeSystemCAs to trust both); TLS.CertPEM and TLS.KeyPEM are the client
// certificate presented in every handshake; MinVersion and MaxVersion bound the version, never
// below TLS 1.2. With no TLS set the system's CAs are trusted, as crypto/tls finds them.
// Proxy.URL is an http://, https://, socks5:// or socks5h:// proxy, with its credentials in the
// URL, and Proxy.ConnectHeader adds headers to each CONNECT. A SOCKS5 proxy resolves the server's
// name under either scheme (unlike curl, where socks5:// resolves locally); Proxy.ResolveLocally
// resolves it on the client instead. SOCKS4 and other schemes are refused.
//
//	ca, _ := os.ReadFile("/etc/agent/ca.pem")
//	cert, _ := os.ReadFile("/etc/agent/client.pem")
//	key, _ := os.ReadFile("/etc/agent/client-key.pem")
//	cfg := opamp.Config{
//		Endpoint: "wss://opamp.example.com/v1/opamp",
//		TLS:      opamp.TLS{CAPEM: ca, CertPEM: cert, KeyPEM: key},
//		Proxy:    opamp.Proxy{URL: "http://agent:secret@proxy.internal:3128"},
//	}
//
// With no proxy configured, the proxy comes from the environment, on HTTP, WebSocket and
// downloads alike; Proxy.Direct tells the client to go direct instead, ignoring the environment.
// The two modes read the environment by different rules:
//
//   - core mode, read once when the client starts, by the one rule every SDK's core mode shares
//     (tests/shell-tables/README.md, its proxy rule), which the core applies:
//     REQUEST_METHOD set (CGI) means none; each variable's upper-case
//     name if it is set at all, else its lower-case one, a value empty or only spaces being
//     unset; HTTP_PROXY for http:// and ws://, HTTPS_PROXY for https:// and wss://, either
//     falling back to ALL_PROXY (a SOCKS5 proxy too), its value trimmed; NO_PROXY's
//     comma-separated entries sending a host direct: * (names only), a name (with or without a
//     leading dot, covering the names under it, ignoring case), an IP address or a network
//     (10.0.0.0/8, 2001:db8::/32) for addresses. localhost and loopback addresses are not
//     special: with HTTP_PROXY set, a server on localhost is reached through the proxy unless
//     NO_PROXY names it. NO_PROXY's host:port entries match nothing.
//   - platform mode, by net/http's (http.ProxyFromEnvironment): HTTP_PROXY, HTTPS_PROXY and NO_PROXY,
//     read once per process; no ALL_PROXY; localhost and loopback addresses never proxied;
//     NO_PROXY's host:port entries honoured; none under CGI. A proxy it names follows the rules
//     of one configured.
//
// A configured Proxy.URL is used as it is in both modes (in core mode it is handed to the core,
// with its credentials and ConnectHeader).
//
// An application with its own *http.Client sets Config.HTTPClient instead, and then no TLS or
// Proxy; the client then runs in platform mode. Its Transport carries HTTP and downloads as it is,
// one round trip each (no redirect is followed, no cookie sent), and WebSocket takes the
// Transport's TLSClientConfig, Proxy, ProxyConnectHeader and DialContext (so it must be an
// *http.Transport). Options that can't work (PEM that doesn't parse, a key that doesn't match its
// certificate, a proxy scheme the client doesn't know) stop Start with the reason.
//
// The client certificate's key stays in memory: the client doesn't write it anywhere. The state
// file is still a secret, though: it holds what the server issued (credentials, and the private
// keys of telemetry settings' certificates). See Config.StateFile.
package opamp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/internal/core"
	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// Config is the client's settings.
type Config struct {
	// Endpoint is the server URL: http(s)://... for plain HTTP, ws(s)://... for WebSocket.
	Endpoint string
	// InstanceUID to start with. The server may assign another (InstanceUIDChanged).
	// Zero: none: the core makes one (a UUIDv7), and with a state file keeps it across restarts
	// (Client.InstanceUID says which).
	InstanceUID [16]byte
	// Capabilities the agent supports (AgentCapabilities bits), fixed for the run. ReportsStatus is
	// always added, and the core adds the bits of what it does itself (packages, connection
	// settings). Declare ReportsHeartbeat for heartbeats, and the Reports* bit of each status you
	// set: a status whose bit isn't declared isn't sent.
	Capabilities uint64
	// HeartbeatInterval until the server offers one; 0: 30 s.
	HeartbeatInterval time.Duration
	// Header is sent with every request or upgrade (e.g. Authorization).
	Header http.Header
	// AuthHeader, if set, gives the headers an authentication extension adds to every request and
	// WebSocket upgrade: a token it renews, say. It is called once before the client starts, then
	// about every second on a goroutine of its own while the client runs; when what it gives
	// changes, the core takes it from the next request or connection on (a connection open keeps
	// its headers). Each header replaces the one of the same name in Header, or in connection
	// settings the server offered. The core checks every list it sends, as it checks Header: one
	// its rule refuses fails that attempt, and nothing is sent. Both modes.
	AuthHeader func() http.Header
	// RequestTimeout for plain HTTP; 0: 30 s.
	RequestTimeout time.Duration
	// ConnectTimeout is how long a WebSocket connection may take to open; 0: 30 s.
	ConnectTimeout time.Duration
	// StopTimeout bounds Stop: the goodbye (agent_disconnect) and the closing handshake of a
	// WebSocket, and a closing handshake at any time; 0: 5 s.
	StopTimeout time.Duration
	// MaxSendBytes is the largest message the client sends, encoded; 0: 64 MiB, and at least
	// 1 KiB. A status that doesn't fit is left out (StatusTooLarge).
	// covers: OPAMP-SIZE-02, OPAMP-SIZE-08
	MaxSendBytes uint64
	// MaxReceiveBytes is the largest response or WebSocket message the client accepts; 0: 64 MiB.
	// covers: OPAMP-SIZE-02, OPAMP-SIZE-08
	MaxReceiveBytes uint64
	// TLS is how the client makes TLS connections (https://, wss://, package downloads, an
	// https:// proxy), in both modes. The zero value trusts the system's CAs.
	TLS TLS
	// Proxy is the proxy to reach the server through, for HTTP, WebSocket and package downloads
	// alike. The zero value takes it from the environment (HTTP_PROXY, HTTPS_PROXY, ALL_PROXY,
	// NO_PROXY, by core mode's rule; in platform mode by net/http's); Proxy.Direct goes direct.
	Proxy Proxy
	// HTTPClient, if set, is used instead of TLS and Proxy (setting both is an error), and the
	// client runs in platform mode: only net/http can use it (with Mode set to ModeCore it is an
	// error). OpAMP requests and package downloads go through its Transport, one round trip
	// each: no redirect is followed (the core decides) and its Jar and Timeout don't apply.
	// WebSocket takes the TLS settings, proxy, CONNECT headers and dialer of its Transport, which
	// must then be an *http.Transport (nil: http.DefaultTransport).
	HTTPClient *http.Client
	// StateFile is where the client keeps state across restarts; "": nowhere. It holds secrets
	// (credentials the server issued, private keys of telemetry settings' certificates), so it
	// is written readable by the agent's user only (mode 0600). On Windows it takes its
	// directory's permissions: keep it on a local path only the agent's user can read.
	StateFile string
	// StateSealer, if set, encrypts the state file: a save writes Seal(state), and Start reads
	// Open(file). Start fails if Open does (a state it couldn't read would lose the credentials
	// in it); a save that can't be sealed fails like one that can't be written. Package statekey
	// has one: AES-256-GCM under a key kept by the OS (systemd credentials, Keychain, DPAPI).
	StateSealer StateSealer
	// AcceptConnectionSettings takes offers of new connection settings from the server
	// (docs/connection-settings.md). Offers are refused without a StateFile: a restart would
	// lose them.
	AcceptConnectionSettings bool
	// VerifyTimeout is how long offered settings have to prove themselves; 0: 60 s.
	VerifyTimeout time.Duration
	// AcceptPackages takes packages from the server (docs/packages.md): the client downloads
	// them to StateFile + ".downloads" and the application installs them (InstallPackage,
	// RemovePackage). Only with a StateFile: a restart would forget what the agent has.
	AcceptPackages bool
	// RequestInstanceUID asks the server to assign the instance UID (the RequestInstanceUid
	// flag, docs/instance-uid.md). The UID the client starts with (made up, or InstanceUID) is
	// the temporary value until the server assigns one. With a StateFile the assigned UID is
	// kept, and a restart doesn't ask again; without one every start asks again, and the server
	// assigns a new UID each time: the agent is a new agent after every restart.
	// covers: OPAMP-UID-04
	RequestInstanceUID bool
	// MaxPackageBytes is the largest package file downloaded; 0: 1 GiB.
	MaxPackageBytes uint64
	// DownloadTimeout is how long a download may take; 0: 10 minutes.
	DownloadTimeout time.Duration
	// Mode is who speaks HTTP and WebSocket: ModeCore (the shared core itself, over Go's TCP
	// connections and crypto/tls) or ModePlatform (net/http and gorilla/websocket). Unset (""): core
	// mode, unless HTTPClient is set, which only platform mode can use. Both take TLS and Proxy.
	Mode Mode
	// NoSpliceDownloads, in core mode, passes package files' bytes through the core. Unset (the
	// default), a download's body goes from the connection straight to its file wherever the
	// core knows the next bytes are body (Config.splice_downloads). It is core mode's: in platform
	// mode it is an error.
	NoSpliceDownloads bool

	// coreModule, when set (by tests in this package only), is the WebAssembly module run
	// instead of the embedded core: the adversary (testdata/core/opamp_core_adversary.wasm).
	coreModule []byte
	// tapFile, when set (by tests), is the file the core is tapped to, whatever OPAMP_CORE_TAP
	// says.
	tapFile string
	// fs, when set (by tests), is what saves and downloads write with instead of the disk.
	fs fileSystem
	// streamNet, when set (by tests), is what core mode connects, starts TLS and resolves with
	// (its nil functions: Go's own).
	streamNet *streamPlatform
	// environ, when set (by tests), is the environment core mode's proxy rule reads.
	environ func(string) (string, bool)
	// authEvery, when set (by tests), is how often AuthHeader is asked again; 0: every second.
	authEvery time.Duration
}

// Status is the agent's status as the client starts. The first message reports it in full.
type Status struct {
	Description        *protobufs.AgentDescription
	Health             *protobufs.ComponentHealth
	EffectiveConfig    *protobufs.EffectiveConfig
	RemoteConfigStatus *protobufs.RemoteConfigStatus
	// AvailableComponents: the components the agent has (leave Hash empty: the core computes it).
	// The server gets the hash, and the components in full when it asks.
	AvailableComponents *protobufs.AvailableComponents
	// CustomCapabilities: the custom capabilities the agent supports; custom messages for them
	// reach the application (CustomMessage), and it may send them.
	CustomCapabilities *protobufs.CustomCapabilities
}

// Client is a running OpAMP client.
type Client struct {
	// cmds holds the application's calls, in order, until the client's goroutine takes them.
	// Putting never blocks: the notification callback runs on that goroutine, so a call from it
	// into a full buffer would wait forever.
	cmds *mailbox[*corepb.Input]
	done chan struct{}
	err  error
	uid  atomic.Pointer[[16]byte] // the instance UID in use, as the core says
	// loopG is the id of the client's goroutine, the one the callback runs on (0 until it runs):
	// Stop called there is a stop asked for from the callback, which can't wait for the client.
	loopG atomic.Uint64
	// used is where the latest OpAMP request or connection went (the executor's).
	used *atomic.Pointer[string]
}

// Start starts a client. notify is called for every Notification, one at a time, on the client's
// goroutine, with the client: keep it quick (the client waits for it). Stop called from it asks
// for the stop and returns at once: the client stops once notify has returned, and Stopped comes
// after. It is never called before Start returns, though perhaps before
// the caller has stored what Start returned (a remote config saved before a restart comes at
// once): answer through the client it is given. The other methods may be called from it, and from
// any goroutine at once; none of them blocks. docs/sdk-contract.md has the whole contract.
//
// Start is Open, then Opened.Start.
func Start(ctx context.Context, cfg Config, status Status, notify func(*Client, Notification)) (*Client, error) {
	o, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return o.Start(status, notify)
}

// Resumed is what the client starts from, read from its state file at Open: for an application
// that needs it before the client connects. A host that runs an agent of its own (the Supervisor
// its collector) starts it on AppliedRemoteConfig and ConnectionSettings, keeping no copy of them
// itself. Every field is nil without a state file, or with a state that isn't this client's
// (another endpoint's: AppliedRemoteConfig aside, which is kept whatever the endpoint).
type Resumed struct {
	// AppliedRemoteConfig is the config the application last answered APPLIED: what it runs from
	// the start.
	AppliedRemoteConfig *protobufs.AgentRemoteConfig
	// RemoteConfig is the config Start hands the application (RemoteConfig), to work on: the last
	// received, unless the application refused it.
	RemoteConfig *protobufs.AgentRemoteConfig
	// RemoteConfigRefused is the application's FAILED about the last config received, reported
	// again at Start; that config is not handed over again until the server offers another.
	RemoteConfigRefused *protobufs.RemoteConfigStatus
	// ConnectionSettings are the telemetry settings Start hands the application
	// (ConnectionSettings): the parts it asks for.
	ConnectionSettings *protobufs.ConnectionSettingsOffers
}

// Opened is a client that has read its state but sent nothing: Start it, or Close it.
type Opened struct {
	cfg      Config
	core     *core.Core
	out      *corepb.Output
	coreMode bool
	conn     *connection
	tlsCfg   *tls.Config
	proxies  *coreProxies

	mu   sync.Mutex
	used bool // started or closed
}

// Open reads the client's state (its state file, if it has one) and says what the client starts
// from (InstanceUID, Resumed), without connecting: Start the Opened to run it. ctx bounds Open.
// Until it is started or closed, nothing else may use the state file.
func Open(ctx context.Context, cfg Config) (*Opened, error) {
	coreMode, err := cfg.coreMode()
	if err != nil {
		return nil, err
	}
	conn, err := cfg.connection()
	if err != nil {
		return nil, err
	}
	cc, err := coreConfig(cfg)
	if err != nil {
		return nil, err
	}
	var tlsCfg *tls.Config
	var proxies *coreProxies
	if coreMode {
		// The core speaks HTTP and WebSocket itself, over the streams this gives it.
		if tlsCfg, err = cfg.TLS.tlsConfig(); err != nil {
			return nil, fmt.Errorf("opamp: TLS: %w", err)
		}
		cc.Streams, cc.SpliceDownloads = true, !cfg.NoSpliceDownloads
		// The core picks each connection's proxy from these.
		proxies = newCoreProxies(cfg.Proxy, cfg.environ)
		cc.Proxy = proxies.choice
	}
	if cfg.StateFile != "" {
		// A save or a download a client stopped before on this file left finishing ends first.
		workOn(cfg.StateFile).settle()
		saved, err := os.ReadFile(cfg.StateFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("opamp: reading the state file: %w", err)
		}
		if cfg.StateSealer != nil && len(saved) > 0 {
			if saved, err = cfg.StateSealer.Open(saved); err != nil {
				return nil, fmt.Errorf("opamp: opening the state file: %w", err)
			}
		}
		cc.Persistence, cc.SavedState = true, saved
		// Files downloaded before a restart: the core downloads again what it still needs.
		_ = os.RemoveAll(downloadsDir(cfg.StateFile))
	}
	c, out, err := core.New(ctx, core.Options{Module: cfg.coreModule, Tap: cfg.tapFile}, cc)
	if err != nil {
		return nil, err
	}
	return &Opened{cfg: cfg, core: c, out: out, coreMode: coreMode, conn: conn, tlsCfg: tlsCfg, proxies: proxies}, nil
}

// InstanceUID is the instance UID the client starts with: the application's, one saved before
// a restart, or one the core made (docs/instance-uid.md).
func (o *Opened) InstanceUID() [16]byte {
	if uid := o.out.GetInstanceUid(); len(uid) == 16 {
		return [16]byte(uid)
	}
	return [16]byte{}
}

// Resumed is what the client starts from.
func (o *Opened) Resumed() Resumed {
	r := o.out.GetResumed()
	return Resumed{
		AppliedRemoteConfig: r.GetAppliedRemoteConfig(),
		RemoteConfig:        r.GetRemoteConfig(),
		RemoteConfigRefused: r.GetRemoteConfigRefused(),
		ConnectionSettings:  r.GetConnectionSettings(),
	}
}

// Close lets go of a client that won't be started.
func (o *Opened) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.used {
		return
	}
	o.used = true
	_ = o.core.Close(context.Background())
}

// errOpenedUsed: Start on an Opened started or closed already.
var errOpenedUsed = errors.New("opamp: the client was started or closed already")

// Start starts the opened client, as Start does: status is the agent's status as it starts, and
// notify is told everything that happens, on the client's goroutine.
func (o *Opened) Start(status Status, notify func(*Client, Notification)) (*Client, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.used {
		return nil, errOpenedUsed
	}
	o.used = true
	cfg, c, out := o.cfg, o.core, o.out
	cl := &Client{cmds: newMailbox[*corepb.Input](), done: make(chan struct{})}
	cl.setUID(out.GetInstanceUid())
	var first []report
	for _, in := range statusInputs(status) {
		first = append(first, report{in: in})
	}
	var auth http.Header
	if cfg.AuthHeader != nil {
		// The first request or connection has them.
		auth = cfg.AuthHeader()
		first = append(first, report{in: authInput(auth)})
	}
	first = append(first, report{in: &corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}}})
	x := newExecutor(o.conn)
	cl.used = &x.used
	if cfg.fs != nil {
		x.fs = cfg.fs
	}
	x.setStateFile(cfg.StateFile)
	x.sealer = cfg.StateSealer
	if cfg.StateFile != "" {
		// With the tap on, every file operation goes to it, the state file named first (core
		// mode's download files too: they go through x.fs).
		x.fs = journaled(x.fs, cfg.StateFile, c.FileTap())
	}
	if o.coreMode {
		// What the core hands over that isn't a stream's (store, discard, a save's cancel; and
		// platform effects, should a core emit them) goes to x as in platform mode.
		x.streams = newStreams(x, o.tlsCfg, o.proxies, cfg.streamNet)
	}
	if notify == nil {
		notify = func(*Client, Notification) {}
	}
	// Stop is bounded by the core's own stop timeout, as given or its default: init reports it.
	stopBound := time.Duration(out.GetLimits().GetStopTimeoutMs()) * time.Millisecond
	l := &loop{core: c, x: x, notify: func(n Notification) { notify(cl, n) }, cmds: cl.cmds, start: time.Now(),
		queue: first, uid: cl.setUID, stopBound: stopBound}
	started := make(chan struct{})
	go func() {
		cl.loopG.Store(goid())
		// Nothing is told before Start returns.
		<-started
		cl.err = l.run()
		if cl.err != nil && !l.saidStopped {
			// The client stopped on its own: the application hears it now, not only from Stop.
			// (A callback that panicked on Stopped itself has heard it: Stopped is said once, and
			// Stop returns the panic.)
			_ = l.tell(Notification{Kind: Stopped, Err: cl.err})
		}
		close(cl.done)
	}()
	if cfg.AuthHeader != nil {
		every := cfg.authEvery
		if every <= 0 {
			every = time.Second
		}
		go cl.renewAuth(cfg.AuthHeader, auth.Clone(), every)
	}
	defer close(started)
	return cl, nil
}

// renewAuth asks auth for its headers every period until the client has stopped, and hands the
// core each change (last: a copy of what it has; auth may hand back one map, changed in place).
func (c *Client) renewAuth(auth func() http.Header, last http.Header, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		if h := auth(); !reflect.DeepEqual(h, last) {
			last = h.Clone()
			c.send(authInput(last))
		}
	}
}

// authInput hands the core the authentication headers h.
func authInput(h http.Header) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_SetAuthHeaders{SetAuthHeaders: &corepb.Headers{Headers: fromHTTP(h)}}}
}

// InstanceUID is the instance UID in use: the application's, one saved before a restart, one the
// core made, or one the server assigned since (InstanceUIDChanged).
func (c *Client) InstanceUID() [16]byte { return *c.uid.Load() }

// Endpoint is the endpoint the client uses now: where its latest OpAMP request or WebSocket
// connection went, which after the server moved the agent (an offer of connection settings) is
// the offered one, or the configured one again once an offer failed. It is "" before any outcome,
// and in core mode, where the core makes the requests itself (the configured one is all there is
// to name then).
// It holds the URL as the core gave it, credentials and query included: redact it before logging.
func (c *Client) Endpoint() string {
	if c.used == nil {
		return ""
	}
	if u := c.used.Load(); u != nil {
		return *u
	}
	return ""
}

func (c *Client) setUID(uid []byte) {
	if len(uid) != 16 {
		return
	}
	if cur := c.uid.Load(); cur != nil && [16]byte(uid) == *cur {
		return
	}
	u := [16]byte(uid)
	c.uid.Store(&u)
}

// SetDescription changes the agent description.
func (c *Client) SetDescription(d *protobufs.AgentDescription) {
	c.send(&corepb.Input{Kind: &corepb.Input_SetDescription{SetDescription: d}})
}

// SetHealth changes the health.
func (c *Client) SetHealth(h *protobufs.ComponentHealth) {
	c.send(&corepb.Input{Kind: &corepb.Input_SetHealth{SetHealth: h}})
}

// SetEffectiveConfig changes the effective config.
func (c *Client) SetEffectiveConfig(e *protobufs.EffectiveConfig) {
	c.send(&corepb.Input{Kind: &corepb.Input_SetEffectiveConfig{SetEffectiveConfig: e}})
}

// SetCustomCapabilities changes the custom capabilities the agent supports.
func (c *Client) SetCustomCapabilities(cc *protobufs.CustomCapabilities) {
	c.send(&corepb.Input{Kind: &corepb.Input_SetCustomCapabilities{SetCustomCapabilities: cc}})
}

// SendCustomMessage sends a custom message, for one of the agent's custom capabilities. It goes
// with the next message the server takes, and again if that message is lost; if it can't be sent
// at all, a CustomMessageNotSent notification says why.
func (c *Client) SendCustomMessage(m *protobufs.CustomMessage) {
	c.send(&corepb.Input{Kind: &corepb.Input_SendCustomMessage{SendCustomMessage: m}})
}

// SetAvailableComponents changes the components the agent has (leave Hash empty: the core computes it).
func (c *Client) SetAvailableComponents(a *protobufs.AvailableComponents) {
	c.send(&corepb.Input{Kind: &corepb.Input_SetAvailableComponents{SetAvailableComponents: a}})
}

// SetRemoteConfigStatus reports on a remote config (the answer to a RemoteConfig notification).
func (c *Client) SetRemoteConfigStatus(s *protobufs.RemoteConfigStatus) {
	c.send(&corepb.Input{Kind: &corepb.Input_SetRemoteConfigStatus{SetRemoteConfigStatus: s}})
}

// SetConnectionSettingsStatus reports on telemetry settings (the answer to a ConnectionSettings
// notification).
func (c *Client) SetConnectionSettingsStatus(s *protobufs.ConnectionSettingsStatus) {
	c.send(&corepb.Input{Kind: &corepb.Input_SetConnectionSettingsStatus{SetConnectionSettingsStatus: s}})
}

// PackageInstalled answers InstallPackage: the package at hash is installed (err nil), or why
// it isn't. An error is a failure for good (the version is reported failed and not tried again
// until another is offered) unless wrapped by FailedForNow or FailedLeavingUnknown.
func (c *Client) PackageInstalled(name string, hash []byte, err error) {
	c.send(&corepb.Input{Kind: &corepb.Input_PackageInstalled{PackageInstalled: packageInstalled(name, hash, err)}})
}

// PackageRemoved answers RemovePackage: the package is removed (err nil), or why it isn't. One
// that isn't is reported failed, and not removed again until it is offered and dropped again,
// unless its error is wrapped by FailedForNow or FailedLeavingUnknown.
func (c *Client) PackageRemoved(name string, err error) {
	c.send(&corepb.Input{Kind: &corepb.Input_PackageRemoved{PackageRemoved: packageRemoved(name, err)}})
}

func packageInstalled(name string, hash []byte, err error) *corepb.PackageInstalled {
	p := &corepb.PackageInstalled{Name: name, Hash: hash, Ok: err == nil}
	if err != nil {
		p.Detail, p.Failure = err.Error(), failureOf(err)
	}
	return p
}

func packageRemoved(name string, err error) *corepb.PackageRemoved {
	p := &corepb.PackageRemoved{Name: name, Failed: err != nil}
	if err != nil {
		p.Detail, p.Failure = err.Error(), failureOf(err)
	}
	return p
}

// packageFailure is an install's or a removal's error that says how it failed.
type packageFailure struct {
	error
	how corepb.PackageFailure
}

func (e packageFailure) Unwrap() error { return e.error }

// FailedForNow marks an install's or a removal's error (for PackageInstalled and PackageRemoved)
// as a failure for now that left what was there: an outage, a timeout. The package is not
// reported failed; it waits and is tried again, after 30 s, doubling each time in a row up to
// 10 minutes, while the other packages go on (a restart tries at once).
func FailedForNow(err error) error {
	return packageFailure{err, corepb.PackageFailure_PACKAGE_FAILURE_LATER}
}

// FailedLeavingUnknown marks an install's or a removal's error (for PackageInstalled and
// PackageRemoved) as a failure that can't tell what it left: a rollback that failed. It is
// reported failed, as for good, and what the agent has of the package is uncertain from then on:
// after a restart it is installed (or removed) again, whatever was saved.
func FailedLeavingUnknown(err error) error {
	return packageFailure{err, corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN}
}

// IsFailedForNow says whether err is marked FailedForNow (and not FailedLeavingUnknown).
func IsFailedForNow(err error) bool {
	return err != nil && failureOf(err) == corepb.PackageFailure_PACKAGE_FAILURE_LATER
}

// IsFailedLeavingUnknown says whether err is marked FailedLeavingUnknown.
func IsFailedLeavingUnknown(err error) bool {
	return err != nil && failureOf(err) == corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN
}

// failureOf is how err says it failed: for good unless marked. Of several marks (a join), what
// is unknown wins over what is for now.
func failureOf(err error) corepb.PackageFailure {
	how := corepb.PackageFailure_PACKAGE_FAILURE_FINAL
	var walk func(error)
	walk = func(e error) {
		switch x := e.(type) {
		case nil:
			return
		case packageFailure:
			if x.how == corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN || how == corepb.PackageFailure_PACKAGE_FAILURE_FINAL {
				how = x.how
			}
			walk(x.error)
		case interface{ Unwrap() []error }:
			for _, e := range x.Unwrap() {
				walk(e)
			}
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		}
	}
	walk(err)
	return how
}

// Stop sends agent_disconnect if a message can still be sent, then stops. It returns when the
// core says the client has stopped (within Config.StopTimeout; nothing the client started is
// still running then, but for a save already writing, which finishes by itself), or when ctx is
// done.
//
// Called from the notification callback, Stop asks for the stop and returns at once, without
// waiting (the client is waiting for the callback): the client stops once the callback has
// returned, and the Stopped notification says so. It returns nil there, or the error the client
// stopped with if it has already stopped (on the Stopped notification itself).
func (c *Client) Stop(ctx context.Context) error {
	c.send(&corepb.Input{Kind: &corepb.Input_Stop{Stop: &corepb.Empty{}}})
	if c.loopG.Load() == goid() {
		// From the callback: only the client's goroutine writes err, before telling Stopped.
		return c.err
	}
	// Stopped already: how it stopped, whatever ctx says (select picks at random among the
	// ready cases, so a ctx done too would be reported half the time instead).
	select {
	case <-c.done:
		return c.err
	default:
	}
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) send(in *corepb.Input) {
	select {
	case <-c.done: // stopped: nobody takes it
	default:
		c.cmds.put(in)
	}
}

// coreMode says which mode the client runs in (true: core mode), and refuses what the mode can't
// take. With no Mode it is core mode, unless the application brought its own HTTP client, which
// only platform mode can use. Nothing one mode alone honours is ignored: it is an error that says
// what to set instead.
func (cfg Config) coreMode() (bool, error) {
	switch cfg.Mode {
	case "":
		if cfg.HTTPClient != nil {
			return false, cfg.platformModeOnly()
		}
		return true, cfg.coreModeOnly()
	case ModePlatform:
		return false, cfg.platformModeOnly()
	case ModeCore:
		if cfg.HTTPClient != nil {
			return false, errors.New("opamp: HTTPClient in core mode: the core speaks HTTP itself over Go's connections; set TLS and Proxy instead, or leave Mode unset (an HTTPClient selects platform mode)")
		}
		return true, cfg.coreModeOnly()
	default:
		return false, fmt.Errorf("opamp: Mode %q: want ModeCore or ModePlatform", cfg.Mode)
	}
}

// platformModeOnly refuses what platform mode doesn't honour.
func (cfg Config) platformModeOnly() error {
	if cfg.NoSpliceDownloads {
		return errors.New("opamp: NoSpliceDownloads in platform mode: it is core mode's (net/http writes package files itself); leave it unset, or run in core mode (no HTTPClient, and Mode unset or ModeCore)")
	}
	return nil
}

// coreModeOnly refuses what core mode doesn't honour.
func (cfg Config) coreModeOnly() error {
	if cfg.TLS.Config != nil && cfg.TLS.Config.ServerName != "" {
		return errors.New("opamp: TLS.Config.ServerName in core mode: the core names the host of each handshake (the server's, a download's, an https:// proxy's), so one name can't stand for all; leave it empty, or set Mode to ModePlatform, where net/http verifies every server by it")
	}
	return nil
}

// connection makes what carries the client's requests from the TLS, Proxy and HTTPClient options.
func (cfg Config) connection() (*connection, error) {
	if cfg.HTTPClient == nil {
		return newConnection(cfg.TLS, cfg.Proxy)
	}
	if !cfg.TLS.isZero() || !cfg.Proxy.isZero() {
		return nil, errors.New("opamp: HTTPClient and TLS or Proxy: set HTTPClient's Transport instead, or leave HTTPClient nil")
	}
	conn := connectionFrom(cfg.HTTPClient)
	// A WebSocket endpoint (the core opens WebSocket connections for ws:// and wss://) needs an
	// *http.Transport to take its settings from.
	if strings.HasPrefix(strings.ToLower(cfg.Endpoint), "ws") && conn.wsErr != nil {
		return nil, conn.wsErr
	}
	return conn, nil
}

// coreConfig is the core's settings. The core takes the transport from the endpoint, keeps every
// effect's time (the client carries out its cancels), judges a download's status, and with no
// instance UID makes one from entropy. It makes the backoff's jitter seed from the same entropy
// (Seed left 0): two clients started in the same tick don't retry in step.
func coreConfig(cfg Config) (*corepb.Config, error) {
	var uid []byte
	if cfg.InstanceUID != ([16]byte{}) {
		uid = cfg.InstanceUID[:]
	}
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return nil, fmt.Errorf("opamp: the operating system's randomness: %w", err)
	}
	return &corepb.Config{
		InstanceUid:              uid,
		Entropy:                  entropy,
		UnixMs:                   unixMs(),
		Capabilities:             cfg.Capabilities,
		HeartbeatIntervalMs:      millis(cfg.HeartbeatInterval),
		Endpoint:                 cfg.Endpoint,
		Headers:                  fromHTTP(cfg.Header),
		RequestTimeoutMs:         millis(cfg.RequestTimeout),
		ConnectTimeoutMs:         millis(cfg.ConnectTimeout),
		StopTimeoutMs:            millis(cfg.StopTimeout),
		MaxSendBytes:             cfg.MaxSendBytes,
		MaxReceiveBytes:          cfg.MaxReceiveBytes,
		AcceptConnectionSettings: cfg.AcceptConnectionSettings,
		VerifyTimeoutMs:          millis(cfg.VerifyTimeout),
		AcceptPackages:           cfg.AcceptPackages,
		RequestInstanceUid:       cfg.RequestInstanceUID,
		MaxPackageBytes:          cfg.MaxPackageBytes,
		DownloadTimeoutMs:        millis(cfg.DownloadTimeout),
	}, nil
}

// millis is d in whole milliseconds for the core; a negative d is none, the core's default, as 0
// is (as the Node.js client takes it).
// millis is d in milliseconds for the core. A negative one reaches it as its two's complement,
// which the core refuses at init, naming the field and the number meant.
func millis(d time.Duration) uint64 { return uint64(d / time.Millisecond) }

func statusInputs(s Status) []*corepb.Input {
	var in []*corepb.Input
	if s.Description != nil {
		in = append(in, &corepb.Input{Kind: &corepb.Input_SetDescription{SetDescription: s.Description}})
	}
	if s.Health != nil {
		in = append(in, &corepb.Input{Kind: &corepb.Input_SetHealth{SetHealth: s.Health}})
	}
	if s.EffectiveConfig != nil {
		in = append(in, &corepb.Input{Kind: &corepb.Input_SetEffectiveConfig{SetEffectiveConfig: s.EffectiveConfig}})
	}
	if s.RemoteConfigStatus != nil {
		in = append(in, &corepb.Input{Kind: &corepb.Input_SetRemoteConfigStatus{SetRemoteConfigStatus: s.RemoteConfigStatus}})
	}
	if s.AvailableComponents != nil {
		in = append(in, &corepb.Input{Kind: &corepb.Input_SetAvailableComponents{SetAvailableComponents: s.AvailableComponents}})
	}
	if s.CustomCapabilities != nil {
		in = append(in, &corepb.Input{Kind: &corepb.Input_SetCustomCapabilities{SetCustomCapabilities: s.CustomCapabilities}})
	}
	return in
}

// errUnknownEffect: the module is newer than this client. Fail loudly rather than skip what the
// core asked for.
var errUnknownEffect = errors.New("opamp: the core asked for an effect this client doesn't know: the client is older than its core")

// known returns an error for an effect of a kind this client doesn't carry out.
func known(e *corepb.Effect) error {
	if e.Kind == nil {
		return errUnknownEffect
	}
	return nil
}

// starts says whether e opens something new: a connection, a download, a stream, a name to
// resolve.
func starts(e *corepb.Effect) bool {
	switch e.Kind.(type) {
	case *corepb.Effect_WsOpen, *corepb.Effect_Download, *corepb.Effect_StreamOpen, *corepb.Effect_StreamResolve:
		return true
	}
	return false
}

// loop owns the core. Everything reaches it as an input on one goroutine.
type loop struct {
	core   *core.Core
	x      *executor
	notify func(Notification)
	cmds   *mailbox[*corepb.Input]
	start  time.Time
	queue  []report
	uid    func([]byte) // told the instance UID in use after every step
	// stopBound bounds the wait, after the core has stopped, for what it started to end: the
	// core's stop timeout.
	stopBound time.Duration
	// stopBy: when Stop was asked, plus stopBound; Stop returns by then (zero: not asked).
	stopBy time.Time
	// saidStopped: the callback has been called with Stopped, the last word, said once.
	saidStopped bool
}

// stopGrace: once the core has stopped, at least this for the work it cancelled to unwind (a
// download's file deleted once the write in flight is over), however much of the stop timeout the
// core took: as every SDK. 250 ms was too little on a loaded runner (Java's, the nightly of
// 2026-10-23: a download's file deleted after stop had returned).
const stopGrace = time.Second

// shutdownBound is how long to wait, once the core has stopped, for what it started to end:
// what is left of Stop's bound (Stop returns within StopTimeout, though the core may have taken
// it all: a save it gave up on finishes by itself), but stopGrace; all of stopBound when the
// client stopped on its own.
func (l *loop) shutdownBound() time.Duration {
	if l.stopBy.IsZero() {
		return l.stopBound
	}
	return max(time.Until(l.stopBy), stopGrace)
}

func (l *loop) now() uint64 { return uint64(time.Since(l.start) / time.Millisecond) }

// untilWake is how long to wait from now until at (ms on the loop's clock), as far as a
// time.Duration goes: any uint64 is a valid wake_at_ms.
func untilWake(at, now uint64) time.Duration {
	return time.Duration(min(at-min(at, now), maxMillis)) * time.Millisecond
}

// tell calls the application's callback; a panic in it ends the client, with the reason.
func (l *loop) tell(n Notification) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("opamp: the notification callback panicked: %v", p)
		}
	}()
	if n.Kind == Stopped {
		l.saidStopped = true
	}
	l.notify(n)
	return nil
}

func (l *loop) run() error {
	ctx := context.Background()
	defer l.core.Close(ctx)
	defer func() { l.x.shutdown(l.shutdownBound()) }()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	var wake *uint64 // the wake_at_ms in force
	for {
		for len(l.queue) > 0 {
			r := l.queue[0]
			l.queue = l.queue[1:]
			// Nothing about an effect the core has cancelled, even if it came before the cancel.
			if !l.x.deliver(r) {
				r.settled()
				continue
			}
			if r.in.GetStop() != nil && l.stopBy.IsZero() {
				l.stopBy = time.Now().Add(l.stopBound)
			}
			out, err := l.core.Step(ctx, l.now(), unixMs(), r.in)
			if err != nil {
				r.settled()
				return err
			}
			if l.x.streams != nil {
				l.x.streams.reading = r.src
			}
			l.uid(out.InstanceUid)
			// stopped is told last: the core may emit a notification after it in the same output
			// (a remote config in the answer to the goodbye), and nothing reaches the application
			// after stopped (RULE-SHELL-01).
			var stopped *Notification
			// How much of the application's calls is the callback's own: as far as the last thing
			// a callback of this step asked for (0: none asked for anything).
			asked := 0
			for _, e := range out.Effects {
				if err := known(e); err != nil {
					return err
				}
				if n := e.GetNotify(); n != nil {
					note := fromCore(n)
					if note.Kind == unknownNotification {
						continue
					}
					if note.Kind == InstallPackage {
						note.Path = l.x.downloadPath(n.GetInstallPackage().Download)
					}
					if note.Kind == Stopped {
						stopped = &note
						continue
					}
					before := l.cmds.size()
					if err := l.tell(note); err != nil {
						return err
					}
					if size := l.cmds.size(); size > before {
						asked = size
					}
				} else if out.Stopped && starts(e) {
					// An output that says stopped starts nothing new (RULE-SHELL-07): what it
					// would start could only be given up on.
				} else if back := l.x.do(e); back != nil {
					l.queue = append(l.queue, *back)
				}
			}
			if l.x.streams != nil {
				l.x.streams.reading = nil
			}
			// What the application asked from inside the callback is its answer to this step: it
			// reaches the core before anything the client takes in next (RULE-SHELL-10). So its
			// calls are taken here as far as the callback's last one, in order (with what other
			// callers asked before it: none is passed over), and queued behind what is queued
			// already. Left to the select below, which picks at random among what is ready, they
			// waited behind a connection that opened while the callback ran: its first message
			// then said APPLYING for a saved config the callback had answered. Calls from
			// elsewhere keep their place: where no callback calls the client, the order is the
			// select's, as it was.
			for _, in := range l.cmds.first(asked) {
				l.queue = append(l.queue, report{in: in})
			}
			r.settled()
			if stopped != nil {
				if err := l.tell(*stopped); err != nil {
					return err
				}
			}
			timer.Stop()
			wake = out.WakeAtMs
			if wake != nil {
				timer.Reset(untilWake(*wake, l.now()))
			}
			// Nothing the core started is still running: release everything (shutdown).
			if out.Stopped {
				return nil
			}
		}
		select {
		case r := <-l.x.reports():
			l.queue = append(l.queue, r)
		case <-l.cmds.wake:
			for _, in := range l.cmds.take() {
				l.queue = append(l.queue, report{in: in})
			}
		case <-timer.C:
			// A wake-up further off than a timer holds: armed as far as it goes, and again.
			if now := l.now(); wake != nil && now < *wake {
				timer.Reset(untilWake(*wake, now))
				continue
			}
			l.queue = append(l.queue, report{in: &corepb.Input{Kind: &corepb.Input_Timer{Timer: &corepb.Empty{}}}})
		}
	}
}

// StateSealer encrypts and authenticates the state file (Config.StateSealer). Seal and Open
// may be called from any goroutine, one at a time. Open must refuse anything Seal didn't make
// with the same key, but may take a state saved before sealing was turned on, so that turning
// it on keeps what the agent has.
type StateSealer interface {
	// Seal returns state, encrypted.
	Seal(state []byte) ([]byte, error)
	// Open returns the state in a file Seal wrote.
	Open(file []byte) ([]byte, error)
}
