// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: Cosign is
// supported, and Verify takes a context.

package verifier

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

func TestNewVerifier(t *testing.T) {
	testCases := []struct {
		name        string
		verifier    config.Verifier
		expectType  string
		expectedErr string
	}{
		{
			name:       "none verifier",
			verifier:   config.Verifier{Type: config.VerifierTypeNone},
			expectType: config.VerifierTypeNone,
		},
		{
			name:        "unsupported verifier type",
			verifier:    config.Verifier{Type: "gpg"},
			expectedErr: "unsupported verifier type",
		},
		{
			name:        "cosign with no signer",
			verifier:    config.Verifier{Type: config.VerifierTypeCosign},
			expectedErr: "at least one signer",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewVerifier(tc.verifier, t.TempDir(), http.DefaultClient)
			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				assert.Nil(t, v)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, v)
			assert.Equal(t, tc.expectType, v.Type())
		})
	}
}

func TestNoneVerifierVerify(t *testing.T) {
	v := &noneVerifier{}
	assert.NoError(t, v.Verify(t.Context(), []byte("package"), []byte("signature")))
	assert.Equal(t, config.VerifierTypeNone, v.Type())
}
