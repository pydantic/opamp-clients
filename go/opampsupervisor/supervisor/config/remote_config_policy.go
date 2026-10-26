// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"runtime"
	"slices"
	"strings"

	gyaml "gopkg.in/yaml.v3"
)

// componentKinds are the collector config's sections that configure components.
var componentKinds = []string{"receivers", "processors", "exporters", "connectors", "extensions"}

// providerScheme is a confmap provider's scheme, as in ${scheme:uri}.
var providerScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*$`)

// RemoteConfigPolicy limits what the OpAMP server's remote config may configure: a server (or
// whoever takes it over) could otherwise point a filelog receiver at any file the collector can
// read, ${file:...} a secret into an exporter's headers, or send what the collector has to any
// host. The local config files are trusted, and only they can widen it.
//
// The policy is on once AllowedComponents names a kind. Then each component the remote config
// configures must have an allowed type; each ${...} reference must name an allowed provider;
// each destination its exporters (and the collector's own telemetry) send to must be an allowed
// endpoint, over checked TLS and no proxy it sets; and each file a filelog receiver reads must be
// an allowed log path or under one; each directory a file_storage extension keeps its files in
// must be an allowed storage directory or under one; and the system log receivers may read only
// this host's own logs. Every list allows nothing unless it is set.
type RemoteConfigPolicy struct {
	// AllowedComponents are the component types (the part of an ID before any "/") the remote
	// config may configure, by kind (receivers, processors, exporters, connectors, extensions).
	// A kind not listed allows none.
	AllowedComponents map[string][]string `mapstructure:"allowed_components"`
	// AllowedProviders are the confmap providers the remote config may reference, by scheme
	// (env, file, http, ...; ${NAME} is env). CheckWithConnectionEnv separately permits bound names.
	AllowedProviders []string `mapstructure:"allowed_providers"`
	// AllowedExporterEndpoints are where the remote config's exporters, and the collector's own
	// telemetry, may send: origins, scheme://host[:port] (http or https; the host's case and the
	// scheme's default port don't matter, any path on it is allowed). None unless listed.
	AllowedExporterEndpoints []string `mapstructure:"allowed_exporter_endpoints"`
	// AllowedLogPaths are the files and directories a filelog receiver in the remote config may
	// read: each path it includes (a glob) must be one of them or lie under one. Absolute paths,
	// with no glob characters; compared case and all. None unless listed.
	AllowedLogPaths []string `mapstructure:"allowed_log_paths"`
	// AllowedStorageDirectories are where a file_storage extension in the remote config may keep
	// its files: its directory, and its compaction directory, must be one of them or lie under
	// one. Absolute paths, with no glob characters. None unless listed.
	AllowedStorageDirectories []string `mapstructure:"allowed_storage_directories"`
	// prevent unkeyed literal initialization
	_ struct{}
}

// Enabled says whether the policy restricts anything.
func (p RemoteConfigPolicy) Enabled() bool { return len(p.AllowedComponents) > 0 }

// Validate checks the kinds and provider schemes are ones the collector has, the endpoints are
// origins and the log paths absolute paths.
func (p RemoteConfigPolicy) Validate() error {
	for kind, types := range p.AllowedComponents {
		if !slices.Contains(componentKinds, kind) {
			return fmt.Errorf("agent::remote_config_policy::allowed_components: %q isn't one of %s", kind, strings.Join(componentKinds, ", "))
		}
		for _, t := range types {
			if t == "" || strings.Contains(t, "/") {
				return fmt.Errorf("agent::remote_config_policy::allowed_components::%s: %q isn't a component type", kind, t)
			}
		}
	}
	for _, scheme := range p.AllowedProviders {
		if !providerScheme.MatchString(scheme) {
			return fmt.Errorf("agent::remote_config_policy::allowed_providers: %q isn't a provider scheme", scheme)
		}
	}
	for _, e := range p.AllowedExporterEndpoints {
		u, err := parseDestination(e)
		if err != nil {
			return fmt.Errorf("agent::remote_config_policy::allowed_exporter_endpoints: %q %w", e, err)
		}
		switch {
		case u.User != nil:
			return fmt.Errorf("agent::remote_config_policy::allowed_exporter_endpoints: %q has user info", e)
		case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
			return fmt.Errorf("agent::remote_config_policy::allowed_exporter_endpoints: %q has a path: give scheme://host[:port]", e)
		}
	}
	for _, path := range p.AllowedLogPaths {
		if err := validateLogPath(runtime.GOOS, path); err != nil {
			return err
		}
	}
	for _, dir := range p.AllowedStorageDirectories {
		if _, err := cleanLogPath(runtime.GOOS, dir, false); err != nil {
			return fmt.Errorf("agent::remote_config_policy::allowed_storage_directories: %q %w", dir, err)
		}
	}
	if !p.Enabled() {
		for name, set := range map[string]bool{
			"allowed_providers":           len(p.AllowedProviders) > 0,
			"allowed_exporter_endpoints":  len(p.AllowedExporterEndpoints) > 0,
			"allowed_log_paths":           len(p.AllowedLogPaths) > 0,
			"allowed_storage_directories": len(p.AllowedStorageDirectories) > 0,
		} {
			if set {
				return fmt.Errorf("agent::remote_config_policy::%s has no effect without allowed_components", name)
			}
		}
	}
	return nil
}

// Check refuses a remote config file (named name in the config map) that the policy doesn't
// allow, saying what it uses that isn't allowed.
func (p RemoteConfigPolicy) Check(name string, body []byte) error {
	return p.check(name, body, nil)
}

// CheckWithConnectionEnv also permits references to the environment names that the local
// agent.connection_env configuration binds to offered credentials. It does not permit other
// environment variables or change the component, destination, or log path restrictions.
func (p RemoteConfigPolicy) CheckWithConnectionEnv(name string, body []byte, bindings map[string]string) error {
	return p.check(name, body, slices.Collect(maps.Values(bindings)))
}

func (p RemoteConfigPolicy) check(name string, body []byte, connectionEnv []string) error {
	if !p.Enabled() {
		return nil
	}
	where := "the remote config"
	if name != "" {
		where = fmt.Sprintf("the remote config file %q", name)
	}
	// The text as written, then the strings YAML decodes (an escape can spell "${").
	if err := p.checkReferences(where, string(body), connectionEnv); err != nil {
		return err
	}
	var doc any
	if err := gyaml.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("%s isn't YAML: %w", where, err)
	}
	var strs []string
	collectStrings(doc, &strs)
	for _, s := range strs {
		if err := p.checkReferences(where, s, connectionEnv); err != nil {
			return err
		}
	}

	// The rest reads the config as the Supervisor merges it, and the collector reads it: with
	// confmap, which splits a key holding "::" into nested ones ("receivers::filelog").
	conf, err := NewConfFromYAML(body)
	if err != nil {
		return fmt.Errorf("%s isn't a collector config: %w", where, err)
	}
	top := conf.ToStringMap()
	sections := map[string]map[string]any{}
	for _, kind := range componentKinds {
		section, ok := top[kind]
		if !ok || section == nil {
			continue
		}
		components, ok := section.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s isn't a map", where, kind)
		}
		sections[kind] = components
		for _, id := range slices.Sorted(maps.Keys(components)) {
			t, _, _ := strings.Cut(id, "/")
			if !slices.Contains(p.AllowedComponents[kind], t) {
				return fmt.Errorf("%s configures %s %q, a type agent::remote_config_policy::allowed_components::%s doesn't allow", where, strings.TrimSuffix(kind, "s"), id, kind)
			}
		}
	}

	for _, id := range slices.Sorted(maps.Keys(sections["exporters"])) {
		t, _, _ := strings.Cut(id, "/")
		// A gRPC target is host:port once the scheme is taken off: a path would be part of it.
		d := destinations{p: p, where: where, what: fmt.Sprintf("exporter %q", id), originOnly: slices.Contains(grpcExporterType, t)}
		if err := d.walk(sections["exporters"][id]); err != nil {
			return err
		}
	}
	if service, ok := top["service"].(map[string]any); ok {
		d := destinations{p: p, where: where, what: "the collector's own telemetry", telemetry: true}
		if err := d.walk(service["telemetry"]); err != nil {
			return err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(sections["receivers"])) {
		if t, _, _ := strings.Cut(id, "/"); slices.Contains(filelogTypes, t) {
			if err := p.checkFilelog(where, id, sections["receivers"][id]); err != nil {
				return err
			}
		}
		if err := checkSystemLogs(where, id, sections["receivers"][id]); err != nil {
			return err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(sections["extensions"])) {
		if t, _, _ := strings.Cut(id, "/"); t == fileStorageType {
			if err := p.checkFileStorage(where, id, sections["extensions"][id]); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileStorageType is the file_storage extension's type.
const fileStorageType = "file_storage"

// systemLogSettings are the settings of the system log receivers that reach past this host's own
// logs: a Windows event log receiver's remote servers (and the credentials for them), and a macOS
// unified logging receiver's archive, read from any path. Only the local config may set them.
var systemLogSettings = map[string][]string{
	"windows_event_log":     {"remote"},
	"windowseventlog":       {"remote"},
	"macos_unified_logging": {"archive_path"},
}

// checkSystemLogs refuses a system log receiver set to read past this host's own logs.
func checkSystemLogs(where, id string, cfg any) error {
	t, _, _ := strings.Cut(id, "/")
	m, _ := cfg.(map[string]any)
	for _, key := range systemLogSettings[t] {
		if v := m[key]; v != nil && v != "" {
			return fmt.Errorf("%s: receiver %q sets %s, which only the local config may", where, id, key)
		}
	}
	return nil
}

// checkFileStorage refuses a file_storage extension that keeps its files, or compacts them,
// outside the allowed storage directories. Its default directory isn't one: it must name it.
func (p RemoteConfigPolicy) checkFileStorage(where, id string, cfg any) error {
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: extension %q sets no directory, which agent::remote_config_policy::allowed_storage_directories needs", where, id)
	}
	dirs := map[string]any{"directory": m["directory"]}
	if compaction, ok := m["compaction"].(map[string]any); ok && compaction["directory"] != nil {
		dirs["compaction::directory"] = compaction["directory"]
	}
	for _, key := range slices.Sorted(maps.Keys(dirs)) {
		dir, ok := dirs[key].(string)
		if !ok || dir == "" {
			return fmt.Errorf("%s: extension %q sets no %s, which agent::remote_config_policy::allowed_storage_directories needs", where, id, key)
		}
		if err := storageDirectoryAllowed(runtime.GOOS, p.AllowedStorageDirectories, dir); err != nil {
			return fmt.Errorf("%s: extension %q keeps files in %q, which %w", where, id, dir, err)
		}
	}
	return nil
}

// storageDirectoryAllowed says whether a directory is an allowed one or lies under one, on goos.
func storageDirectoryAllowed(goos string, allowed []string, dir string) error {
	clean, err := cleanLogPath(goos, dir, false)
	if err != nil {
		return err
	}
	for _, a := range allowed {
		root, err := cleanLogPath(goos, a, false)
		if err != nil {
			continue
		}
		if clean == root || strings.HasPrefix(clean, strings.TrimSuffix(root, "/")+"/") {
			return nil
		}
	}
	return errors.New("agent::remote_config_policy::allowed_storage_directories doesn't allow")
}

// The collector's names for the filelog receiver and the gRPC OTLP exporter: v0.163.0 names them
// file_log and otlp_grpc, and keeps the old names as aliases.
var (
	filelogTypes     = []string{"filelog", "file_log"}
	grpcExporterType = []string{"otlp", "otlp_grpc"}
)

// localOnly are the client settings that would let a remote config send somewhere other than
// where its endpoint says, or to a host whose certificate isn't checked: a proxy, other trust
// anchors, another name to check, no TLS.
var localOnly = []string{"proxy_url", "ca_file", "ca_pem", "server_name_override", "insecure", "insecure_skip_verify"}

// destinations checks where one exporter (or the collector's own telemetry) sends.
type destinations struct {
	p           RemoteConfigPolicy
	where, what string
	// originOnly refuses a path on the endpoint.
	originOnly bool
	// telemetry also checks where the collector writes its own logs.
	telemetry bool
}

func (d destinations) walk(v any) error {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			e := v[k]
			switch {
			case k == "endpoint" || strings.HasSuffix(k, "_endpoint"):
				if err := d.endpoint(k, e); err != nil {
					return err
				}
			case slices.Contains(localOnly, k):
				if e != nil && e != false && e != "" {
					return fmt.Errorf("%s: %s sets %s, which only the local config may", d.where, d.what, k)
				}
			case d.telemetry && (k == "output_paths" || k == "error_output_paths"):
				paths, ok := e.([]any)
				if !ok && e != nil {
					paths = []any{e}
				}
				for _, path := range paths {
					if path != "stdout" && path != "stderr" {
						return fmt.Errorf("%s: %s writes to %q: a remote config may send it only to stdout and stderr", d.where, d.what, fmt.Sprint(path))
					}
				}
			}
			if err := d.walk(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range v {
			if err := d.walk(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d destinations) endpoint(key string, v any) error {
	if v == nil || v == "" {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("%s: %s %s isn't a string", d.where, d.what, key)
	}
	if strings.Contains(s, "$") {
		return fmt.Errorf("%s: %s sends to %q, which holds a $ (what it expands to can't be checked)", d.where, d.what, s)
	}
	u, err := parseDestination(s)
	if err != nil {
		return fmt.Errorf("%s: %s sends to %q, which %w", d.where, d.what, s, err)
	}
	if d.originOnly && ((u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "") {
		return fmt.Errorf("%s: %s sends to %q, which has a path: a gRPC endpoint is scheme://host[:port]", d.where, d.what, s)
	}
	for _, a := range d.p.AllowedExporterEndpoints {
		if au, err := parseDestination(a); err == nil && sameOrigin(u, au) {
			return nil
		}
	}
	return fmt.Errorf("%s: %s sends to %q, which agent::remote_config_policy::allowed_exporter_endpoints doesn't allow", d.where, d.what, s)
}

// parseDestination parses an http:// or https:// URL with a host.
func parseDestination(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return nil, errors.New("isn't an http:// or https:// URL")
	}
	if u.Hostname() == "" {
		return nil, errors.New("has no host")
	}
	return u, nil
}

// sameOrigin compares scheme, host (in any case, with or without the root's dot) and port (the
// scheme's default if none).
func sameOrigin(a, b *url.URL) bool {
	host := func(u *url.URL) string { return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") }
	port := func(u *url.URL) string {
		switch p := u.Port(); {
		case p != "":
			return p
		case u.Scheme == "https":
			return "443"
		default:
			return "80"
		}
	}
	return a.Scheme == b.Scheme && host(a) == host(b) && port(a) == port(b)
}

// checkFilelog refuses a filelog receiver that reads a path the policy doesn't allow, or deletes
// what it reads.
func (p RemoteConfigPolicy) checkFilelog(where, id string, cfg any) error {
	if cfg == nil {
		return nil
	}
	m, ok := cfg.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: receiver %q isn't a map", where, id)
	}
	if v := m["delete_after_read"]; v != nil && v != false {
		return fmt.Errorf("%s: receiver %q sets delete_after_read, which only the local config may", where, id)
	}
	var paths []string
	switch include := m["include"].(type) {
	case nil:
	case string:
		// As the collector decodes a string into a list.
		paths = strings.Split(include, ",")
	case []any:
		for _, e := range include {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("%s: receiver %q include isn't a list of paths", where, id)
			}
			paths = append(paths, s)
		}
	default:
		return fmt.Errorf("%s: receiver %q include isn't a list of paths", where, id)
	}
	for _, path := range paths {
		if err := logPathAllowed(runtime.GOOS, p.AllowedLogPaths, path); err != nil {
			return fmt.Errorf("%s: receiver %q reads %q, which %w", where, id, path, err)
		}
	}
	return nil
}

// logPathAllowed says whether a filelog include glob lies within an allowed path, on goos.
func logPathAllowed(goos string, allowed []string, include string) error {
	clean, err := cleanLogPath(goos, include, true)
	if err != nil {
		return err
	}
	for _, a := range allowed {
		dir, err := cleanLogPath(goos, a, false)
		if err != nil {
			continue
		}
		if clean == dir || strings.HasPrefix(clean, strings.TrimSuffix(dir, "/")+"/") {
			return nil
		}
	}
	return errors.New("agent::remote_config_policy::allowed_log_paths doesn't allow")
}

// validateLogPath checks an allowed log path is an absolute path with no glob in it.
func validateLogPath(goos, path string) error {
	if _, err := cleanLogPath(goos, path, false); err != nil {
		return fmt.Errorf("agent::remote_config_policy::allowed_log_paths: %q %w", path, err)
	}
	return nil
}

// cleanLogPath gives an absolute path (a glob, if glob) with forward slashes, cleaned, or says
// why it can't be compared: anything that could take a glob outside the directory it starts in
// is refused, not interpreted.
func cleanLogPath(goos, p string, glob bool) (string, error) {
	if goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	} else if strings.Contains(p, `\`) {
		// The glob's escape: "\.\." is "..".
		return "", errors.New(`has a \`)
	}
	switch {
	case strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", errors.New("has a control character")
	case strings.Contains(p, "$"):
		return "", errors.New("holds a $ (what it expands to can't be checked)")
	case strings.Contains(p, ".."):
		return "", errors.New("could climb out with ..")
	case strings.ContainsAny(p, "{}"):
		return "", errors.New("has braces: give each pattern its own include")
	case !glob && strings.ContainsAny(p, "*?[]"):
		return "", errors.New("has a glob character")
	}
	if goos == "windows" {
		// A drive, then a path: not a share (//server), a device (//?/) or a stream (a.log:x).
		if len(p) < 3 || !isLetter(p[0]) || p[1] != ':' || p[2] != '/' {
			return "", errors.New("isn't absolute (a drive letter, then the path)")
		}
		if strings.Contains(p[2:], ":") {
			return "", errors.New("has a : after the drive")
		}
	} else if !strings.HasPrefix(p, "/") {
		return "", errors.New("isn't absolute")
	}
	return path.Clean(p), nil
}

func isLetter(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }

// checkReferences refuses a ${...} reference to a provider not allowed. $$ is a literal $; the
// collector no longer expands a bare $NAME.
func (p RemoteConfigPolicy) checkReferences(where, s string, connectionEnv []string) error {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			continue
		}
		next := s[i+1]
		var scheme string
		switch {
		case next == '$':
			i++
			continue
		case next == '{':
			ref := s[i+2:]
			end := strings.IndexAny(ref, ":}")
			if end > 0 && ref[end] == ':' && providerScheme.MatchString(ref[:end]) {
				scheme = ref[:end]
			} else {
				scheme = "env"
			}
		default:
			continue
		}
		if scheme == "env" {
			ref := s[i+2:]
			if end := strings.IndexByte(ref, '}'); end >= 0 {
				name := strings.TrimPrefix(ref[:end], "env:")
				if name != "" && slices.Contains(connectionEnv, name) {
					continue
				}
			}
		}
		if !slices.Contains(p.AllowedProviders, scheme) {
			return fmt.Errorf("%s references the %s provider, which agent::remote_config_policy::allowed_providers doesn't allow", where, scheme)
		}
	}
	return nil
}

// collectStrings gathers every string in a decoded YAML document: keys and values.
func collectStrings(v any, out *[]string) {
	switch v := v.(type) {
	case string:
		*out = append(*out, v)
	case map[string]any:
		for k, e := range v {
			*out = append(*out, k)
			collectStrings(e, out)
		}
	case map[any]any:
		for k, e := range v {
			collectStrings(k, out)
			collectStrings(e, out)
		}
	case []any:
		for _, e := range v {
			collectStrings(e, out)
		}
	}
}
