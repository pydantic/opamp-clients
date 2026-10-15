// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
)

// What Config, TLS and Proxy print, with any of fmt's verbs or through slog: every exported
// field, but not the client key, the headers' values (Authorization, say) or a password in a URL,
// as Python's Config.__repr__ and .NET's options records. What a field holds that can hold a
// secret of its own (a *tls.Config's certificates, an *http.Client's transport, a StateSealer's
// key) is shown by its type only.

const redacted = "<redacted>"

// String is c without its secrets.
func (c Config) String() string {
	return fields(c, map[string]any{
		"Endpoint":    redactedURL(c.Endpoint),
		"Header":      redactedHeader(c.Header),
		"AuthHeader":  typeOnly(c.AuthHeader),
		"HTTPClient":  typeOnly(c.HTTPClient),
		"StateSealer": typeOnly(c.StateSealer),
	})
}

// Format prints c as String does, whatever the verb.
func (c Config) Format(f fmt.State, verb rune) { format(f, verb, "opamp.Config", c.String()) }

// LogValue is what slog logs of c: String.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// String is t without the client key.
func (t TLS) String() string {
	return fields(t, map[string]any{
		"CAPEM":   string(t.CAPEM),
		"CertPEM": string(t.CertPEM),
		"KeyPEM":  redactedBytes(t.KeyPEM),
		"Config":  typeOnly(t.Config),
	})
}

// Format prints t as String does, whatever the verb.
func (t TLS) Format(f fmt.State, verb rune) { format(f, verb, "opamp.TLS", t.String()) }

// LogValue is what slog logs of t: String.
func (t TLS) LogValue() slog.Value { return slog.StringValue(t.String()) }

// String is p without the URL's password or the CONNECT headers' values.
func (p Proxy) String() string {
	return fields(p, map[string]any{"URL": redactedURL(p.URL), "ConnectHeader": redactedHeader(p.ConnectHeader)})
}

// Format prints p as String does, whatever the verb.
func (p Proxy) Format(f fmt.State, verb rune) { format(f, verb, "opamp.Proxy", p.String()) }

// LogValue is what slog logs of p: String.
func (p Proxy) LogValue() slog.Value { return slog.StringValue(p.String()) }

// format writes shown, after the type's name for %#v.
func format(f fmt.State, verb rune, name, shown string) {
	if verb == 'v' && f.Flag('#') {
		_, _ = io.WriteString(f, name)
	}
	_, _ = io.WriteString(f, shown)
}

// fields prints v's exported fields as %+v does ({Name:value ...}), those in shown as given there.
func fields(v any, shown map[string]any) string {
	rv := reflect.ValueOf(v)
	var b strings.Builder
	b.WriteByte('{')
	for i := range rv.NumField() {
		f := rv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(' ')
		}
		value, ok := shown[f.Name]
		if !ok {
			value = rv.Field(i).Interface()
		}
		fmt.Fprintf(&b, "%s:%v", f.Name, value)
	}
	b.WriteByte('}')
	return b.String()
}

// redactedURL is raw with its password, if any, as xxxxx; one that doesn't parse is left out whole.
func redactedURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redacted
	}
	return u.Redacted()
}

// redactedHeader is h's names, each value left out: any may hold a credential.
func redactedHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for name := range h {
		out[name] = []string{redacted}
	}
	return out
}

// redactedBytes says whether there is a secret, not what it is.
func redactedBytes(b []byte) string {
	if b == nil {
		return "<nil>"
	}
	return redacted
}

// typeOnly is v's type, or <nil>.
func typeOnly(v any) string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return "<nil>"
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface, reflect.Map, reflect.Slice:
		if rv.IsNil() {
			return "<nil>"
		}
	}
	return fmt.Sprintf("%T", v)
}
