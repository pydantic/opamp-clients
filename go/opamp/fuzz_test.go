// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package opamp

// Fuzz tests (go test -fuzz) of the client's own code around the core, where it takes what it
// doesn't control: what a server sends back (headers, a redirect's Location, bodies), the core's
// outputs (decoded and carried out, effect by effect, in both modes), the environment's proxies,
// and the disk (faults injected into every file operation). Each checks properties, not just that
// nothing panics: everything handed to the core can be encoded for it and names an effect it asked
// for, a request goes to exactly the URL the core gave with exactly its headers, a file is only
// ever written under the state file's directory, a save is all or nothing, a download's file holds
// what was written and its digest says so, and conversions to and from the core's messages lose
// nothing. The seeds run with every go test; scripts/go_fuzz.sh fuzzes each for a while.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// inputID is the id an input names (the effect it answers), if its kind has one.
func inputID(in *corepb.Input) (uint64, bool) {
	m := in.ProtoReflect()
	fd := m.WhichOneof(m.Descriptor().Oneofs().ByName("kind"))
	if fd == nil || fd.Message() == nil {
		return 0, false
	}
	inner := m.Get(fd).Message()
	idf := inner.Descriptor().Fields().ByName("id")
	if idf == nil || idf.Kind() != protoreflect.Uint64Kind {
		return 0, false
	}
	return inner.Get(idf).Uint(), true
}

// fuzzHeaders reads headers from b: a line each, the name up to the first ':', the value after it.
func fuzzHeaders(b []byte) []*corepb.Header {
	var hs []*corepb.Header
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		name, value, _ := strings.Cut(line, ":")
		hs = append(hs, &corepb.Header{Name: name, Value: value})
	}
	return hs
}

// byCanonicalName groups headers' values by the name net/http gives them, in order.
func byCanonicalName(hs []*corepb.Header) map[string][]string {
	m := map[string][]string{}
	for _, h := range hs {
		k := http.CanonicalHeaderKey(h.Name)
		m[k] = append(m[k], h.Value)
	}
	return m
}

// A redirect's Location: given back as given (as UTF-8) when it doesn't resolve, and otherwise an
// absolute URL whose own scheme, host and port are the origin reported with it, and which resolves
// to itself again: the core judges the place the client would go.
func FuzzRedirectLocation(f *testing.F) {
	base := "https://files.example.com:8443/pkgs/v1/agent.tar.gz?sig=1"
	for _, given := range []string{"", "agent-2.tar.gz", "../v2/./agent.tar.gz", "/a/b/../c",
		"//mirror.example.org/agent.tar.gz", "http://mirror.example.org:80/x", "http://[::1]:9000/x",
		"https://mirror.example.org:443/x", "http://[bad", "http://host:99999/x", "HTTP://EXAMPLE.com:0080/",
		"http://[fe80::1%25eth0]/", "http://user:pass@host/", "\xe9:/x", "?q", "#f", "http://h:/", "ws://h:80/"} {
		f.Add(base, given)
	}
	f.Add("http://127.0.0.1/a/b", "//x:65535")
	f.Fuzz(func(t *testing.T, requested, given string) {
		req, err := url.Parse(requested)
		// What was requested is a URL net/http sent: http or https, with a host.
		if err != nil || (req.Scheme != "http" && req.Scheme != "https") || req.Host == "" {
			t.Skip()
		}
		got, origin := location(req, given)
		if !utf8.ValidString(got) {
			t.Fatalf("location %q isn't UTF-8", got)
		}
		if given == "" {
			if got != "" || origin != nil {
				t.Fatalf("no Location: %q %v", got, origin)
			}
			return
		}
		if origin == nil {
			if got != lossy(given) {
				t.Fatalf("unresolved %q came back as %q", given, got)
			}
			return
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%q resolved to %q, which doesn't parse: %v", given, got, err)
		}
		if !u.IsAbs() || u.Scheme != origin.Scheme || u.Hostname() != origin.Host {
			t.Fatalf("%q resolved to %q (scheme %q host %q), reported as %v", given, got, u.Scheme, u.Hostname(), origin)
		}
		defaults := map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21"}
		switch p := u.Port(); {
		case origin.Port == 0:
			if p != "" && strings.TrimLeft(p, "0") != strings.TrimLeft(defaults[u.Scheme], "0") {
				t.Fatalf("%q: port %q, reported as the default", got, p)
			}
		default:
			n, err := strconv.ParseUint(p, 10, 16)
			if err != nil || uint32(n) != origin.Port {
				t.Fatalf("%q: port %q, reported as %d", got, p, origin.Port)
			}
		}
		again, origin2 := location(req, got)
		if again != got || !proto.Equal(origin, origin2) {
			t.Fatalf("%q resolved to %q %v, and that to %q %v", given, got, origin, again, origin2)
		}
	})
}

