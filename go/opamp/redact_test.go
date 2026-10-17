// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"bytes"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Noninterference of secrets, as Python's tests/test_redaction.py and the core's own tests: two configurations that differ only in their secrets' values, each as long in
// both, look the same from outside. What an application logs of them (fmt's verbs, slog's text
// and JSON) is the same text. The secrets: the headers sent to the server, a password in the
// endpoint's or the proxy's URL, the proxy's CONNECT headers, and the client key.

// A secret's characters: those a URL's user-info holds as they are.
const secretChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~!$&'()*+,;="

func redactShown(token, password, proxyToken, key string) string {
	tls := TLS{CAPEM: []byte("CA"), CertPEM: []byte("CERT"), KeyPEM: []byte(key)}
	proxy := Proxy{URL: "http://agent:" + password + "@proxy.example:3128",
		ConnectHeader: http.Header{"Proxy-Authorization": {proxyToken}}}
	cfg := Config{Endpoint: "https://agent:" + password + "@opamp.example/v1/opamp",
		Header: http.Header{"Authorization": {token}}, TLS: tls, Proxy: proxy}
	var out strings.Builder
	for _, v := range []any{tls, proxy, cfg, &cfg} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x"} {
			fmt.Fprintf(&out, verb+"\n", v)
		}
		var logged bytes.Buffer
		slog.New(slog.NewTextHandler(&logged, nil)).Info("x", "v", v)
		slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "v", v)
		out.WriteString(strings.ReplaceAll(logged.String(), "time=", "\ntime="))
	}
	return out.String()
}

// stripTimes leaves out slog's time stamps, which differ between two runs.
func stripTimes(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "level="); strings.HasPrefix(line, "time=") && i > 0 {
			line = line[i:]
		}
		if i := strings.Index(line, `"level"`); strings.HasPrefix(line, `{"time"`) && i > 0 {
			line = line[i:]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// pair is two different secrets of one length.
func pair(r *rand.Rand) (string, string) {
	n := 1 + r.Intn(40)
	one := func() string {
		b := make([]byte, n)
		for i := range b {
			b[i] = secretChars[r.Intn(len(secretChars))]
		}
		return string(b)
	}
	a, b := one(), one()
	for b == a {
		b = one()
	}
	return a, b
}

func TestSecretsChangeNothingShown(t *testing.T) {
	r := rand.New(rand.NewSource(120))
	for range 200 {
		t1, t2 := pair(r)
		p1, p2 := pair(r)
		x1, x2 := pair(r)
		k1, k2 := pair(r)
		one, two := stripTimes(redactShown(t1, p1, x1, k1)), stripTimes(redactShown(t2, p2, x2, k2))
		if one != two {
			t.Fatalf("secrets are shown:\n%s\n---\n%s", one, two)
		}
	}
}

func TestWhatIsNotSecretIsStillShown(t *testing.T) {
	shown := redactShown("tok", "pw", "ptok", "key")
	for _, part := range []string{"opamp.example/v1/opamp", "Authorization", "Proxy-Authorization", "proxy.example:3128",
		"agent", "CERT", "KeyPEM"} {
		if !strings.Contains(shown, part) {
			t.Errorf("%q is not shown in\n%s", part, shown)
		}
	}
}

// Each type prints its exported fields, so a field added later is printed too, or this fails.
func TestEveryFieldIsPrinted(t *testing.T) {
	for _, v := range []any{TLS{}, Proxy{}, Config{}} {
		shown := fmt.Sprint(v)
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			if f := typ.Field(i); f.IsExported() && !strings.Contains(shown, f.Name+":") {
				t.Errorf("%s.%s is not printed: %s", typ.Name(), f.Name, shown)
			}
		}
	}
}
