// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opampotel_test

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/pydantic/opamp-clients/go/otel"
)

// An application reads a setting of its own, here a feature flag, by its key with a default. Until
// the server sends one (and without a server at all) the read is the default.
func ExampleAppSetting() {
	if opampotel.AppSetting("checkout.new_flow", "off") == "on" {
		fmt.Println("the new checkout flow")
	} else {
		fmt.Println("the old checkout flow")
	}
	// Output: the old checkout flow
}

// The typed reads take a value as every language does (docs/app-settings.md), and anything else,
// or no value, is the default: they never fail.
func ExampleAppSettingInt() {
	limit := opampotel.AppSettingInt("checkout.max_items", 50)
	ratio := opampotel.AppSettingFloat("checkout.discount", 0.1)
	verbose := opampotel.AppSettingBool("checkout.verbose", false)
	fmt.Println(limit, ratio, verbose)
	// Output: 50 0.1 false
}

// Every value in force, as a map of the caller's own.
func ExampleAppSettings() {
	for key, value := range opampotel.AppSettings() {
		fmt.Println(key, "=", value)
	}
	fmt.Println(len(opampotel.AppSettings()), "settings in force")
	// Output: 0 settings in force
}

// Told of each change, on the OpAMP client's goroutine: keep the function quick, and hand the
// work to the application's own goroutine, as here through a channel.
func ExampleOnAppSettingsChange() {
	sdk, err := opampotel.Setup(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer sdk.Shutdown(context.Background())

	changes := make(chan []string, 16)
	remove := opampotel.OnAppSettingsChange(func(settings map[string]string, changed []string) {
		select {
		case changes <- changed:
		default: // the application is behind: it reads what is in force anyway
		}
	})
	defer remove()

	level := new(slog.LevelVar)
	for range time.Tick(time.Second) {
		select {
		case changed := <-changes:
			slog.Info("settings changed", "keys", strings.Join(changed, ", "))
			// A key left out is back to its default.
			_ = level.UnmarshalText([]byte(opampotel.AppSetting("example.log_level", "INFO")))
		default:
		}
		if opampotel.AppSetting("example.feature", "off") == "on" {
			// ...
		}
	}
}
