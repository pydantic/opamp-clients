// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package verifier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/rekor/pkg/tle"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
)

// rekorURL is the public Rekor instance, where Cosign's keyless signatures are logged.
const rekorURL = "https://rekor.sigstore.dev"

// lookupTimeout bounds fetching the trusted root and the signature's Rekor entries.
const lookupTimeout = time.Minute

// TrustedMaterialFunc returns the Sigstore roots signatures are checked against: Fulcio's
// certificates, Rekor's and the CT logs' keys.
type TrustedMaterialFunc func(ctx context.Context) (root.TrustedMaterial, error)

// TlogLookupFunc returns the transparency log entries that may record a signature over the
// artifact with this SHA-256; each is checked, and one must match.
type TlogLookupFunc func(ctx context.Context, artifactSHA256 []byte) ([]*tlog.Entry, error)

// cosignVerifier checks a keyless Cosign signature: a Fulcio certificate for one of the
// accepted signers, a signature over the package by its key, and the signature's entry in the
// transparency log while the certificate was valid (it lives minutes; the log says when).
type cosignVerifier struct {
	identities []verify.CertificateIdentity
	trusted    TrustedMaterialFunc
	lookup     TlogLookupFunc
}

// NewCosignVerifier is a Cosign verifier with its trusted roots and log lookups given, so a
// test can stand in for the public Sigstore instance.
func NewCosignVerifier(cfg config.CosignVerifier, trusted TrustedMaterialFunc, lookup TlogLookupFunc) (Verifier, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	var ext certificate.Extensions
	ext.GithubWorkflowRepository = cfg.GithubWorkflowRepository
	identities := make([]verify.CertificateIdentity, 0, len(cfg.Identities))
	for _, id := range cfg.Identities {
		san, err := verify.NewSANMatcher(id.Subject, id.SubjectRegex)
		if err != nil {
			return nil, err
		}
		issuer, err := verify.NewIssuerMatcher(id.Issuer, id.IssuerRegex)
		if err != nil {
			return nil, err
		}
		ci, err := verify.NewCertificateIdentity(san, issuer, ext)
		if err != nil {
			return nil, err
		}
		identities = append(identities, ci)
	}
	return &cosignVerifier{identities: identities, trusted: trusted, lookup: lookup}, nil
}

// PublicSigstore returns the public Sigstore instance's trusted roots (fetched over TUF once,
// at the first verification, and cached under cacheDir) and Rekor lookups.
func PublicSigstore(cacheDir string, httpClient *http.Client) (TrustedMaterialFunc, TlogLookupFunc) {
	var (
		mu      sync.Mutex
		trusted root.TrustedMaterial
	)
	fetch := func(ctx context.Context) (root.TrustedMaterial, error) {
		mu.Lock()
		defer mu.Unlock()
		if trusted != nil {
			return trusted, nil
		}
		opts := tuf.DefaultOptions().WithCachePath(cacheDir).WithContext(ctx)
		tr, err := root.FetchTrustedRootWithOptions(opts)
		if err != nil {
			return nil, fmt.Errorf("fetch Sigstore's trusted root: %w", err)
		}
		trusted = tr
		return tr, nil
	}
	lookup := func(ctx context.Context, digest []byte) ([]*tlog.Entry, error) {
		return rekorEntries(ctx, httpClient, rekorURL, digest)
	}
	return fetch, lookup
}

func (*cosignVerifier) Type() string { return config.VerifierTypeCosign }

