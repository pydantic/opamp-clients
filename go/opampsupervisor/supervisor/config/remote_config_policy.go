// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	gyaml "gopkg.in/yaml.v3"
)

// componentKinds are the collector config's sections that configure components.
var componentKinds = []string{"receivers", "processors", "exporters", "connectors", "extensions"}

// providerScheme is a confmap provider's scheme, as in ${scheme:uri}.
var providerScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*$`)

// RemoteConfigPolicy limits what the OpAMP server's remote config may configure: a server (or
// whoever takes it over) could otherwise point a filelog receiver at any file the collector can
// read, or ${file:...} a secret into an exporter's headers. The local config files are trusted.
//
// The policy is on once AllowedComponents names a kind: then each component the remote config
// configures must have an allowed type, and each ${...} reference an allowed provider.
type RemoteConfigPolicy struct {
	// AllowedComponents are the component types (the part of an ID before any "/") the remote
	// config may configure, by kind (receivers, processors, exporters, connectors, extensions).
	// A kind not listed allows none.
	AllowedComponents map[string][]string `mapstructure:"allowed_components"`
	// AllowedProviders are the confmap providers the remote config may reference, by scheme
	// (env, file, http, ...; ${NAME} is env). None unless listed.
	AllowedProviders []string `mapstructure:"allowed_providers"`
	// prevent unkeyed literal initialization
	_ struct{}
}

// Enabled says whether the policy restricts anything.
func (p RemoteConfigPolicy) Enabled() bool { return len(p.AllowedComponents) > 0 }

// Validate checks the kinds and provider schemes are ones the collector has.
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
	if !p.Enabled() && len(p.AllowedProviders) > 0 {
		return errors.New("agent::remote_config_policy::allowed_providers has no effect without allowed_components")
	}
	return nil
}

// Check refuses a remote config file (named name in the config map) that the policy doesn't
// allow, saying what it uses that isn't allowed.
func (p RemoteConfigPolicy) Check(name string, body []byte) error {
	if !p.Enabled() {
		return nil
	}
	where := "the remote config"
	if name != "" {
		where = fmt.Sprintf("the remote config file %q", name)
	}
	// The text as written, then the strings YAML decodes (an escape can spell "${").
	if err := p.checkReferences(where, string(body)); err != nil {
		return err
	}
	var doc any
	if err := gyaml.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("%s isn't YAML: %w", where, err)
	}
	var strs []string
	collectStrings(doc, &strs)
	for _, s := range strs {
		if err := p.checkReferences(where, s); err != nil {
			return err
		}
	}

	top, _ := doc.(map[string]any)
	for _, kind := range componentKinds {
		section, ok := top[kind]
		if !ok || section == nil {
			continue
		}
		components, ok := section.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s isn't a map", where, kind)
		}
		ids := make([]string, 0, len(components))
		for id := range components {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			t, _, _ := strings.Cut(id, "/")
			if !slices.Contains(p.AllowedComponents[kind], t) {
				return fmt.Errorf("%s configures %s %q, a type agent::remote_config_policy::allowed_components::%s doesn't allow", where, strings.TrimSuffix(kind, "s"), id, kind)
			}
		}
	}
	return nil
}

// checkReferences refuses a ${...} reference to a provider not allowed. $$ is a literal $; the
// collector no longer expands a bare $NAME.
func (p RemoteConfigPolicy) checkReferences(where, s string) error {
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
