// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

// Package plumb is what package collector adds to package opamp's client: the Collector's
// features (packages, custom messages, server commands, authentication headers renewed), which
// an SDK's client does without. Package opamp sets the hooks at init; package collector calls
// them. The client underneath is one: the core does every feature, each turned on by its
// capability, and package opamp's client simply never declares these.
package plumb

import (
	"context"
	"net/http"
	"reflect"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// Options are the Collector's settings, on top of package opamp's Config.
type Options struct {
	AcceptPackages    bool
	MaxPackageBytes   uint64
	DownloadTimeout   time.Duration
	NoSpliceDownloads bool
}

// The kinds of the Collector's notifications: package opamp's NotificationKind, past its own.
const (
	KindCommand = 100 + iota
	KindCustomMessage
	KindServerCustomCapabilities
	KindCustomMessageNotSent
	KindInstallPackage
	KindRemovePackage
)

// Hooks, set by package opamp at init. Their any values are package opamp's types: Config,
// Status, *Opened, *Client and Notification.
var (
	// Open is opamp.Open with the Collector's options.
	Open func(ctx context.Context, cfg any, o Options) (any, error)
	// Start is (*opamp.Opened).Start with the inputs that go before the start (the custom
	// capabilities, the first authentication headers) and a notify told, besides package opamp's
	// Notification, the core's (for what that doesn't carry) and an InstallPackage's file.
	Start func(opened any, status any, first []*corepb.Input, notify func(c any, n any, raw *corepb.Notification, path string)) (any, error)
	// Send hands the client's core an input.
	Send func(c any, in *corepb.Input)
	// Done is closed once the client has stopped.
	Done func(c any) <-chan struct{}
	// Headers are h as the core takes them.
	Headers func(h http.Header) []*corepb.Header
)

// CustomCapabilities is the input that declares cc.
func CustomCapabilities(cc *protobufs.CustomCapabilities) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_SetCustomCapabilities{SetCustomCapabilities: cc}}
}

// CustomMessage is the input that sends m.
func CustomMessage(m *protobufs.CustomMessage) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_SendCustomMessage{SendCustomMessage: m}}
}

// AuthHeaders is the input that hands the core the authentication headers h.
func AuthHeaders(h http.Header) *corepb.Input {
	return &corepb.Input{Kind: &corepb.Input_SetAuthHeaders{SetAuthHeaders: &corepb.Headers{Headers: Headers(h)}}}
}

// RenewAuth asks auth for its headers every period until done, and sends each change (last: a
// copy of what the core has; auth may hand back one map, changed in place).
func RenewAuth(send func(*corepb.Input), done <-chan struct{}, auth func() http.Header, last http.Header, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if h := auth(); !reflect.DeepEqual(h, last) {
			last = h.Clone()
			send(AuthHeaders(last))
		}
	}
}

// PackageInstalled is the input that answers InstallPackage.
func PackageInstalled(name string, hash []byte, err error) *corepb.Input {
	p := &corepb.PackageInstalled{Name: name, Hash: hash, Ok: err == nil}
	if err != nil {
		p.Detail, p.Failure = err.Error(), FailureOf(err)
	}
	return &corepb.Input{Kind: &corepb.Input_PackageInstalled{PackageInstalled: p}}
}

// PackageRemoved is the input that answers RemovePackage.
func PackageRemoved(name string, err error) *corepb.Input {
	p := &corepb.PackageRemoved{Name: name, Failed: err != nil}
	if err != nil {
		p.Detail, p.Failure = err.Error(), FailureOf(err)
	}
	return &corepb.Input{Kind: &corepb.Input_PackageRemoved{PackageRemoved: p}}
}

// packageFailure is an install's or a removal's error that says how it failed.
type packageFailure struct {
	error
	how corepb.PackageFailure
}

func (e packageFailure) Unwrap() error { return e.error }

// Marked is err marked as failing how.
func Marked(err error, how corepb.PackageFailure) error { return packageFailure{err, how} }

// FailureOf is how err says it failed: for good unless marked. Of several marks (a join), what
// is unknown wins over what is for now.
func FailureOf(err error) corepb.PackageFailure {
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
