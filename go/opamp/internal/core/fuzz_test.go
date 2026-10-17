// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package core

// The shipped module through wazero (32-bit WebAssembly, unlike the Rust fuzz targets' native
// build), fed what a shell may hand it: a saved state of any bytes, then any order of answers to
// the effects it asked for (responses with any status, headers and body, server messages built
// from the fuzzer's bytes, with package and connection settings offers naming any URL), failures,
// timers and the application's calls. It never traps or refuses a well-formed input; every effect
// is of a kind the client knows; an effect never takes the id of one still under way; a wake-up is
// never in the past; a request or connection goes to the endpoint, or to one a server offered;
// every URL the core hands over is one net/url reads with the same host as RFC 3986's own split
// (so the client goes where the core judged), and every header is one net/http can send.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// rd reads the fuzzer's bytes: zero once they run out.
type rd struct{ b []byte }

func (r *rd) byte() byte {
	if len(r.b) == 0 {
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *rd) bytes() []byte {
	n := min(int(r.byte()), len(r.b))
	out := bytes.Clone(r.b[:n])
	r.b = r.b[n:]
	return out
}

func (r *rd) str() string { return strings.ToValidUTF8(string(r.bytes()), "�") }

// urlOf is a URL from the fuzzer: mostly near a real one (so offers get past the core's checks
// often), sometimes as given.
func (r *rd) urlOf() string {
	switch r.byte() % 4 {
	case 0:
		return "http://files.test/" + r.str()
	case 1:
		return "https://" + r.str() + "/p"
	case 2:
		return "ws://" + r.str() + ":8080/v1/opamp?" + r.str()
	default:
		return r.str()
	}
}

// serverMessage builds a ServerToAgent from the fuzzer's bytes; offered collects the URLs it offers.
func (r *rd) serverMessage(uid []byte, offered map[string]bool) []byte {
	what := r.byte()
	m := &protobufs.ServerToAgent{InstanceUid: uid}
	if what&1 != 0 {
		m.Flags = uint64(r.byte())
	}
	if what&2 != 0 {
		m.RemoteConfig = &protobufs.AgentRemoteConfig{ConfigHash: r.bytes(), Config: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{r.str(): {Body: r.bytes(), ContentType: r.str()}}}}
	}
	if what&4 != 0 {
		u := r.urlOf()
		offered[u] = true
		m.ConnectionSettings = &protobufs.ConnectionSettingsOffers{Hash: r.bytes(), Opamp: &protobufs.OpAMPConnectionSettings{
			DestinationEndpoint: u, HeartbeatIntervalSeconds: uint64(r.byte()),
			Headers: &protobufs.Headers{Headers: []*protobufs.Header{{Key: r.str(), Value: r.str()}}}}}
	}
	if what&8 != 0 {
		u := r.urlOf()
		body := r.bytes()
		sum := sha256.Sum256(body)
		hash := sum[:]
		if r.byte()%2 == 0 {
			hash = r.bytes()
		}
		m.PackagesAvailable = &protobufs.PackagesAvailable{AllPackagesHash: r.bytes(), Packages: map[string]*protobufs.PackageAvailable{
			r.str(): {Type: protobufs.PackageType(r.byte() % 3), Version: r.str(), Hash: r.bytes(),
				File: &protobufs.DownloadableFile{DownloadUrl: u, ContentHash: hash, Signature: r.bytes()}}}}
	}
	if what&16 != 0 {
		m.ErrorResponse = &protobufs.ServerErrorResponse{Type: protobufs.ServerErrorResponseType(r.byte() % 4), ErrorMessage: r.str(),
			Details: &protobufs.ServerErrorResponse_RetryInfo{RetryInfo: &protobufs.RetryInfo{RetryAfterNanoseconds: uint64(r.byte()) << 30}}}
	}
	if what&32 != 0 {
		m.CustomCapabilities = &protobufs.CustomCapabilities{Capabilities: []string{r.str(), "io.test.cap"}}
		m.CustomMessage = &protobufs.CustomMessage{Capability: "io.test.cap", Type: r.str(), Data: r.bytes()}
	}
	if what&64 != 0 {
		m.Command = &protobufs.ServerToAgentCommand{Type: protobufs.CommandType(r.byte() % 2)}
	}
	if what&128 != 0 {
		m.AgentIdentification = &protobufs.AgentIdentification{NewInstanceUid: r.bytes()}
	}
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// rfc3986 is RFC 3986's appendix B: a URL's parts.
var rfc3986 = regexp.MustCompile(`^(([^:/?#]+):)?(//([^/?#]*))?([^?#]*)(\?([^#]*))?(#(.*))?$`)

// rfc3986Host is the host of an absolute URL as appendix B splits it: the authority, less any
// userinfo and port, less an IPv6 literal's brackets.
func rfc3986Host(raw string) (string, bool) {
	m := rfc3986.FindStringSubmatch(raw)
	if m == nil || m[3] == "" {
		return "", false
	}
	auth := m[4]
	if i := strings.LastIndex(auth, "@"); i >= 0 {
		auth = auth[i+1:]
	}
	if strings.HasPrefix(auth, "[") {
		end := strings.Index(auth, "]")
		if end < 0 {
			return "", false
		}
		return auth[1:end], true
	}
	if i := strings.LastIndex(auth, ":"); i >= 0 {
		auth = auth[:i]
	}
	return auth, true
}

// checkURL fails unless the core's URL is one the client can request where the core meant: net/url
// reads it, absolute, with one of the schemes given, a host, and the host RFC 3986 finds in it.
func checkURL(t *testing.T, what, raw string, schemes ...string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s %q: net/url can't read it: %v", what, raw, err)
	}
	ok := false
	for _, s := range schemes {
		ok = ok || u.Scheme == s
	}
	if !ok || u.Hostname() == "" {
		t.Fatalf("%s %q: scheme %q, host %q", what, raw, u.Scheme, u.Hostname())
	}
	host, _ := rfc3986Host(raw)
	if pct, err := url.PathUnescape(host); err == nil {
		host = pct
	}
	if !strings.EqualFold(host, u.Hostname()) {
		t.Fatalf("%s %q: net/url goes to %q, RFC 3986 says %q", what, raw, u.Hostname(), host)
	}
	if _, err := http.NewRequest(http.MethodGet, raw, nil); err != nil && (u.Scheme == "http" || u.Scheme == "https") {
		t.Fatalf("%s %q: net/http can't request it: %v", what, raw, err)
	}
}

