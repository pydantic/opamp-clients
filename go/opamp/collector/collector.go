// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package collector is the OpAMP client of the Go Supervisor and the Collector's extension: package
// opamp's client, with what only they use on top. The server's packages (downloaded beside the
// state file, for the agent to install), custom messages both ways, the server's commands, and
// authentication headers an extension renews. An SDK takes package opamp's client, which does
// without them (docs/sdks.md).
//
// The client underneath is the same, on the same core: each feature here is turned on by its
// capability, which package opamp's client never declares.
package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
	"github.com/pydantic/opamp-clients/go/opamp/internal/plumb"
)

// Config is package opamp's Config with the Collector's settings.
type Config struct {
	opamp.Config
	// AuthHeader, if set, gives the headers an authentication extension adds to every request and
	// WebSocket upgrade: a token it renews, say. It is called once before the client starts, then
	// about every second on a goroutine of its own while the client runs; when what it gives
	// changes, the core takes it from the next request or connection on (a connection open keeps
	// its headers). Each header replaces the one of the same name in Header, or in connection
	// settings the server offered. The core checks every list it sends, as it checks Header: one
	// its rule refuses fails that attempt, and nothing is sent. Both modes.
	AuthHeader func() http.Header
	// AcceptPackages takes packages from the server (docs/packages.md): the client downloads
	// them to StateFile + ".downloads" and the application installs them (InstallPackage,
	// RemovePackage). Only with a StateFile: a restart would forget what the agent has.
	AcceptPackages bool
	// MaxPackageBytes is the largest package file downloaded; 0: 1 GiB.
	MaxPackageBytes uint64
	// DownloadTimeout is how long a download may take; 0: 10 minutes.
	DownloadTimeout time.Duration
	// NoSpliceDownloads, in core mode, passes package files' bytes through the core. Unset (the
	// default), a download's body goes from the connection straight to its file wherever the
	// core knows the next bytes are body (Config.splice_downloads). It is core mode's: in platform
	// mode it is an error.
	NoSpliceDownloads bool

	// authEvery, when set (by tests), is how often AuthHeader is asked again; 0: every second.
	authEvery time.Duration
}

// String is c without its secrets, as package opamp's Config prints.
func (c Config) String() string {
	return fmt.Sprintf("{Config:%s AuthHeader:%s AcceptPackages:%t MaxPackageBytes:%d DownloadTimeout:%s NoSpliceDownloads:%t}",
		c.Config.String(), typeOnly(c.AuthHeader), c.AcceptPackages, c.MaxPackageBytes, c.DownloadTimeout, c.NoSpliceDownloads)
}

// Format prints c as String does, whatever the verb.
func (c Config) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = io.WriteString(f, "collector.Config")
	}
	_, _ = io.WriteString(f, c.String())
}

// LogValue is what slog logs of c: String.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

func typeOnly(f func() http.Header) string {
	if f == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", f)
}

// Status is package opamp's Status with the agent's custom capabilities.
type Status struct {
	opamp.Status
	// CustomCapabilities: the custom capabilities the agent supports; custom messages for them
	// reach the application (CustomMessage), and it may send them.
	CustomCapabilities *protobufs.CustomCapabilities
}

// The Collector's notifications, past package opamp's.
const (
	// Command: a command from the server (Command).
	Command opamp.NotificationKind = plumb.KindCommand
	// CustomMessage: a custom message from the server, for a custom capability the agent
	// declared (CustomMessage).
	CustomMessage opamp.NotificationKind = plumb.KindCustomMessage
	// ServerCustomCapabilities: the custom capabilities the server supports changed
	// (CustomCapabilities).
	ServerCustomCapabilities opamp.NotificationKind = plumb.KindServerCustomCapabilities
	// CustomMessageNotSent: a custom message the application sent won't be (CustomMessage,
	// Detail: why).
	CustomMessageNotSent opamp.NotificationKind = plumb.KindCustomMessageNotSent
	// InstallPackage: install package PackageName (Package) from the file at Path; answer with
	// PackageInstalled. Checking the file's signature is the application's job; the client
	// deletes the file after the answer.
	InstallPackage opamp.NotificationKind = plumb.KindInstallPackage
	// RemovePackage: remove package PackageName, no longer offered; answer with PackageRemoved.
	RemovePackage opamp.NotificationKind = plumb.KindRemovePackage
)

// Notification is package opamp's Notification with what the Collector's carry.
type Notification struct {
	opamp.Notification
	// CustomMessage: the message (CustomMessage, CustomMessageNotSent).
	CustomMessage *protobufs.CustomMessage
	// CustomCapabilities: the server's (ServerCustomCapabilities).
	CustomCapabilities []string
	// Command: the server's command (Command).
	Command *protobufs.ServerToAgentCommand
	// PackageName: the package (InstallPackage, RemovePackage).
	PackageName string
	// Package: the package as offered: version, hash, type, and the file's signature (InstallPackage).
	Package *protobufs.PackageAvailable
	// Path: the downloaded file (InstallPackage).
	Path string
}

func notification(n opamp.Notification, raw *corepb.Notification, path string) Notification {
	out := Notification{Notification: n, Path: path}
	switch k := raw.GetKind().(type) {
	case *corepb.Notification_CustomMessage:
		out.CustomMessage = k.CustomMessage
	case *corepb.Notification_ServerCustomCapabilities:
		out.CustomCapabilities = k.ServerCustomCapabilities.Capabilities
	case *corepb.Notification_CustomMessageNotSent:
		out.CustomMessage = k.CustomMessageNotSent.Message
	case *corepb.Notification_InstallPackage:
		out.PackageName, out.Package = k.InstallPackage.Name, k.InstallPackage.Package
	case *corepb.Notification_RemovePackage:
		out.PackageName = k.RemovePackage.Name
	case *corepb.Notification_Command:
		out.Command = k.Command
	}
	return out
}

