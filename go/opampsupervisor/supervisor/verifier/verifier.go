// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: Verify takes
// a context, and the Cosign verifier (cosign.go) is implemented.

// Package verifier verifies the signatures of collector executable packages
// downloaded by the supervisor.
package verifier

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// Verifier verifies the signature of a downloaded package.
type Verifier interface {
	// Verify returns nil if signature is a valid signature for packageBytes.
	Verify(ctx context.Context, packageBytes, signature []byte) error
	// Type returns the verifier type, matching the configured verifier type.
	Type() string
}

// NewVerifier returns the Verifier for the given verifier configuration. Cosign's checks against
// the public Sigstore instance: its trusted roots are cached under cacheDir.
func NewVerifier(cfg config.Verifier, cacheDir string, httpClient *http.Client) (Verifier, error) {
	switch cfg.Type {
	case config.VerifierTypeNone:
		return &noneVerifier{}, nil
	case config.VerifierTypeCosign:
		trusted, lookup := PublicSigstore(cacheDir, httpClient)
		return NewCosignVerifier(cfg.Cosign, trusted, lookup)
	default:
		return nil, fmt.Errorf("unsupported verifier type: %q", cfg.Type)
	}
}

// noneVerifier performs no verification and accepts every package.
type noneVerifier struct{}

func (noneVerifier) Verify(context.Context, []byte, []byte) error { return nil }

func (noneVerifier) Type() string { return config.VerifierTypeNone }