// checkHeaders fails unless net/http can send every header: a token for a name, and a value with
// no CR, LF or NUL.
func checkHeaders(t *testing.T, what string, hs []*corepb.Header) {
	t.Helper()
	for _, h := range hs {
		if h.Name == "" || strings.ContainsFunc(h.Name, func(r rune) bool {
			return r <= ' ' || r >= 0x7f || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", r)
		}) || strings.ContainsAny(h.Value, "\r\n\x00") {
			t.Fatalf("%s: header %q: %q can't be sent", what, h.Name, h.Value)
		}
	}
}

func FuzzCoreSteps(f *testing.F) {
	f.Add(byte(0), []byte{}, []byte{1, 0, 0, 13})
	f.Add(byte(1), []byte{}, []byte{4, 0, 5, 0, 0, 2, 0, 0, 13, 9, 0})
	// A package offered, downloaded, saved, installed.
	f.Add(byte(2|4), []byte{}, []byte{1, 0, 0x08, 0, 3, 'p', 'k', 'g', 4, 'a', 'b', 'c', 'd', 1, 1, 9, 1, 'p', 0, 1, '1', 1, 1, 0,
		0, 0, 11, 0, 1, 4, 'a', 'b', 'c', 'd', 0, 9, 0, 12, 3, 1, 'p', 1, 1, 0, 0, 9, 0, 0, 0, 13})
	// New connection settings offered, and tried.
	f.Add(byte(2|8), []byte{}, []byte{1, 0, 0x04, 1, 8, 'n', 'e', 'w', '.', 't', 'e', 's', 't', 1, 5, 7, 1, 'X', 1, 'v',
		0, 0, 9, 0, 1, 0, 0, 1, 1, 0, 0, 0, 0, 0, 13})
	f.Add(byte(2), []byte{0x0a, 0x10, 1, 2, 3}, []byte{0, 30, 3, 0, 0xf7, 1, 9, 0, 0, 30})
	f.Add(byte(1), []byte{}, []byte{4, 0, 15, 0, 3, 0, 1, 2, 6, 0, 0xe8, 13})
	f.Fuzz(func(t *testing.T, flags byte, saved, script []byte) {
		ctx := context.Background()
		endpoint := "http://server.test/v1/opamp"
		if flags&1 != 0 {
			endpoint = "ws://server.test/v1/opamp"
		}
		persist := flags&2 != 0
		cfg := &corepb.Config{InstanceUid: bytes.Repeat([]byte{7}, 16), Endpoint: endpoint, Entropy: bytes.Repeat([]byte{1}, 32),
			UnixMs: 1_790_000_000_000, Seed: 42, Capabilities: 0xffff, Persistence: persist,
			AcceptPackages: persist && flags&4 != 0, AcceptConnectionSettings: persist && flags&8 != 0,
			MaxReceiveBytes: 1 << 16, StopTimeoutMs: 5000}
		if persist {
			cfg.SavedState = saved
		}
		c, first, err := New(ctx, Options{}, cfg)
		if err != nil {
			if strings.Contains(err.Error(), "trapped") {
				t.Fatalf("init trapped: %v", err)
			}
			return // a saved state it refuses, with the reason
		}
		defer c.Close(ctx)
		uid := first.GetInstanceUid()
		offered := map[string]bool{endpoint: true}
		live := map[uint64]string{} // effects under way: their kind
		opened := map[uint64]bool{} // WebSocket connections open
		now := uint64(0)
		stopped := false
		step := func(in *corepb.Input) {
			t.Helper()
			out, err := c.Step(ctx, now, cfg.UnixMs+now, in)
			if err != nil {
				t.Fatalf("step %v: %v", in, err)
			}
			if len(out.GetInstanceUid()) != 16 {
				t.Fatalf("instance UID %x", out.GetInstanceUid())
			}
			uid = out.GetInstanceUid()
			if w := out.WakeAtMs; w != nil && *w < now {
				t.Fatalf("a wake-up at %d, at %d", *w, now)
			}
			for _, e := range out.Effects {
				fresh := func(id uint64, kind string) {
					if k, ok := live[id]; ok {
						t.Fatalf("%s %d while %s %d is under way", kind, id, k, id)
					}
					live[id] = kind
				}
				switch k := e.Kind.(type) {
				case nil:
					t.Fatalf("an effect of no kind the client knows: %v", e)
				case *corepb.Effect_HttpRequest:
					fresh(k.HttpRequest.Id, "request")
					if !offered[k.HttpRequest.Url] {
						t.Fatalf("a request to %q: neither the endpoint nor offered", k.HttpRequest.Url)
					}
					checkURL(t, "request", k.HttpRequest.Url, "http", "https")
					checkHeaders(t, "request", k.HttpRequest.Headers)
				case *corepb.Effect_WsOpen:
					fresh(k.WsOpen.Id, "connection")
					if !offered[k.WsOpen.Url] {
						t.Fatalf("a connection to %q: neither the endpoint nor offered", k.WsOpen.Url)
					}
					checkURL(t, "connection", k.WsOpen.Url, "ws", "wss")
					checkHeaders(t, "connection", k.WsOpen.Headers)
				case *corepb.Effect_Download:
					fresh(k.Download.Id, "download")
					if !cfg.AcceptPackages {
						t.Fatalf("a download, with packages not accepted: %v", k.Download)
					}
					checkURL(t, "download", k.Download.Url, "http", "https")
					checkHeaders(t, "download", k.Download.Headers)
				case *corepb.Effect_Store:
					fresh(k.Store.Id, "save")
					if !persist {
						t.Fatalf("a save, with no persistence: %v", k.Store)
					}
				case *corepb.Effect_Cancel:
					delete(live, k.Cancel.Id)
					delete(opened, k.Cancel.Id)
				case *corepb.Effect_WsSend:
					if !opened[k.WsSend.Id] {
						t.Fatalf("a send on %d, not open", k.WsSend.Id)
					}
				case *corepb.Effect_Notify:
					if k.Notify.Kind == nil {
						t.Fatalf("a notification of no kind: %v", e)
					}
				case *corepb.Effect_StreamOpen, *corepb.Effect_StreamResolve, *corepb.Effect_StreamWrite,
					*corepb.Effect_StreamClose, *corepb.Effect_StreamStartTls, *corepb.Effect_StreamSplice,
					*corepb.Effect_ProxyQuery, *corepb.Effect_DownloadData, *corepb.Effect_DownloadDone:
					t.Fatalf("a stream effect without Config.streams: %v", e)
				}
			}
			stopped = out.Stopped
		}
		// pick is an effect under way of kind (opened: an open connection), chosen by the next byte.
		r := &rd{b: script}
		pick := func(kind string, open bool) (uint64, bool) {
			var ids []uint64
			for id, k := range live {
				if k == kind && opened[id] == open {
					ids = append(ids, id)
				}
			}
			if len(ids) == 0 {
				return 0, false
			}
			// In id order, so a script means the same each run.
			for i := range ids {
				for j := i + 1; j < len(ids); j++ {
					if ids[j] < ids[i] {
						ids[i], ids[j] = ids[j], ids[i]
					}
				}
			}
			return ids[int(r.byte())%len(ids)], true
		}

		step(&corepb.Input{Kind: &corepb.Input_Start{Start: &corepb.Empty{}}})
		for len(r.b) > 0 && !stopped {
			switch op := r.byte() % 16; op {
			case 0:
				now += uint64(r.byte()) * 1000
				step(&corepb.Input{Kind: &corepb.Input_Timer{Timer: &corepb.Empty{}}})
			case 1, 3, 14:
				id, ok := pick("request", false)
				if !ok {
					continue
				}
				resp := &corepb.HttpResponse{Id: id, Status: 200, UnixMs: cfg.UnixMs + now}
				switch op {
				case 1:
					resp.Body = r.serverMessage(uid, offered)
				case 3:
					resp.Status = uint32(r.byte())*4 + 100
					resp.Headers = []*corepb.Header{{Name: "Retry-After", Value: r.str()}, {Name: "Content-Type", Value: r.str()}}
				case 14:
					resp.Body = r.bytes()
				}
				delete(live, id)
				step(&corepb.Input{Kind: &corepb.Input_HttpResponse{HttpResponse: resp}})
			case 2:
				if id, ok := pick("request", false); ok {
					delete(live, id)
					step(&corepb.Input{Kind: &corepb.Input_HttpFailed{HttpFailed: &corepb.Failed{Id: id, Detail: r.str()}}})
				}
			case 4:
				if id, ok := pick("connection", false); ok {
					opened[id] = true
					step(&corepb.Input{Kind: &corepb.Input_WsOpened{WsOpened: &corepb.Id{Id: id}}})
				}
			case 5, 15:
				if id, ok := pick("connection", true); ok {
					data := append([]byte{0}, r.serverMessage(uid, offered)...)
					if op == 15 {
						data = r.bytes()
					}
					step(&corepb.Input{Kind: &corepb.Input_WsMessage{WsMessage: &corepb.WsMessage{Id: id, Data: data}}})
				}
			case 6:
				if id, ok := pick("connection", true); ok {
					step(&corepb.Input{Kind: &corepb.Input_WsClosed{WsClosed: &corepb.WsClosed{Id: id, Code: 1000 + uint32(r.byte())}}})
				}
			case 7:
				if id, ok := pick("connection", true); ok {
					delete(live, id)
					delete(opened, id)
					step(&corepb.Input{Kind: &corepb.Input_WsLost{WsLost: &corepb.Failed{Id: id, Detail: r.str()}}})
				}
			case 8:
				if id, ok := pick("connection", false); ok {
					delete(live, id)
					status := 400 + uint32(r.byte())
					step(&corepb.Input{Kind: &corepb.Input_WsFailed{WsFailed: &corepb.WsFailed{Id: id, Status: &status, Detail: r.str(),
						Headers: []*corepb.Header{{Name: "Retry-After", Value: r.str()}}, UnixMs: cfg.UnixMs + now}}})
				}
			case 9, 10:
				if id, ok := pick("save", false); ok {
					delete(live, id)
					if op == 9 {
						step(&corepb.Input{Kind: &corepb.Input_Stored{Stored: &corepb.Id{Id: id}}})
					} else {
						step(&corepb.Input{Kind: &corepb.Input_StoreFailed{StoreFailed: &corepb.Failed{Id: id, Detail: r.str()}}})
					}
				}
			case 11:
				if id, ok := pick("download", false); ok {
					delete(live, id)
					if r.byte()%4 == 0 {
						step(&corepb.Input{Kind: &corepb.Input_DownloadFailed{DownloadFailed: &corepb.Failed{Id: id, Detail: r.str()}}})
						continue
					}
					body := r.bytes()
					sum := sha256.Sum256(body)
					status := []uint32{200, 200, 302, 404}[r.byte()%4]
					resp := &corepb.DownloadResponse{Id: id, Status: status, Sha256: sum[:]}
					if status == 302 {
						resp.Location = r.urlOf()
						// As the shell does (transport.go's location): a host net/url decoded to bytes
						// that aren't UTF-8 has no origin the core could be told of.
						if u, err := url.Parse(resp.Location); err == nil && u.Hostname() != "" && utf8.ValidString(u.Hostname()) {
							resp.LocationOrigin = &corepb.Origin{Scheme: u.Scheme, Host: u.Hostname()}
						}
					}
					step(&corepb.Input{Kind: &corepb.Input_DownloadResponse{DownloadResponse: resp}})
				}
			case 12:
				switch r.byte() % 5 {
				case 0:
					step(&corepb.Input{Kind: &corepb.Input_SetDescription{SetDescription: &protobufs.AgentDescription{
						IdentifyingAttributes: []*protobufs.KeyValue{{Key: "service.name", Value: &protobufs.AnyValue{
							Value: &protobufs.AnyValue_StringValue{StringValue: r.str()}}}}}}})
				case 1:
					step(&corepb.Input{Kind: &corepb.Input_SendCustomMessage{SendCustomMessage: &protobufs.CustomMessage{
						Capability: "io.test.cap", Type: r.str(), Data: r.bytes()}}})
				case 2:
					step(&corepb.Input{Kind: &corepb.Input_SetRemoteConfigStatus{SetRemoteConfigStatus: &protobufs.RemoteConfigStatus{
						LastRemoteConfigHash: r.bytes(), Status: protobufs.RemoteConfigStatuses(r.byte() % 4)}}})
				case 3:
					step(&corepb.Input{Kind: &corepb.Input_PackageInstalled{PackageInstalled: &corepb.PackageInstalled{
						Name: r.str(), Hash: r.bytes(), Ok: r.byte()%2 == 0, Detail: r.str()}}})
				case 4:
					step(&corepb.Input{Kind: &corepb.Input_SetConnectionSettingsStatus{SetConnectionSettingsStatus: &protobufs.ConnectionSettingsStatus{
						LastConnectionSettingsHash: r.bytes(), Status: protobufs.ConnectionSettingsStatuses(r.byte() % 4)}}})
				}
			case 13:
				step(&corepb.Input{Kind: &corepb.Input_Stop{Stop: &corepb.Empty{}}})
			}
		}
	})
}