// Client is package opamp's Client, with the Collector's calls.
type Client struct {
	*opamp.Client
}

// Opened is a client that has read its state but sent nothing: Start it, or Close it.
type Opened struct {
	*opamp.Opened
	cfg Config
}

// Open is opamp.Open, with the Collector's settings.
func Open(ctx context.Context, cfg Config) (*Opened, error) {
	o, err := plumb.Open(ctx, cfg.Config, plumb.Options{
		AcceptPackages:    cfg.AcceptPackages,
		MaxPackageBytes:   cfg.MaxPackageBytes,
		DownloadTimeout:   cfg.DownloadTimeout,
		NoSpliceDownloads: cfg.NoSpliceDownloads,
	})
	if err != nil {
		return nil, err
	}
	return &Opened{Opened: o.(*opamp.Opened), cfg: cfg}, nil
}

// Start is opamp.Start, with the Collector's settings, status and notifications.
func Start(ctx context.Context, cfg Config, status Status, notify func(*Client, Notification)) (*Client, error) {
	o, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return o.Start(status, notify)
}

// Start starts the opened client, as opamp's Opened.Start does.
func (o *Opened) Start(status Status, notify func(*Client, Notification)) (*Client, error) {
	var first []*corepb.Input
	if status.CustomCapabilities != nil {
		first = append(first, plumb.CustomCapabilities(status.CustomCapabilities))
	}
	var auth http.Header
	if o.cfg.AuthHeader != nil {
		// The first request or connection has them.
		auth = o.cfg.AuthHeader()
		first = append(first, plumb.AuthHeaders(auth))
	}
	if notify == nil {
		notify = func(*Client, Notification) {}
	}
	// One Client for the run, whichever of notify and Start makes it first.
	var one atomic.Pointer[Client]
	wrap := func(c *opamp.Client) *Client {
		one.CompareAndSwap(nil, &Client{c})
		return one.Load()
	}
	started, err := plumb.Start(o.Opened, status.Status, first, func(c any, n any, raw *corepb.Notification, path string) {
		notify(wrap(c.(*opamp.Client)), notification(n.(opamp.Notification), raw, path))
	})
	if err != nil {
		return nil, err
	}
	cl := wrap(started.(*opamp.Client))
	if o.cfg.AuthHeader != nil {
		every := o.cfg.authEvery
		if every <= 0 {
			every = time.Second
		}
		go plumb.RenewAuth(cl.send, plumb.Done(cl.Client), o.cfg.AuthHeader, auth.Clone(), every)
	}
	return cl, nil
}

func (c *Client) send(in *corepb.Input) { plumb.Send(c.Client, in) }

// SetCustomCapabilities changes the custom capabilities the agent supports.
func (c *Client) SetCustomCapabilities(cc *protobufs.CustomCapabilities) {
	c.send(plumb.CustomCapabilities(cc))
}

// SendCustomMessage sends a custom message, for one of the agent's custom capabilities. It goes
// with the next message the server takes, and again if that message is lost; if it can't be sent
// at all, a CustomMessageNotSent notification says why.
func (c *Client) SendCustomMessage(m *protobufs.CustomMessage) {
	c.send(plumb.CustomMessage(m))
}

// PackageInstalled answers InstallPackage: the package at hash is installed (err nil), or why
// it isn't. An error is a failure for good (the version is reported failed and not tried again
// until another is offered) unless wrapped by FailedForNow or FailedLeavingUnknown.
func (c *Client) PackageInstalled(name string, hash []byte, err error) {
	c.send(plumb.PackageInstalled(name, hash, err))
}

// PackageRemoved answers RemovePackage: the package is removed (err nil), or why it isn't. One
// that isn't is reported failed, and not removed again until it is offered and dropped again,
// unless its error is wrapped by FailedForNow or FailedLeavingUnknown.
func (c *Client) PackageRemoved(name string, err error) {
	c.send(plumb.PackageRemoved(name, err))
}

// FailedForNow marks an install's or a removal's error (for PackageInstalled and PackageRemoved)
// as a failure for now that left what was there: an outage, a timeout. The package is not
// reported failed; it waits and is tried again, after 30 s, doubling each time in a row up to
// 10 minutes, while the other packages go on (a restart tries at once).
func FailedForNow(err error) error {
	return plumb.Marked(err, corepb.PackageFailure_PACKAGE_FAILURE_LATER)
}

// FailedLeavingUnknown marks an install's or a removal's error (for PackageInstalled and
// PackageRemoved) as a failure that can't tell what it left: a rollback that failed. It is
// reported failed, as for good, and what the agent has of the package is uncertain from then on:
// after a restart it is installed (or removed) again, whatever was saved.
func FailedLeavingUnknown(err error) error {
	return plumb.Marked(err, corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN)
}

// IsFailedForNow says whether err is marked FailedForNow (and not FailedLeavingUnknown).
func IsFailedForNow(err error) bool {
	return err != nil && plumb.FailureOf(err) == corepb.PackageFailure_PACKAGE_FAILURE_LATER
}

// IsFailedLeavingUnknown says whether err is marked FailedLeavingUnknown.
func IsFailedLeavingUnknown(err error) bool {
	return err != nil && plumb.FailureOf(err) == corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN
}
