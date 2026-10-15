// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package verifier

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	rekorpb "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

const (
	testIssuer  = "https://token.actions.githubusercontent.com"
	testSubject = "https://github.com/pydantic/opamp-clients/.github/workflows/release.yaml@refs/tags/v1.2.3"
)

// signed is a package Cosign signed in a test Sigstore, as a release workflow would.
type signed struct {
	pkg     []byte
	sig     string // upstream's format: base64 PEM certificate, space, base64 signature
	bundle  string // the same, as a Sigstore bundle
	entries []*tlog.Entry
	tle     *rekorpb.TransparencyLogEntry // the entry, as Rekor would return it
}

func sign(t *testing.T, vs *ca.VirtualSigstore, subject string, pkg []byte) signed {
	t.Helper()
	e, err := vs.Sign(subject, testIssuer, pkg)
	require.NoError(t, err)
	vc, err := e.VerificationContent()
	require.NoError(t, err)
	sc, err := e.SignatureContent()
	require.NoError(t, err)
	entries, err := e.TlogEntries()
	require.NoError(t, err)
	cert := vc.Certificate()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})

	// The entry in full, with its signed entry timestamp (the test entity keeps it to itself).
	tle := entries[0].TransparencyLogEntry()
	logID := hex.EncodeToString(tle.GetLogId().GetKeyId())
	set, err := vs.RekorSignPayload(tlog.RekorPayload{
		Body:           base64.StdEncoding.EncodeToString(tle.GetCanonicalizedBody()),
		IntegratedTime: tle.GetIntegratedTime(),
		LogIndex:       tle.GetLogIndex(),
		LogID:          logID,
	})
	require.NoError(t, err)
	tle.KindVersion = &rekorpb.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}
	tle.InclusionPromise = &rekorpb.InclusionPromise{SignedEntryTimestamp: set}

	digest := sha256.Sum256(pkg)
	pb := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.1", // a promise, no inclusion proof: the test log has none
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content:     &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: cert.Raw}},
			TlogEntries: []*rekorpb.TransparencyLogEntry{tle},
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: digest[:]},
			Signature:     sc.Signature(),
		}},
	}
	bundleJSON, err := protojson.Marshal(pb)
	require.NoError(t, err)
	return signed{
		pkg:     pkg,
		sig:     base64.StdEncoding.EncodeToString(certPEM) + " " + base64.StdEncoding.EncodeToString(sc.Signature()),
		bundle:  string(bundleJSON),
		entries: entries,
		tle:     tle,
	}
}

func testVerifier(t *testing.T, vs *ca.VirtualSigstore, cfg config.CosignVerifier, entries []*tlog.Entry) Verifier {
	t.Helper()
	v, err := NewCosignVerifier(cfg,
		func(context.Context) (root.TrustedMaterial, error) { return vs, nil },
		func(context.Context, []byte) ([]*tlog.Entry, error) { return entries, nil })
	require.NoError(t, err)
	return v
}

func ourSigner() config.CosignVerifier {
	return config.CosignVerifier{Identities: []config.CosignIdentity{{Issuer: testIssuer, Subject: testSubject}}}
}

func TestCosignAcceptsASignatureFromAnAcceptedSigner(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	s := sign(t, vs, testSubject, []byte("the collector, v1.2.3"))

	v := testVerifier(t, vs, ourSigner(), s.entries)
	require.Equal(t, config.VerifierTypeCosign, v.Type())
	require.NoError(t, v.Verify(t.Context(), s.pkg, []byte(s.sig)), "upstream's format")
	require.NoError(t, v.Verify(t.Context(), s.pkg, []byte(s.bundle)), "a Sigstore bundle")

	regex := config.CosignVerifier{Identities: []config.CosignIdentity{
		{Issuer: testIssuer, Subject: "https://example.com/someone-else"},
		{IssuerRegex: `^https://token\.actions\.githubusercontent\.com$`, SubjectRegex: `^https://github\.com/pydantic/opamp-clients/.*@refs/tags/[^/]*$`},
	}}
	require.NoError(t, testVerifier(t, vs, regex, s.entries).Verify(t.Context(), s.pkg, []byte(s.sig)), "any one signer matching is enough")
}

