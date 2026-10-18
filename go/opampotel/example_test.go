// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel_test

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/pydantic/opamp-clients/go/opampotel"
)

// The OpAMP server given in code: each option wins over its OTEL_OPAMP_* variable, and headers
// given replace OTEL_OPAMP_HEADERS. An endpoint given turns the package on without
// OTEL_OPAMP_ENDPOINT.
func ExampleSetup_inCode() {
	ctx := context.Background()
	sdk, err := opampotel.Setup(ctx,
		opampotel.WithEndpoint("https://opamp.example.com/v1/opamp"),
		opampotel.WithHeaders(map[string]string{"Authorization": "Bearer " + os.Getenv("CHECKOUT_OPAMP_KEY")}),
		opampotel.WithStateDir("/var/lib/checkout/opamp"), // or "none": a new instance at each start
		opampotel.WithHeartbeatInterval(30*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer sdk.Shutdown(ctx) // the server hears the application go, then the SDK flushes
}

// The application's own flags: an option left blank is as if not given, so a flag left unset
// falls back to its OTEL_OPAMP_* variable, and the flags can be passed as they are.
func ExampleSetup_fromFlags() {
	endpoint := flag.String("opamp", "", "the OpAMP server (default: OTEL_OPAMP_ENDPOINT)")
	heartbeat := flag.Duration("heartbeat", 0, "how often to check in (default: OTEL_OPAMP_HEARTBEAT_INTERVAL)")
	flag.Parse()

	ctx := context.Background()
	sdk, err := opampotel.Setup(ctx, opampotel.WithEndpoint(*endpoint), opampotel.WithHeartbeatInterval(*heartbeat))
	if err != nil {
		log.Fatal(err)
	}
	defer sdk.Shutdown(ctx)
}
