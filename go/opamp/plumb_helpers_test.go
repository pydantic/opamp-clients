// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
	"github.com/pydantic/opamp-clients/go/opamp/internal/plumb"
)

// What package collector adds (internal/plumb), for this package's tests of the client
// underneath: they drive packages, custom messages and commands as package collector does.

const (
	Command                  NotificationKind = plumb.KindCommand
	CustomMessage            NotificationKind = plumb.KindCustomMessage
	ServerCustomCapabilities NotificationKind = plumb.KindServerCustomCapabilities
	CustomMessageNotSent     NotificationKind = plumb.KindCustomMessageNotSent
	InstallPackage           NotificationKind = plumb.KindInstallPackage
	RemovePackage            NotificationKind = plumb.KindRemovePackage
)

// collectorNote is what a notification of the Collector's carries (package collector's
// Notification has the same fields).
type collectorNote struct {
	CustomMessage      *protobufs.CustomMessage
	CustomCapabilities []string
	Command            *protobufs.ServerToAgentCommand
	PackageName        string
	Package            *protobufs.PackageAvailable
	Path               string
}

// x is what n carries of the Collector's.
func (n Notification) x() collectorNote {
	out := collectorNote{Path: n.path}
	switch k := n.raw.GetKind().(type) {
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

func (c *Client) SetCustomCapabilities(cc *protobufs.CustomCapabilities) {
	c.send(plumb.CustomCapabilities(cc))
}

func (c *Client) SendCustomMessage(m *protobufs.CustomMessage) { c.send(plumb.CustomMessage(m)) }

func (c *Client) PackageInstalled(name string, hash []byte, err error) {
	c.send(plumb.PackageInstalled(name, hash, err))
}

func (c *Client) PackageRemoved(name string, err error) { c.send(plumb.PackageRemoved(name, err)) }

func FailedForNow(err error) error {
	return plumb.Marked(err, corepb.PackageFailure_PACKAGE_FAILURE_LATER)
}

func FailedLeavingUnknown(err error) error {
	return plumb.Marked(err, corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN)
}

// startFull starts a client as package collector does: the Collector's notifications are told
// too, and the custom capabilities cc (at most one) declared.
func startFull(ctx context.Context, cfg Config, status Status, notify func(*Client, Notification), cc ...*protobufs.CustomCapabilities) (*Client, error) {
	o, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return o.startFull(status, notify, cc...)
}

// startFull is Opened.Start as package collector's.
func (o *Opened) startFull(status Status, notify func(*Client, Notification), cc ...*protobufs.CustomCapabilities) (*Client, error) {
	if notify == nil {
		notify = func(*Client, Notification) {}
	}
	var first []*corepb.Input
	for _, c := range cc {
		first = append(first, plumb.CustomCapabilities(c))
	}
	return o.start(status, first, notify)
}

// startWith is startFull declaring the custom capabilities cc.
func startWith(cc *protobufs.CustomCapabilities) func(context.Context, Config, Status, func(*Client, Notification)) (*Client, error) {
	return func(ctx context.Context, cfg Config, status Status, notify func(*Client, Notification)) (*Client, error) {
		return startFull(ctx, cfg, status, notify, cc)
	}
}