func TestCosignRefuses(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	s := sign(t, vs, testSubject, []byte("the collector, v1.2.3"))
	other, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	fromOther := sign(t, other, testSubject, s.pkg)
	notOurs := sign(t, vs, "https://github.com/someone/else/.github/workflows/release.yaml@refs/tags/v1", s.pkg)

	for _, tc := range []struct {
		name    string
		v       Verifier
		pkg     []byte
		sig     string
		wantErr string
	}{
		{name: "a package that isn't the one signed", v: testVerifier(t, vs, ourSigner(), s.entries), pkg: []byte("something else"), sig: s.sig, wantErr: "doesn't verify"},
		{name: "a bundle for a package that isn't the one signed", v: testVerifier(t, vs, ourSigner(), nil), pkg: []byte("something else"), sig: s.bundle, wantErr: "doesn't verify"},
		{name: "a signer not accepted", v: testVerifier(t, vs, ourSigner(), notOurs.entries), pkg: s.pkg, sig: notOurs.sig, wantErr: "doesn't verify"},
		{name: "a certificate from another Fulcio", v: testVerifier(t, vs, ourSigner(), fromOther.entries), pkg: s.pkg, sig: fromOther.sig, wantErr: "doesn't verify"},
		{name: "a signature the log doesn't hold", v: testVerifier(t, vs, ourSigner(), nil), pkg: s.pkg, sig: s.sig, wantErr: "isn't in the transparency log"},
		{name: "a log entry for another signature", v: testVerifier(t, vs, ourSigner(), notOurs.entries), pkg: s.pkg, sig: s.sig, wantErr: "doesn't verify"},
		{name: "the wrong repository", v: testVerifier(t, vs, config.CosignVerifier{GithubWorkflowRepository: "open-telemetry/opentelemetry-collector-releases", Identities: ourSigner().Identities}, s.entries), pkg: s.pkg, sig: s.sig, wantErr: "doesn't verify"},
		{name: "no signature", v: testVerifier(t, vs, ourSigner(), s.entries), pkg: s.pkg, sig: "", wantErr: "joined by a space"},
		{name: "a certificate that isn't base64", v: testVerifier(t, vs, ourSigner(), s.entries), pkg: s.pkg, sig: "%%% " + strings.Split(s.sig, " ")[1], wantErr: "certificate isn't base64"},
		{name: "a certificate that isn't PEM", v: testVerifier(t, vs, ourSigner(), s.entries), pkg: s.pkg, sig: "aGVsbG8= " + strings.Split(s.sig, " ")[1], wantErr: "isn't a PEM certificate"},
		{name: "a signature that isn't base64", v: testVerifier(t, vs, ourSigner(), s.entries), pkg: s.pkg, sig: strings.Split(s.sig, " ")[0] + " %%%", wantErr: "signature isn't base64"},
		{name: "a bundle that isn't one", v: testVerifier(t, vs, ourSigner(), s.entries), pkg: s.pkg, sig: `{"mediaType": 1}`, wantErr: "isn't a Sigstore bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, tc.v.Verify(t.Context(), tc.pkg, []byte(tc.sig)), tc.wantErr)
		})
	}
}

func TestCosignNeedsASigner(t *testing.T) {
	_, err := NewCosignVerifier(config.CosignVerifier{}, nil, nil)
	require.ErrorContains(t, err, "at least one signer")
	v, err := NewVerifier(config.Verifier{Type: config.VerifierTypeCosign, Cosign: config.DefaultCosignVerifier()}, t.TempDir(), http.DefaultClient)
	require.NoError(t, err)
	require.Equal(t, config.VerifierTypeCosign, v.Type())
}

func TestRekorEntriesAreLookedUpByTheFilesHash(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	s := sign(t, vs, testSubject, []byte("the collector, v1.2.3"))
	digest := sha256.Sum256(s.pkg)

	tle := s.tle
	logID := hex.EncodeToString(tle.GetLogId().GetKeyId())
	anon := models.LogEntryAnon{
		Body:           base64.StdEncoding.EncodeToString(tle.GetCanonicalizedBody()),
		IntegratedTime: &tle.IntegratedTime,
		LogIndex:       &tle.LogIndex,
		LogID:          &logID,
		Verification:   &models.LogEntryAnonVerification{SignedEntryTimestamp: tle.GetInclusionPromise().GetSignedEntryTimestamp()},
	}
	rekor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/index/retrieve":
			var q map[string]string
			if json.NewDecoder(r.Body).Decode(&q) != nil || q["hash"] != "sha256:"+hex.EncodeToString(digest[:]) {
				_ = json.NewEncoder(w).Encode([]string{})
				return
			}
			_ = json.NewEncoder(w).Encode([]string{"the-uuid"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/log/entries/the-uuid":
			_ = json.NewEncoder(w).Encode(map[string]models.LogEntryAnon{"the-uuid": anon})
		default:
			http.Error(w, "no", http.StatusNotFound)
		}
	}))
	defer rekor.Close()

	entries, err := rekorEntries(t.Context(), rekor.Client(), rekor.URL, digest[:])
	require.NoError(t, err)
	require.Len(t, entries, 1)
	v := testVerifier(t, vs, ourSigner(), entries)
	require.NoError(t, v.Verify(t.Context(), s.pkg, []byte(s.sig)), "an entry read from Rekor verifies")

	other := sha256.Sum256([]byte("unsigned"))
	entries, err = rekorEntries(t.Context(), rekor.Client(), rekor.URL, other[:])
	require.NoError(t, err)
	require.Empty(t, entries)

	_, err = rekorEntries(t.Context(), rekor.Client(), rekor.URL+"/nowhere", digest[:])
	require.ErrorContains(t, err, "HTTP 404")
}

type noNetwork struct{}

func (noNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in this test")
}

// A signature that can't be one is refused before Sigstore's roots are fetched.
func TestCosignReadsTheSignatureBeforeFetchingAnything(t *testing.T) {
	v, err := NewVerifier(config.Verifier{Type: config.VerifierTypeCosign, Cosign: config.DefaultCosignVerifier()}, t.TempDir(), &http.Client{Transport: noNetwork{}})
	require.NoError(t, err)
	require.ErrorContains(t, v.Verify(t.Context(), []byte("pkg"), []byte("nope")), "joined by a space")
	require.ErrorContains(t, v.Verify(t.Context(), []byte("pkg"), []byte("{")), "isn't a Sigstore bundle")
}