// Headers to and from net/http: the core's headers become a request's, every value kept in order
// under the name net/http gives it; a response's reach the core in a fixed order (by name), UTF-8,
// each value once; and going round again changes nothing.
func FuzzHeaderConversion(f *testing.F) {
	f.Add([]byte("Content-Type:application/x-protobuf\nauthorization:Bearer x\nAuthorization:second\nx-b:1\nX-A:2"))
	f.Add([]byte("bad name:v\n:empty\nnovalue\nX-Odd:caf\xe9\n\xff:\xfe"))
	f.Add([]byte("Set-Cookie:a=1\nSet-Cookie:b=2\nset-cookie:c=3"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		hs := fuzzHeaders(raw)
		h := toHTTP(hs)
		if want := byCanonicalName(hs); len(want) != len(h) {
			t.Fatalf("names %v, want %v", h, want)
		} else {
			for k, vs := range want {
				if !slices.Equal(h[k], vs) {
					t.Fatalf("%q: %q, want %q", k, h[k], vs)
				}
			}
		}
		back := fromHTTP(h)
		n := 0
		for _, vs := range h {
			n += len(vs)
		}
		if len(back) != n {
			t.Fatalf("%d headers back from %d values", len(back), n)
		}
		lossless := true
		for _, x := range hs {
			lossless = lossless && utf8.ValidString(x.Name) && utf8.ValidString(x.Value)
		}
		// (In order of the names as they came: one that isn't UTF-8 sorts by its own bytes.)
		if lossless && !slices.IsSortedFunc(back, func(a, b *corepb.Header) int { return strings.Compare(a.Name, b.Name) }) {
			t.Fatalf("not in order: %v", back)
		}
		if _, err := proto.Marshal(&corepb.HttpResponse{Headers: back}); err != nil {
			t.Fatalf("can't reach the core: %v", err)
		}
		if again := fromHTTP(toHTTP(back)); lossless && !slices.EqualFunc(again, back, func(a, b *corepb.Header) bool { return proto.Equal(a, b) }) {
			t.Fatalf("round again: %v, was %v", again, back)
		}
		for k, vs := range byCanonicalName(back) {
			if lossless && !slices.Equal(vs, h[k]) {
				t.Fatalf("%q: back as %q, sent %q", k, vs, h[k])
			}
		}
	})
}

// Rust mode's proxy rule, against what it promises: under CGI no proxy; otherwise one of the
// variables' values, trimmed; NO_PROXY's entries only ever send hosts direct (an entry added to
// the list never brings a proxy back), a host its own entry and * (for names) are direct, case
// doesn't matter, and the answer can be handed to the core.
func FuzzRustProxyRule(f *testing.F) {
	f.Add("localhost,.example.com,10.0.0.0/8, 2001:db8::/32 ,*", "api.example.com", "HTTP_PROXY=http://p:3128\nNO_PROXY=x", "http")
	f.Add("example.com", "EXAMPLE.COM", "https_proxy= socks5://u:p@p:1080 \nALL_PROXY=http://all", "wss")
	f.Add("::1,127.0.0.1/32,fe80::1%eth0", "::1", "ALL_PROXY=p\nREQUEST_METHOD=", "ws")
	f.Add(".", "a.", "HTTPS_PROXY=\xe9", "https")
	f.Fuzz(func(t *testing.T, list, host, envBlob, scheme string) {
		if strings.ContainsAny(host, "/?#@[]") || strings.Contains(host, ",") {
			t.Skip()
		}
		env := map[string]string{}
		for _, line := range strings.Split(envBlob, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				env[k] = v
			}
		}
		if list != "" {
			env["NO_PROXY"] = list
		}
		lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

		direct := rustNoProxy(list, host)
		if !direct && (rustNoProxy(list+","+host, host) != true || rustNoProxy(host+","+list, host) != true) && strings.TrimSpace(host) == host && host != "" {
			t.Fatalf("NO_PROXY %q plus the host %q itself doesn't send it direct", list, host)
		}
		if direct {
			for _, more := range []string{"x.invalid", "10.1.2.3", "::/0", "", "*"} {
				if !rustNoProxy(list+","+more, host) || !rustNoProxy(more+","+list, host) {
					t.Fatalf("adding %q to %q brought a proxy back for %q", more, list, host)
				}
			}
		}
		if rustNoProxy(list, swapASCIICase(host)) != direct || rustNoProxy(swapASCIICase(list), host) != direct {
			t.Fatalf("NO_PROXY %q for %q depends on case", list, host)
		}
		if ip, err := netip.ParseAddr(host); (err != nil || ip.Zone() != "") && !rustNoProxy("*", host) {
			t.Fatalf("* doesn't send the name %q direct", host)
		}

		raw := scheme + "://" + host + "/v1/opamp"
		got := rustEnvProxy(lookup, raw)
		if _, cgi := env["REQUEST_METHOD"]; cgi && got != "" {
			t.Fatalf("under CGI the proxy is %q", got)
		}
		if got != "" {
			ok := false
			for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
				ok = ok || (env[k] != "" && strings.TrimSpace(env[k]) == got)
			}
			if !ok || strings.TrimSpace(got) != got {
				t.Fatalf("proxy %q is none of the environment's (%v)", got, env)
			}
			if u, err := url.Parse(raw); err == nil && rustNoProxy(list, u.Hostname()) {
				t.Fatalf("NO_PROXY %q names %q, but the proxy is %q", list, u.Hostname(), got)
			}
		}
		in := newRustProxies(Proxy{}, lookup).answer(9, raw)
		if _, err := proto.Marshal(in); err != nil {
			t.Fatalf("the answer can't reach the core: %v", err)
		}
		if in.GetProxyAnswer().GetId() != 9 || in.GetProxyAnswer().GetProxy() != lossy(got) {
			t.Fatalf("answer %v for proxy %q", in, got)
		}
	})
}

func swapASCIICase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 32
		case 'A' <= c && c <= 'Z':
			b[i] = c + 32
		}
	}
	return string(b)
}

