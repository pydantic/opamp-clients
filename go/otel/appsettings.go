// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opampotel

import (
	"fmt"
	"log/slog"
	"maps"
	"math"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The application's own settings, as the OpAMP server sends them (docs/app-settings.md): the
// custom map of the remote config's elastic entry, string keys to string values, which the SDK
// doesn't read but hands on for the application to read here.

// AppSetting is the value of the application setting key in force, or def while there is none:
// before the first config arrives (unless one was saved before a restart), when the config in
// force leaves the key out, before Setup and without an OpAMP server at all. Safe from any
// goroutine; it never fails.
func AppSetting(key, def string) string { return theAppSettings.get(key, def) }

// AppSettingBool reads the application setting key as `true` or `false` in any ASCII case,
// spaces round it allowed; def for anything else (`on`, `1` and `yes` too) and while there is none.
func AppSettingBool(key string, def bool) bool { return theAppSettings.getBool(key, def) }

// AppSettingInt reads the application setting key as a decimal integer (a sign and leading zeros
// allowed, spaces round it) in the int64 range; def for anything else and while there is none.
func AppSettingInt(key string, def int64) int64 { return theAppSettings.getInt(key, def) }

// AppSettingFloat reads the application setting key as a number as JSON writes one (a leading
// `+` allowed, spaces round it), finite as a float64; def for anything else and while there is none.
func AppSettingFloat(key string, def float64) float64 { return theAppSettings.getFloat(key, def) }

// AppSettings is every application setting in force: a copy of its own, which no later change
// alters and which the caller may change. Empty while there are none.
func AppSettings() map[string]string { return theAppSettings.snapshot() }

// OnAppSettingsChange calls f after each change to the application settings is put in force,
// with the settings now in force (a copy of f's own) and the keys added, changed or removed,
// sorted. It isn't called for the values in force when it is added (read those with AppSettings),
// nor when a config leaves them as they were (the same map again, another setting changed, a
// FAILED config).
//
// Changes are told one at a time, in the order they happened, to each function in the order it
// was added; AppSetting already returns the new values inside f. f runs on the integration's
// goroutine (the OpAMP client's, or Setup's for the settings saved before a restart), so it
// should be quick: hand longer work to the application's own goroutines, and don't call
// SDK.Shutdown from it. A panic in f is recovered and logged as an error; the other functions are
// still called and the config is still APPLIED.
//
// It returns a function that removes f; calling that again does nothing. f may add or remove
// functions, itself included, while it runs: that takes effect from the next change. Safe from
// any goroutine.
func OnAppSettingsChange(f func(settings map[string]string, changed []string)) (remove func()) {
	return theAppSettings.onChange(f)
}

// theAppSettings are the package's: what Setup's agent puts in force, and what the functions read.
var theAppSettings = newAppSettings()

// appSettings are application settings in force, and who to tell when they change.
type appSettings struct {
	values  atomic.Pointer[map[string]string] // never changed once stored; never nil
	telling sync.Mutex                        // held while a change is put in force and told
	mu      sync.Mutex                        // callbacks
	subs    []*appSub
}

// appSub is a function as added: each addition its own, so removing one leaves the same function
// added again.
type appSub struct {
	f func(map[string]string, []string)
}

func newAppSettings() *appSettings {
	a := &appSettings{}
	a.values.Store(&map[string]string{})
	return a
}

func (a *appSettings) get(key, def string) string {
	if v, ok := (*a.values.Load())[key]; ok {
		return v
	}
	return def
}

// typedSpaces are the spaces a typed read takes round a value: every language trims these, and
// only these.
const typedSpaces = " \t\r\n"

// text is the value of key, spaces round it trimmed, and whether there is one.
func (a *appSettings) text(key string) (string, bool) {
	v, ok := (*a.values.Load())[key]
	return strings.Trim(v, typedSpaces), ok
}

func (a *appSettings) getBool(key string, def bool) bool {
	v, ok := a.text(key)
	if !ok {
		return def
	}
	return parseAppBool(v, def)
}

func (a *appSettings) getInt(key string, def int64) int64 {
	v, ok := a.text(key)
	if !ok {
		return def
	}
	return parseAppInt(v, def)
}

func (a *appSettings) getFloat(key string, def float64) float64 {
	v, ok := a.text(key)
	if !ok {
		return def
	}
	return parseAppFloat(v, def)
}

// parseAppBool is `true` or `false` in any ASCII case (strings.EqualFold folds Unicode too: "ſ"
// is no "s"), else def.
func parseAppBool(v string, def bool) bool {
	switch asciiLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	return def
}

func asciiLower(v string) string {
	b := []byte(v)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

var (
	appInt = regexp.MustCompile(`^[+-]?[0-9]+$`)
	// A JSON number, a leading + too: what every language reads the same way.
	appFloat = regexp.MustCompile(`^[+-]?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
)

// parseAppInt is decimal digits with an optional sign in the int64 range (no `_`, no hex), else def.
func parseAppInt(v string, def int64) int64 {
	if !appInt.MatchString(v) {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// parseAppFloat is a JSON number with an optional leading +, finite as a float64, else def.
func parseAppFloat(v string, def float64) float64 {
	if !appFloat.MatchString(v) {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return def
	}
	return f
}

func (a *appSettings) snapshot() map[string]string { return maps.Clone(*a.values.Load()) }

func (a *appSettings) onChange(f func(map[string]string, []string)) func() {
	if f == nil {
		return func() {}
	}
	sub := &appSub{f}
	a.mu.Lock()
	a.subs = append(a.subs, sub)
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if i := slices.Index(a.subs, sub); i >= 0 {
			// A new slice: a change being told goes on with the list it took.
			a.subs = slices.Delete(slices.Clone(a.subs), i, i+1)
		}
	}
}

// put puts values in force (the layer's Settings.custom; nil: none), and tells the functions if
// anything changed, on the calling goroutine.
func (a *appSettings) put(values map[string]string) {
	a.telling.Lock()
	defer a.telling.Unlock()
	old := *a.values.Load()
	changed := changedKeys(old, values)
	if len(changed) == 0 {
		return
	}
	now := maps.Clone(values)
	if now == nil {
		now = map[string]string{}
	}
	a.values.Store(&now)
	slog.Info("opamp: application settings changed: " + strings.Join(changed, ", "))
	a.mu.Lock()
	subs := a.subs
	a.mu.Unlock()
	for _, sub := range subs {
		tell(sub.f, maps.Clone(now), slices.Clone(changed))
	}
}

// changedKeys are the keys added, changed or removed between old and new, sorted.
func changedKeys(old, new map[string]string) []string {
	var changed []string
	for k, v := range old {
		if w, ok := new[k]; !ok || w != v {
			changed = append(changed, k)
		}
	}
	for k := range new {
		if _, ok := old[k]; !ok {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}

// tell calls f, a panic in it recovered and logged: the application's bug, and the client goes on.
func tell(f func(map[string]string, []string), settings map[string]string, changed []string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("opamp: an application settings callback panicked", "panic", fmt.Sprint(r),
				"changed", changed, "stack", string(debug.Stack()))
		}
	}()
	f(settings, changed)
}
