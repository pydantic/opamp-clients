// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: the Cosign
// verifier's settings, as upstream's specification describes them, health_timeout, the archive
// format read from the file, and PydanticCosignVerifier.

package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// AgentPackage describes how collector executable updates downloaded by the
// supervisor are identified and verified. The archive format is not configured
// here; the supervisor detects it from the file (gzip: a .tar.gz; zip: a .zip).
type AgentPackage struct {
	// AgentBinary is the name of the collector binary as it appears inside the
	// downloaded archive. Used to locate the binary in archives that bundle
	// multiple files (e.g. tar.gz).
	AgentBinary string `mapstructure:"agent_binary"`
	// Verifier configures how downloaded packages are verified.
	Verifier Verifier `mapstructure:"verifier"`
	// HealthTimeout is how long an installed collector has to report healthy before the
	// Supervisor puts the previous one back. Changed here: upstream's design names no limit.
	HealthTimeout time.Duration `mapstructure:"health_timeout"`
	// prevent unkeyed literal initialization
	_ struct{}
}

// Verifier configures how downloaded packages are verified. Changed here: Cosign is
// implemented, and is the default.
type Verifier struct {
	// Type selects the verification method: "cosign", or "" for none.
	Type string `mapstructure:"type"`
	// Cosign configures the "cosign" method.
	Cosign CosignVerifier `mapstructure:"cosign"`
	// prevent unkeyed literal initialization
	_ struct{}
}

const (
	// VerifierTypeNone disables package signature verification.
	VerifierTypeNone = ""
	// VerifierTypeCosign verifies a keyless Cosign signature: a Fulcio certificate whose
	// identity matches one of Identities, and the signature's entry in Rekor.
	VerifierTypeCosign = "cosign"
)

// CosignVerifier says whose Cosign signatures a package may carry. The defaults are those of
// the OpenTelemetry Collector Releases repository's release workflow.
type CosignVerifier struct {
	// GithubWorkflowRepository is the repository the certificate must name as the one whose
	// workflow signed (owner/name); empty: not checked.
	GithubWorkflowRepository string `mapstructure:"github_workflow_repository"`
	// Identities are the signers accepted: one must match.
	Identities []CosignIdentity `mapstructure:"identities"`
	// prevent unkeyed literal initialization
	_ struct{}
}

// CosignIdentity is a signer: its OIDC issuer and subject, each exact or a regular expression.
type CosignIdentity struct {
	Issuer       string `mapstructure:"issuer"`
	IssuerRegex  string `mapstructure:"issuer_regex"`
	Subject      string `mapstructure:"subject"`
	SubjectRegex string `mapstructure:"subject_regex"`
	// prevent unkeyed literal initialization
	_ struct{}
}

// DefaultCosignVerifier accepts the OpenTelemetry Collector Releases repository's releases.
func DefaultCosignVerifier() CosignVerifier {
	return CosignVerifier{
		GithubWorkflowRepository: "open-telemetry/opentelemetry-collector-releases",
		Identities: []CosignIdentity{{
			Issuer:       "https://token.actions.githubusercontent.com",
			SubjectRegex: `^https://github.com/open-telemetry/opentelemetry-collector-releases/.github/workflows/base-release.yaml@refs/tags/[^/]*$`,
		}},
	}
}

// PydanticCosignVerifier accepts this repository's Supervisor releases: the archives its release
// workflow signs, on a go/opampsupervisor/vX.Y.Z tag or run by hand from main. It is what
// pydantic-opamp-enroll --accept-packages writes.
func PydanticCosignVerifier() CosignVerifier {
	return CosignVerifier{
		GithubWorkflowRepository: "pydantic/opamp-clients",
		Identities: []CosignIdentity{{
			Issuer:       "https://token.actions.githubusercontent.com",
			SubjectRegex: PydanticReleaseSubjectRegex,
		}},
	}
}

// PydanticReleaseSubjectRegex matches the certificate subject of this repository's release
// workflow.
const PydanticReleaseSubjectRegex = `^https://github\.com/pydantic/opamp-clients/\.github/workflows/release\.yml@refs/(tags/go/opampsupervisor/v[0-9]+\.[0-9]+\.[0-9]+|heads/main)$`

// Validate checks that health_timeout isn't negative (0: the default, 30s).
func (p AgentPackage) Validate() error {
	if p.HealthTimeout < 0 {
		return errors.New("agent::package::health_timeout must not be negative")
	}
	return nil
}

// Validate validates the verifier configuration.
func (v Verifier) Validate() error {
	switch v.Type {
	case VerifierTypeNone:
		return nil
	case VerifierTypeCosign:
		return v.Cosign.Check()
	default:
		return fmt.Errorf("unsupported verifier type: %q", v.Type)
	}
}

// Check checks that there is a signer to accept, and that each names its issuer and subject
// once, with any regular expression valid. Not Validate: confmap would run it on every config,
// Cosign's or not.
func (c CosignVerifier) Check() error {
	if len(c.Identities) == 0 {
		return errors.New("agent::package::verifier::cosign::identities must name at least one signer")
	}
	for i, id := range c.Identities {
		if (id.Issuer == "") == (id.IssuerRegex == "") {
			return fmt.Errorf("agent::package::verifier::cosign::identities[%d] must set one of issuer and issuer_regex", i)
		}
		if (id.Subject == "") == (id.SubjectRegex == "") {
			return fmt.Errorf("agent::package::verifier::cosign::identities[%d] must set one of subject and subject_regex", i)
		}
		for _, re := range []string{id.IssuerRegex, id.SubjectRegex} {
			if _, err := regexp.Compile(re); err != nil {
				return fmt.Errorf("agent::package::verifier::cosign::identities[%d]: %w", i, err)
			}
		}
	}
	return nil
}