// toCore is the core's notification that n was made from, as far as n says (the inverse of
// fromCore): an install's download isn't in the notification (the loop puts its path in).
func toCore(n Notification) *corepb.Notification {
	var k any
	switch n.Kind {
	case Connected:
		k = &corepb.Notification_Connected{Connected: &corepb.Empty{}}
	case ConnectFailed:
		f := &corepb.NotifyConnectFailed{Detail: n.Detail}
		if n.Status != 0 {
			s := uint32(n.Status)
			f.Status = &s
		}
		k = &corepb.Notification_ConnectFailed{ConnectFailed: f}
	case ErrorResponse:
		k = &corepb.Notification_ErrorResponse{ErrorResponse: &corepb.ErrorResponse{Kind: int32(n.ErrorType), Message: n.Message}}
	case RemoteConfig:
		k = &corepb.Notification_RemoteConfig{RemoteConfig: n.RemoteConfig}
	case CustomMessage:
		k = &corepb.Notification_CustomMessage{CustomMessage: n.CustomMessage}
	case ServerCustomCapabilities:
		k = &corepb.Notification_ServerCustomCapabilities{ServerCustomCapabilities: &protobufs.CustomCapabilities{Capabilities: n.CustomCapabilities}}
	case CustomMessageNotSent:
		k = &corepb.Notification_CustomMessageNotSent{CustomMessageNotSent: &corepb.CustomMessageNotSent{Message: n.CustomMessage, Reason: n.Detail}}
	case InstallPackage:
		k = &corepb.Notification_InstallPackage{InstallPackage: &corepb.InstallPackage{Name: n.PackageName, Package: n.Package}}
	case RemovePackage:
		k = &corepb.Notification_RemovePackage{RemovePackage: &corepb.Name{Name: n.PackageName}}
	case ConnectionSettings:
		k = &corepb.Notification_ConnectionSettings{ConnectionSettings: n.ConnectionSettings}
	case Command:
		k = &corepb.Notification_Command{Command: n.Command}
	case InstanceUIDChanged:
		k = &corepb.Notification_InstanceUidChanged{InstanceUidChanged: slices.Clone(n.InstanceUID[:])}
	case HeartbeatInterval:
		k = &corepb.Notification_HeartbeatIntervalMs{HeartbeatIntervalMs: uint64(n.HeartbeatInterval / time.Millisecond)}
	case StatusTooLarge:
		k = &corepb.Notification_StatusTooLarge{StatusTooLarge: &corepb.StatusTooLarge{Field: corepb.StatusField(n.Field), Bytes: uint64(n.Bytes), Name: n.FieldName}}
	case BadMessage:
		k = &corepb.Notification_BadMessage{BadMessage: &corepb.Detail{Detail: n.Detail}}
	case SaveStalled:
		k = &corepb.Notification_SaveStalled{SaveStalled: &corepb.SaveStalled{Stalled: n.Stalled, WaitedMs: uint64(n.Waited / time.Millisecond)}}
	case Stopped:
		k = &corepb.Notification_Stopped{Stopped: &corepb.Empty{}}
	default:
		return &corepb.Notification{}
	}
	return setKind(&corepb.Notification{}, k)
}

// The core's notifications, read: whatever kind comes (any bytes the core could send), what the
// application is told says the same thing (the core's notification can be made again from it),
// with values past a Go type saturated; Stopped only for the core's Stopped, and a kind this
// client doesn't know never told at all.
func FuzzNotificationFromCore(f *testing.F) {
	seeds := []*corepb.Notification{
		{Kind: &corepb.Notification_Connected{Connected: &corepb.Empty{}}},
		{Kind: &corepb.Notification_ConnectFailed{ConnectFailed: &corepb.NotifyConnectFailed{Status: proto.Uint32(503), Detail: "busy"}}},
		{Kind: &corepb.Notification_ErrorResponse{ErrorResponse: &corepb.ErrorResponse{Kind: -5, Message: "m"}}},
		{Kind: &corepb.Notification_HeartbeatIntervalMs{HeartbeatIntervalMs: math.MaxUint64}},
		{Kind: &corepb.Notification_StatusTooLarge{StatusTooLarge: &corepb.StatusTooLarge{Field: 99, Bytes: math.MaxUint64, Name: "health"}}},
		{Kind: &corepb.Notification_InstanceUidChanged{InstanceUidChanged: bytes.Repeat([]byte{7}, 16)}},
		{Kind: &corepb.Notification_InstallPackage{InstallPackage: &corepb.InstallPackage{Name: "p", Download: 4,
			Package: &protobufs.PackageAvailable{Version: "1", Hash: []byte{1}}}}},
		{Kind: &corepb.Notification_Stopped{Stopped: &corepb.Empty{}}},
		{Kind: &corepb.Notification_SaveStalled{SaveStalled: &corepb.SaveStalled{Stalled: true, WaitedMs: math.MaxUint64}}},
		{Kind: &corepb.Notification_ServerCustomCapabilities{ServerCustomCapabilities: &protobufs.CustomCapabilities{Capabilities: []string{"a", "b"}}}},
		{Kind: &corepb.Notification_CustomMessageNotSent{CustomMessageNotSent: &corepb.CustomMessageNotSent{
			Message: &protobufs.CustomMessage{Capability: "c", Type: "t", Data: []byte{1}}, Reason: "r"}}},
		{},
	}
	for _, s := range seeds {
		b, _ := proto.Marshal(s)
		f.Add(b)
	}
	f.Add([]byte{0x9a, 0x01, 0x00}) // field 19: a kind from a newer core
	f.Fuzz(func(t *testing.T, raw []byte) {
		orig := &corepb.Notification{}
		if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, orig); err != nil {
			t.Skip()
		}
		n := fromCore(orig)
		if (n.Kind == Stopped) != (orig.GetStopped() != nil) {
			t.Fatalf("%v read as %v", orig, n.Kind)
		}
		if (n.Kind == unknownNotification) != (orig.Kind == nil) {
			t.Fatalf("%v read as %v", orig, n.Kind)
		}
		if n.HeartbeatInterval < 0 || n.Bytes < 0 {
			t.Fatalf("%v read as negative: %v", orig, n)
		}
		if n.Kind == unknownNotification {
			return
		}
		// What the notification says, normalized as fromCore promises: values saturated, a UID of
		// 16 bytes, no status for a status of 0, and no download (the loop gives its path).
		want := proto.Clone(orig).(*corepb.Notification)
		switch k := want.Kind.(type) {
		case *corepb.Notification_HeartbeatIntervalMs:
			k.HeartbeatIntervalMs = min(k.HeartbeatIntervalMs, maxMillis)
		case *corepb.Notification_StatusTooLarge:
			k.StatusTooLarge.Bytes = min(k.StatusTooLarge.Bytes, math.MaxInt)
		case *corepb.Notification_SaveStalled:
			k.SaveStalled.WaitedMs = min(k.SaveStalled.WaitedMs, maxMillis)
		case *corepb.Notification_InstanceUidChanged:
			uid := make([]byte, 16)
			copy(uid, k.InstanceUidChanged)
			k.InstanceUidChanged = uid
		case *corepb.Notification_ConnectFailed:
			if k.ConnectFailed.GetStatus() == 0 {
				k.ConnectFailed.Status = nil
			}
		case *corepb.Notification_InstallPackage:
			k.InstallPackage.Download = 0
		}
		if back := toCore(n); !proto.Equal(back, want) {
			t.Fatalf("%v read as %+v, which says %v", orig, n, back)
		}
	})
}

