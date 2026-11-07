// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
	"github.com/pydantic/opamp-clients/go/opamp/internal/plumb"
)

// The hooks package collector builds the Collector's client from (internal/plumb).
func init() {
	plumb.Open = func(ctx context.Context, cfg any, o plumb.Options) (any, error) {
		c := cfg.(Config)
		c.plumbed = o
		return Open(ctx, c)
	}
	plumb.Start = func(opened any, status any, first []*corepb.Input, notify func(any, any, *corepb.Notification, string)) (any, error) {
		return opened.(*Opened).start(status.(Status), first, func(c *Client, n Notification) {
			notify(c, n, n.raw, n.path)
		})
	}
	plumb.Send = func(c any, in *corepb.Input) { c.(*Client).send(in) }
	plumb.Done = func(c any) <-chan struct{} { return c.(*Client).done }
	plumb.Headers = fromHTTP
}