// Verify checks signature over packageBytes. The signature is a Sigstore bundle (JSON, as
// `cosign sign-blob --bundle` writes it: it carries its log entry), or upstream's format: the
// base64 certificate and the base64 signature Cosign writes, joined by a space, whose log entry
// is looked up in Rekor.
func (v *cosignVerifier) Verify(ctx context.Context, packageBytes, signature []byte) error {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	digest := sha256.Sum256(packageBytes)

	// Read first: a signature that isn't one is refused without fetching anything.
	var b *bundle.Bundle
	var cert *x509.Certificate
	var sig []byte
	trimmed := bytes.TrimSpace(signature)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		b = &bundle.Bundle{}
		if err := b.UnmarshalJSON(trimmed); err != nil {
			return fmt.Errorf("the signature isn't a Sigstore bundle: %w", err)
		}
	} else {
		var err error
		if cert, sig, err = parseCertAndSignature(trimmed); err != nil {
			return err
		}
	}

	trusted, err := v.trusted(ctx)
	if err != nil {
		return err
	}
	verifier, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithIntegratedTimestamps(1))
	if err != nil {
		return err
	}
	policy := func() verify.PolicyBuilder {
		opts := make([]verify.PolicyOption, 0, len(v.identities))
		for _, id := range v.identities {
			opts = append(opts, verify.WithCertificateIdentity(id))
		}
		return verify.NewPolicy(verify.WithArtifactDigest("sha256", digest[:]), opts...)
	}

	if b != nil {
		if _, err := verifier.Verify(b, policy()); err != nil {
			return fmt.Errorf("the package's signature doesn't verify: %w", err)
		}
		return nil
	}
	entries, err := v.lookup(ctx, digest[:])
	if err != nil {
		return fmt.Errorf("look the signature up in the transparency log: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("the package's signature isn't in the transparency log")
	}
	// Each entry on its own: the log may hold other signatures over the same file.
	var errs []error
	for _, entry := range entries {
		entity := &blobEntity{cert: cert, sig: bundle.NewMessageSignature(digest[:], "SHA2_256", sig), entry: entry}
		_, err := verifier.Verify(entity, policy())
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return fmt.Errorf("the package's signature doesn't verify: %w", errors.Join(errs...))
}

// parseCertAndSignature reads upstream's signature format: base64(PEM certificate), a space,
// base64(signature).
func parseCertAndSignature(s []byte) (*x509.Certificate, []byte, error) {
	certB64, sigB64, ok := strings.Cut(string(s), " ")
	if !ok {
		return nil, nil, errors.New("the signature isn't a Sigstore bundle or a base64 certificate and signature joined by a space")
	}
	certPEM, err := base64.StdEncoding.DecodeString(strings.TrimSpace(certB64))
	if err != nil {
		return nil, nil, fmt.Errorf("the signature's certificate isn't base64: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("the signature's certificate isn't a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("the signature's certificate doesn't parse: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return nil, nil, fmt.Errorf("the signature isn't base64: %w", err)
	}
	return cert, sig, nil
}

// blobEntity is a signature over a file with its certificate and one log entry: a Sigstore
// bundle assembled from Cosign's separate files and Rekor.
type blobEntity struct {
	cert  *x509.Certificate
	sig   *bundle.MessageSignature
	entry *tlog.Entry
}

func (e *blobEntity) HasInclusionPromise() bool { return e.entry.HasInclusionPromise() }
func (e *blobEntity) HasInclusionProof() bool   { return e.entry.HasInclusionProof() }
func (e *blobEntity) SignatureContent() (verify.SignatureContent, error) {
	return e.sig, nil
}
func (e *blobEntity) Timestamps() ([][]byte, error)       { return nil, nil }
func (e *blobEntity) TlogEntries() ([]*tlog.Entry, error) { return []*tlog.Entry{e.entry}, nil }
func (e *blobEntity) Version() (string, error)            { return "v0.1", nil }
func (e *blobEntity) VerificationContent() (verify.VerificationContent, error) {
	return bundle.NewCertificate(e.cert), nil
}

// maxRekorResponse bounds what is read from Rekor per request.
const maxRekorResponse = 4 << 20

// rekorEntries asks Rekor (its v1 REST API) for the entries recording the artifact's SHA-256.
func rekorEntries(ctx context.Context, client *http.Client, base string, digest []byte) ([]*tlog.Entry, error) {
	query, err := json.Marshal(map[string]string{"hash": "sha256:" + hex.EncodeToString(digest)})
	if err != nil {
		return nil, err
	}
	var uuids []string
	if err := rekorCall(ctx, client, http.MethodPost, base+"/api/v1/index/retrieve", query, &uuids); err != nil {
		return nil, err
	}
	var entries []*tlog.Entry
	for _, uuid := range uuids {
		var found map[string]models.LogEntryAnon
		if err := rekorCall(ctx, client, http.MethodGet, base+"/api/v1/log/entries/"+uuid, nil, &found); err != nil {
			return nil, err
		}
		for _, anon := range found {
			entry, err := rekorEntry(anon)
			if err != nil {
				return nil, fmt.Errorf("Rekor entry %s: %w", uuid, err)
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// rekorEntry reads an entry Rekor returned. One without an inclusion proof (only its signed
// entry timestamp) is read as Rekor v1 entries were.
func rekorEntry(anon models.LogEntryAnon) (*tlog.Entry, error) {
	if anon.Verification == nil || anon.LogID == nil || anon.IntegratedTime == nil || anon.LogIndex == nil {
		return nil, errors.New("the entry is incomplete")
	}
	if anon.Verification.InclusionProof != nil {
		pb, err := tle.GenerateTransparencyLogEntry(anon)
		if err != nil {
			return nil, err
		}
		return tlog.NewTlogEntry(pb)
	}
	body, ok := anon.Body.(string)
	if !ok {
		return nil, errors.New("the entry's body isn't a string")
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, err
	}
	logID, err := hex.DecodeString(*anon.LogID)
	if err != nil {
		return nil, err
	}
	return tlog.NewEntry(raw, *anon.IntegratedTime, *anon.LogIndex, logID, anon.Verification.SignedEntryTimestamp, nil) //nolint:staticcheck // SA1019: the only reader of an entry with no inclusion proof
}

func rekorCall(ctx context.Context, client *http.Client, method, url string, body []byte, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRekorResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d", method, url, resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}