// planFS is the disk, failing the operations its plan says: the nth operation (createNew, mkdir,
// rename, remove, syncDir, and a file's Write, Sync and Close) fails if plan[n]%7 is 1, and a Write
// is cut short (half of it written) if it is 2. Operations past the plan pass.
type planFS struct {
	osFS
	mu       sync.Mutex
	plan     []byte
	n        int
	injected int  // faults injected so far
	removeOK bool // no remove has failed
}

func newPlanFS(plan []byte) *planFS { return &planFS{plan: plan, removeOK: true} }

// fault says what happens to the next operation: 0 nothing, 1 it fails, 2 a short write.
func (f *planFS) fault(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.n
	f.n++
	if i >= len(f.plan) {
		return 0
	}
	switch f.plan[i] % 7 {
	case 1:
		f.injected++
		if op == "remove" {
			f.removeOK = false
		}
		return 1
	case 2:
		if op == "write" {
			f.injected++
			return 2
		}
	}
	return 0
}

func (f *planFS) faults() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.injected
}

var errPlanned = errors.New("injected fault")

func (f *planFS) createNew(path string) (writeFile, error) {
	if f.fault("create") == 1 {
		return nil, errPlanned
	}
	w, err := f.osFS.createNew(path)
	if err != nil {
		return nil, err
	}
	return &planFile{w, f}, nil
}

func (f *planFS) remove(path string) error {
	if f.fault("remove") == 1 {
		return errPlanned
	}
	return f.osFS.remove(path)
}

func (f *planFS) rename(from, to string) error {
	if f.fault("rename") == 1 {
		return errPlanned
	}
	return f.osFS.rename(from, to)
}

func (f *planFS) syncDir(dir string) (bool, error) {
	if f.fault("syncdir") == 1 {
		return false, errPlanned
	}
	return f.osFS.syncDir(dir)
}

func (f *planFS) mkdir(dir string) error {
	if f.fault("mkdir") == 1 {
		return errPlanned
	}
	return f.osFS.mkdir(dir)
}

type planFile struct {
	writeFile
	fs *planFS
}

func (w *planFile) Write(b []byte) (int, error) {
	switch w.fs.fault("write") {
	case 1:
		return 0, errPlanned
	case 2:
		n, _ := w.writeFile.Write(b[:len(b)/2])
		return n, io.ErrShortWrite
	}
	return w.writeFile.Write(b)
}

func (w *planFile) Sync() error {
	if w.fs.fault("sync") == 1 {
		return errPlanned
	}
	return w.writeFile.Sync()
}

func (w *planFile) Close() error {
	err := w.writeFile.Close()
	if w.fs.fault("close") == 1 {
		return errPlanned
	}
	return err
}

// xorSealer seals by XOR with a key and a 4-byte tag in front; Open refuses anything else.
type xorSealer struct{ key byte }

func (s xorSealer) Seal(b []byte) ([]byte, error) {
	out := append([]byte("SEAL"), b...)
	for i := 4; i < len(out); i++ {
		out[i] ^= s.key
	}
	return out, nil
}

func (s xorSealer) Open(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, []byte("SEAL")) {
		return nil, errors.New("not sealed")
	}
	out := slices.Clone(b[4:])
	for i := range out {
		out[i] ^= s.key
	}
	return out, nil
}

