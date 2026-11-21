// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package supervisor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/sdk/resource"
)

// hostKeyAttribute is the non-identifying attribute of the agent description that carries this
// host's key: the same value for as long as the host keeps its machine ID, whatever is installed,
// removed or enrolled on it, so a server can tie a host enrolled again to the one it replaces.
const hostKeyAttribute = "pydantic.fleet.host_key"

// hostKeyApp is the fixed key the host key is derived with. It makes the key this application's
// alone: the same machine gives another value to any other use of its machine ID.
var hostKeyApp = uuid.MustParse("c8fe2a9e-5d40-4615-8f88-fd606391a0ca")

// hostKeyFrom derives the host key from a machine ID: HMAC-SHA256 keyed with hostKeyApp, its
// first 16 bytes as a version 4 UUID (as systemd derives an application-specific machine ID). It
// reveals nothing about the machine ID. "" for no machine ID: no key is made up.
func hostKeyFrom(machineID string) string {
	machineID = strings.TrimSpace(machineID)
	// The SDK says "<unknown>" on a system it can't read one from: no machine's own.
	if machineID == "" || machineID == "<unknown>" {
		return ""
	}
	mac := hmac.New(sha256.New, hostKeyApp[:])
	mac.Write([]byte(machineID))
	var id uuid.UUID
	copy(id[:], mac.Sum(nil))
	id[6] = id[6]&0x0f | 0x40 // version 4
	id[8] = id[8]&0x3f | 0x80 // RFC 4122 variant
	return id.String()
}

// machineID is the OS's machine ID, as the OpenTelemetry Go SDK's host ID detector reads it:
// Linux /etc/machine-id or /var/lib/dbus/machine-id, macOS IOPlatformUUID, Windows MachineGuid, the
// BSDs' hostid. "" when there is none (a container with no machine ID, say).
// The SDK reads it with no time limit (macOS runs ioreg), so it is given machineIDTimeout: the
// first agent description waits on it.
func machineID() string {
	found := make(chan string, 1)
	go func() {
		r, err := resource.New(context.Background(), resource.WithHostID())
		if err != nil {
			found <- ""
			return
		}
		v, _ := r.Set().Value("host.id")
		found <- v.AsString()
	}()
	select {
	case id := <-found:
		return id
	case <-time.After(machineIDTimeout):
		return ""
	}
}

// machineIDTimeout is how long reading the machine ID may take.
const machineIDTimeout = 5 * time.Second

// hostKey is this host's key, computed once: it never changes while the Supervisor runs. A
// variable for the tests.
var hostKey = sync.OnceValue(func() string { return hostKeyFrom(machineID()) })

// withHostKey adds the host key to an agent description's non-identifying attributes, unless
// they have one already (agent::description::non_identifying_attributes in the config, say) or
// the host has no machine ID. One set to "" there means none: it is left out.
func withHostKey(attrs []*protobufs.KeyValue) []*protobufs.KeyValue {
	for i, kv := range attrs {
		if kv.GetKey() == hostKeyAttribute {
			if kv.GetValue().GetStringValue() == "" {
				return append(attrs[:i:i], attrs[i+1:]...)
			}
			return attrs
		}
	}
	key := hostKey()
	if key == "" {
		return attrs
	}
	return append(attrs, &protobufs.KeyValue{Key: hostKeyAttribute, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: key}}})
}