// A save, whatever fails under it: all or nothing. Stored only once the file holds exactly the new
// state (sealed, if a sealer is set), readable by its user only, with no temporary file left; if
// the save failed, the file holds the old state or the new one whole (the rename landed, but the
// directory couldn't be flushed), never anything else, and the temporary file is gone unless
// deleting it failed too. A stale temporary file (anyone's, any mode) is never written into.
func FuzzSaveState(f *testing.F) {
	f.Add([]byte("old"), true, false, []byte("new state"), []byte{}, byte(0))
	f.Add([]byte("old"), true, true, []byte("new state"), []byte{0, 0, 2}, byte(0))
	f.Add([]byte{}, false, false, []byte{}, []byte{1}, byte(3))
	f.Add([]byte("o"), true, false, bytes.Repeat([]byte{0xab}, 4096), []byte{0, 0, 0, 0, 0, 1}, byte(0x5a))
	f.Add([]byte("o"), true, false, []byte("n"), []byte{0, 0, 0, 0, 1}, byte(0))
	f.Fuzz(func(t *testing.T, old []byte, hadOld, staleTmp bool, data, plan []byte, key byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state")
		var sealer StateSealer
		stored := old
		if key != 0 {
			sealer = xorSealer{key}
			stored, _ = sealer.Seal(old)
		}
		if hadOld {
			if err := os.WriteFile(path, stored, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if staleTmp {
			if err := os.WriteFile(tempFile(path), []byte("someone's"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		fsys := newPlanFS(plan)
		in := store(context.Background(), fsys, path, sealer, &corepb.Store{Id: 7, Data: data})
		if _, err := proto.Marshal(in); err != nil {
			t.Fatalf("%v can't reach the core: %v", in, err)
		}
		got, rerr := os.ReadFile(path)
		if sealer != nil && rerr == nil {
			if got, rerr = sealer.Open(got); rerr != nil {
				t.Fatalf("the file can't be opened: %v", rerr)
			}
		}
		_, terr := os.Stat(tempFile(path))
		tmpLeft := terr == nil
		switch {
		case in.GetStored() != nil:
			if in.GetStored().Id != 7 {
				t.Fatalf("stored %v", in)
			}
			if rerr != nil || !bytes.Equal(got, data) {
				t.Fatalf("stored, but the file holds %q (%v), not %q", got, rerr, data)
			}
			if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
				t.Fatalf("mode %v", st.Mode())
			}
			if tmpLeft {
				t.Fatal("stored, with the temporary file left")
			}
		case in.GetStoreFailed() != nil:
			if in.GetStoreFailed().Id != 7 || fsys.faults() == 0 && !staleTmp {
				t.Fatalf("failed with no fault: %v", in)
			}
			switch {
			case rerr == nil && bytes.Equal(got, data):
			case hadOld && rerr == nil && bytes.Equal(got, old):
			case !hadOld && errors.Is(rerr, os.ErrNotExist):
			default:
				t.Fatalf("a failed save left %q (%v): neither the old state %q nor the new %q", got, rerr, old, data)
			}
			if tmpLeft && fsys.removeOK {
				t.Fatal("a failed save left its temporary file")
			}
		default:
			t.Fatalf("saved as %v", in)
		}
	})
}

// fuzzDownload is one download in a script: what the core has handed over for its file.
type fuzzDownload struct {
	data      []byte
	done      bool // download_done
	cancelled bool
}

// Rust mode's download files, over any order of data, download_done and cancels the core may give
// for a few downloads at once, with faults injected into the disk: a download done is answered
// once, downloaded with the digest of exactly the bytes handed over and a file holding them, or
// failed (only when a fault was injected) with no file left; a cancelled one is never answered and
// leaves no file; a discard deletes the file; every file operation is under the state file's
// directory; and nothing is left once the core has discarded everything.
func FuzzRustModeDownloadFiles(f *testing.F) {
	f.Add([]byte{0x00, 3, 'a', 'b', 'c', 0x04}, []byte{})
	f.Add([]byte{0x00, 2, 'x', 'y', 0x08, 1, 'z', 0x04, 0x0c, 0x05}, []byte{})
	f.Add([]byte{0x00, 1, 'q', 0x05, 0x0c}, []byte{})
	f.Add([]byte{0x04, 0x00, 4, 1, 2, 3, 4, 0x0c}, []byte{0, 1})
	f.Add([]byte{0x00, 5, 1, 2, 3, 4, 5, 0x01, 5, 6, 7, 8, 9, 10, 0x04}, []byte{0, 0, 2})
	f.Fuzz(func(t *testing.T, script, plan []byte) {
		fsys := newPlanFS(plan)
		r := newStreamRig(t, nil, fsys)
		dls := map[uint64]*fuzzDownload{1: {}, 2: {}, 3: {}}
		for i := 0; i < len(script); i++ {
			b := script[i]
			id := uint64(1 + (b>>3)%3)
			d := dls[id]
			if d.cancelled || (d.done && b%8 != 5) {
				continue // the core says nothing more of it (but may cancel it while it waits)
			}
			switch b % 8 {
			case 0, 1, 2, 3:
				n := 0
				if i+1 < len(script) {
					i++
					n = min(int(script[i]%64), len(script)-i-1)
				}
				chunk := slices.Clone(script[i+1 : i+1+n])
				i += n
				d.data = append(d.data, chunk...)
				r.do(nil, dData(id, chunk))
			case 4:
				d.done = true
				r.do(nil, dDone(id))
			case 5:
				d.cancelled = true
				r.do(nil, cancelEffect(id))
			}
		}
		expect := 0
		for _, d := range dls {
			if d.done && !d.cancelled {
				expect++
			}
		}
		answered := map[uint64]bool{}
		for range expect {
			in := r.take()
			id, _ := inputID(in)
			d := dls[id]
			if d == nil || !d.done || d.cancelled || answered[id] {
				t.Fatalf("answered %v: %+v (answered %v)", in, d, answered)
			}
			answered[id] = true
			if _, err := proto.Marshal(in); err != nil {
				t.Fatalf("%v can't reach the core: %v", in, err)
			}
			path := r.x.downloadPath(id)
			switch {
			case in.GetDownloaded() != nil:
				sum := sha256.Sum256(d.data)
				if !bytes.Equal(in.GetDownloaded().Sha256, sum[:]) {
					t.Fatalf("download %d: digest %x, want %x", id, in.GetDownloaded().Sha256, sum)
				}
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, d.data) {
					t.Fatalf("download %d: the file holds %q (%v), want %q", id, got, err, d.data)
				}
			case in.GetDownloadFailed() != nil:
				if fsys.faults() == 0 {
					t.Fatalf("download %d failed with no fault: %v", id, in)
				}
				r.x.work.settle()
				if _, err := os.Stat(path); !os.IsNotExist(err) && fsys.removeOK {
					t.Fatalf("download %d failed, and its file is left (%v)", id, err)
				}
			default:
				t.Fatalf("download %d: %v", id, in)
			}
		}
		r.x.work.settle()
		for {
			select {
			case rep := <-r.x.reports():
				if r.x.deliver(rep) {
					t.Fatalf("reported more: %v", rep.in)
				}
				rep.settled()
				continue
			default:
			}
			break
		}
		for id, d := range dls {
			if d.cancelled {
				if _, err := os.Stat(r.x.downloadPath(id)); !os.IsNotExist(err) && fsys.removeOK {
					t.Fatalf("download %d was cancelled, and its file is left (%v)", id, err)
				}
			}
			switch {
			case answered[id]:
				r.do(nil, discardEffect(id))
			case !d.done && !d.cancelled:
				r.do(nil, cancelEffect(id)) // never finished: the core gives up on it
			}
		}
		r.x.work.settle()
		if left, _ := os.ReadDir(downloadsDir(r.state)); len(left) > 0 && fsys.removeOK {
			t.Fatalf("left after every discard: %v", left)
		}
		checkJournal(t, r)
	})
}

// checkJournal fails if a file operation in r's journal is outside the state file's directory.
func checkJournal(t *testing.T, r *streamRig) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, op := range r.journal {
		for _, k := range []string{"path", "from", "to"} {
			if p, ok := op[k].(string); ok && (filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../")) {
				t.Fatalf("a file operation outside the state file's directory: %v", op)
			}
		}
	}
}

// fuzzURL makes the URL a script names: one the core would give (to a test host, so nothing leaves
// the machine), or the fuzzer's as it is.
func fuzzURL(raw string) string {
	if strings.HasPrefix(raw, "/") {
		return "http://files.test" + raw
	}
	return raw
}

// Go mode's requests and downloads, against a server that answers anything: a request goes only to
// the URL the core gave (as net/url reads it), with the core's headers exactly and its body, or
// fails without being sent when that URL can't be requested; the answer names the effect, can be
// encoded for the core, and is the response as it came (status, headers, body), or failed when the
// body is over the core's limit. A download's file holds the body, its digest says so, a redirect's
// Location is resolved against the URL requested, a body over the limit leaves no file, and a
// discard deletes it.
func FuzzGoModeRequests(f *testing.F) {
	f.Add(false, "/v1/opamp", []byte("Content-Type:application/x-protobuf"), []byte("body"), uint16(200), []byte("X-Odd:caf\xe9"), []byte("reply"), uint64(1<<20))
	f.Add(true, "/pkg/agent.tar.gz?sig=1", []byte("Authorization:Bearer t"), []byte{}, uint16(302), []byte("Location:../v2/agent.tar.gz"), []byte("moved"), uint64(1<<20))
	f.Add(true, "/pkg", []byte{}, []byte{}, uint16(301), []byte("Location:http://[\xe9"), []byte{}, uint64(0))
	f.Add(true, "/pkg", []byte{}, []byte{}, uint16(200), []byte{}, []byte("too big"), uint64(3))
	f.Add(false, "http://%zz", []byte{}, []byte{}, uint16(200), []byte{}, []byte{}, uint64(10))
	f.Add(false, "/x", []byte{}, []byte{}, uint16(999), []byte("a:b\na:c"), []byte("12345"), uint64(5))
	f.Fuzz(func(t *testing.T, download bool, rawURL string, reqHeaders, reqBody []byte, status uint16, respHeaders, respBody []byte, max uint64) {
		target := fuzzURL(rawURL)
		var mu sync.Mutex
		var sent []*http.Request
		var sentBody []byte
		respH := toHTTP(fuzzHeaders(respHeaders)) // as net/http hands headers over: by canonical name
		conn := &connection{http: roundTripper(func(r *http.Request) (*http.Response, error) {
			var b []byte
			if r.Body != nil {
				b, _ = io.ReadAll(r.Body)
			}
			mu.Lock()
			sent, sentBody = append(sent, r), b
			mu.Unlock()
			return &http.Response{StatusCode: int(status), Header: respH.Clone(), Request: r,
				Body: io.NopCloser(bytes.NewReader(respBody))}, nil
		})}
		x := newExecutor(conn)
		state := filepath.Join(t.TempDir(), "state")
		x.setStateFile(state)
		defer x.shutdown(5 * time.Second)
		hs := fuzzHeaders(reqHeaders)
		var eff *corepb.Effect
		if download {
			eff = &corepb.Effect{Kind: &corepb.Effect_Download{Download: &corepb.Download{Id: 3, Url: target, Headers: hs, MaxBytes: max, TimeoutMs: 60_000}}}
		} else {
			eff = &corepb.Effect{Kind: &corepb.Effect_HttpRequest{HttpRequest: &corepb.HttpRequest{Id: 3, Url: target, Headers: hs, Body: reqBody, TimeoutMs: 60_000, MaxResponseBytes: max}}}
		}
		if back := x.do(eff); back != nil {
			t.Fatalf("answered at once: %v", back.in)
		}
		rep := nextReport(t, x)
		in := rep.in
		if id, ok := inputID(in); !ok || id != 3 {
			t.Fatalf("answer %v names no effect 3", in)
		}
		if _, err := proto.Marshal(in); err != nil {
			t.Fatalf("%v can't reach the core: %v", in, err)
		}
		x.fl.close(5 * time.Second) // the round trip's goroutine is over
		mu.Lock()
		defer mu.Unlock()

		u, perr := url.Parse(target)
		if len(sent) == 0 {
			if in.GetHttpFailed() == nil && in.GetDownloadFailed() == nil {
				t.Fatalf("not sent, but answered %v", in)
			}
			if _, err := http.NewRequest(http.MethodGet, target, nil); err == nil {
				t.Fatalf("%q could be requested, but wasn't", target)
			}
			return
		}
		if len(sent) != 1 {
			t.Fatalf("sent %d times", len(sent))
		}
		req := sent[0]
		if perr != nil || req.URL.String() != u.String() {
			t.Fatalf("sent to %v, but the core said %q", req.URL, target)
		}
		if want := map[bool]string{true: http.MethodGet, false: http.MethodPost}[download]; req.Method != want {
			t.Fatalf("method %s", req.Method)
		}
		want := byCanonicalName(hs)
		if len(req.Header) != len(want) {
			t.Fatalf("headers %v, want %v", req.Header, want)
		}
		for k, vs := range want {
			if !slices.Equal(req.Header[k], vs) {
				t.Fatalf("header %q: %q, want %q", k, req.Header[k], vs)
			}
		}
		over := uint64(len(respBody)) > max
		if !download {
			if !bytes.Equal(sentBody, reqBody) {
				t.Fatalf("body %q, want %q", sentBody, reqBody)
			}
			if over {
				if in.GetHttpFailed() == nil {
					t.Fatalf("a body over %d bytes: %v", max, in)
				}
				return
			}
			got := in.GetHttpResponse()
			if got == nil || got.Status != uint32(status) || !bytes.Equal(got.Body, respBody) {
				t.Fatalf("response %v, want %d %q", in, status, respBody)
			}
			n := 0
			for _, vs := range respH {
				n += len(vs)
			}
			if len(got.Headers) != n {
				t.Fatalf("headers %v, from %v", got.Headers, respH)
			}
			return
		}
		path := x.downloadPath(3)
		if over {
			if in.GetDownloadFailed() == nil {
				t.Fatalf("a file over %d bytes: %v", max, in)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("a file over the limit is left (%v)", err)
			}
			return
		}
		got := in.GetDownloadResponse()
		sum := sha256.Sum256(respBody)
		if got == nil || got.Status != uint32(status) || !bytes.Equal(got.Sha256, sum[:]) {
			t.Fatalf("download %v, want %d %x", in, status, sum)
		}
		if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, respBody) {
			t.Fatalf("the file holds %q (%v), want %q", b, err, respBody)
		}
		if rel, err := filepath.Rel(downloadsDir(state), path); err != nil || rel != filepath.Base(path) {
			t.Fatalf("the file %s isn't in %s", path, downloadsDir(state))
		}
		wantLoc, wantOrigin := "", (*corepb.Origin)(nil)
		if status >= 300 && status < 400 {
			wantLoc, wantOrigin = location(req.URL, respH.Get("Location"))
		}
		if got.Location != wantLoc || !proto.Equal(got.LocationOrigin, wantOrigin) {
			t.Fatalf("location %q %v, want %q %v", got.Location, got.LocationOrigin, wantLoc, wantOrigin)
		}
		x.do(&corepb.Effect{Kind: &corepb.Effect_Discard{Discard: &corepb.Id{Id: 3}}})
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("discarded, and the file is left (%v)", err)
		}
	})
}

// fuzzStreamPlatform connects, starts TLS and resolves nothing: every attempt fails at once.
var fuzzStreamPlatform = &streamPlatform{
	dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no network here") },
	startTLS: func(context.Context, net.Conn, *tls.Config) (net.Conn, error) {
		return nil, errors.New("no TLS here")
	},
	resolve: func(context.Context, string) ([]string, error) { return nil, errors.New("no names here") },
}

// Any output the core could give (any bytes that decode), carried out as the loop does, in either
// mode, with no network (every connection fails at once; HTTP answers 200): nothing panics, every
// input that reaches the core can be encoded for it and names an effect of the output, a
// notification is Stopped only when the core says so, every file operation is under the state
// file's directory, and stopping ends everything within its bound.
func FuzzEffectDispatch(f *testing.F) {
	outs := []*corepb.Output{
		{Effects: []*corepb.Effect{httpRequest(1, "http://server.test/v1/opamp")}},
		{Effects: []*corepb.Effect{
			{Kind: &corepb.Effect_Store{Store: &corepb.Store{Id: 2, Data: []byte("state")}}},
			{Kind: &corepb.Effect_Download{Download: &corepb.Download{Id: 3, Url: "https://files.test/p", MaxBytes: 10, TimeoutMs: 1000}}},
			wsOpen(4, "wss://server.test/v1/opamp"),
			{Kind: &corepb.Effect_WsSend{WsSend: &corepb.WsMessage{Id: 4, Data: []byte{0}}}},
			wsClose(4),
			cancelEffect(3),
			{Kind: &corepb.Effect_Discard{Discard: &corepb.Id{Id: 3}}},
		}},
		{Effects: []*corepb.Effect{
			{Kind: &corepb.Effect_ProxyQuery{ProxyQuery: &corepb.Url{Id: 5, Url: "https://server.test/"}}},
			sResolve(6, "server.test"), sOpen(5, "server.test", 443), sTLS(5, "server.test"), sWrite(5, []byte("GET")),
			sSplice(5, 7, 4), dData(7, []byte("abc")), dDone(7), sClose(5, true), discardEffect(7),
		}},
		{Effects: []*corepb.Effect{
			{Kind: &corepb.Effect_Notify{Notify: &corepb.Notification{Kind: &corepb.Notification_Connected{Connected: &corepb.Empty{}}}}},
			{Kind: &corepb.Effect_Notify{Notify: &corepb.Notification{}}},
			{Kind: &corepb.Effect_Notify{Notify: &corepb.Notification{Kind: &corepb.Notification_Stopped{Stopped: &corepb.Empty{}}}}},
		}, Stopped: true},
	}
	for _, o := range outs {
		b, _ := proto.Marshal(o)
		f.Add(b, false)
		f.Add(b, true)
	}
	f.Fuzz(func(t *testing.T, raw []byte, rust bool) {
		out := &corepb.Output{}
		if err := proto.Unmarshal(raw, out); err != nil || len(out.Effects) > 64 {
			t.Skip()
		}
		conn := &connection{
			http: roundTripper(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}),
			dial: fuzzStreamPlatform.dial,
		}
		x := newExecutor(conn)
		state := filepath.Join(t.TempDir(), "state")
		x.setStateFile(state)
		var jmu sync.Mutex
		var journal []map[string]any
		x.fs = journaled(osFS{}, state, func(op []byte) {
			var m map[string]any
			_ = json.Unmarshal(op, &m)
			jmu.Lock()
			journal = append(journal, m)
			jmu.Unlock()
		})
		if rust {
			x.streams = newStreams(x, &tls.Config{}, newRustProxies(Proxy{}, func(string) (string, bool) { return "", false }), fuzzStreamPlatform)
		}
		ids := map[uint64]bool{}
		for _, e := range out.Effects {
			m := e.ProtoReflect()
			if fd := m.WhichOneof(m.Descriptor().Oneofs().ByName("kind")); fd != nil && fd.Message() != nil {
				inner := m.Get(fd).Message()
				if idf := inner.Descriptor().Fields().ByName("id"); idf != nil && idf.Kind() == protoreflect.Uint64Kind {
					ids[inner.Get(idf).Uint()] = true
				}
			}
		}
		check := func(in *corepb.Input) {
			t.Helper()
			if _, err := proto.Marshal(in); err != nil {
				t.Fatalf("%v can't reach the core: %v", in, err)
			}
			if id, ok := inputID(in); ok && !ids[id] {
				t.Fatalf("%v names no effect of the output", in)
			}
		}
		var queued []report
		for _, e := range out.Effects {
			if known(e) != nil {
				continue // the loop stops the client
			}
			if n := e.GetNotify(); n != nil {
				if note := fromCore(n); (note.Kind == Stopped) != (n.GetStopped() != nil) {
					t.Fatalf("%v told as %v", n, note.Kind)
				}
				continue
			}
			if out.Stopped && starts(e) {
				continue
			}
			if back := x.do(e); back != nil {
				queued = append(queued, *back)
			}
		}
		for _, r := range queued {
			if x.deliver(r) {
				check(r.in)
			}
			r.settled()
		}
		// What comes back while the saves and file writes end (connections fail at once too).
		settled := make(chan struct{})
		go func() {
			x.work.settle()
			close(settled)
		}()
		timeout := time.After(10 * time.Second)
	reading:
		for {
			select {
			case r := <-x.reports():
				if x.deliver(r) {
					check(r.in)
				}
				r.settled()
			case <-settled:
				break reading
			case <-timeout:
				t.Fatal("the saves and file writes didn't end")
			}
		}
		begin := time.Now()
		x.shutdown(time.Second)
		if took := time.Since(begin); took > 3*time.Second {
			t.Fatalf("stopping took %v", took)
		}
		jmu.Lock()
		defer jmu.Unlock()
		for _, op := range journal {
			for _, k := range []string{"path", "from", "to"} {
				if p, ok := op[k].(string); ok && (filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../")) {
					t.Fatalf("a file operation outside the state file's directory: %v", op)
				}
			}
		}
	})
}

// setKind puts a oneof wrapper into a notification (toCore's helper).
func setKind(n *corepb.Notification, k any) *corepb.Notification {
	switch v := k.(type) {
	case *corepb.Notification_Connected:
		n.Kind = v
	case *corepb.Notification_ConnectFailed:
		n.Kind = v
	case *corepb.Notification_ErrorResponse:
		n.Kind = v
	case *corepb.Notification_RemoteConfig:
		n.Kind = v
	case *corepb.Notification_CustomMessage:
		n.Kind = v
	case *corepb.Notification_ServerCustomCapabilities:
		n.Kind = v
	case *corepb.Notification_CustomMessageNotSent:
		n.Kind = v
	case *corepb.Notification_InstallPackage:
		n.Kind = v
	case *corepb.Notification_RemovePackage:
		n.Kind = v
	case *corepb.Notification_ConnectionSettings:
		n.Kind = v
	case *corepb.Notification_Command:
		n.Kind = v
	case *corepb.Notification_InstanceUidChanged:
		n.Kind = v
	case *corepb.Notification_HeartbeatIntervalMs:
		n.Kind = v
	case *corepb.Notification_StatusTooLarge:
		n.Kind = v
	case *corepb.Notification_BadMessage:
		n.Kind = v
	case *corepb.Notification_SaveStalled:
		n.Kind = v
	case *corepb.Notification_Stopped:
		n.Kind = v
	default:
		panic(fmt.Sprintf("kind %T", k))
	}
	return n
}
