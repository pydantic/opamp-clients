// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/pydantic/opamp-clients/go/opamp/internal/corepb"
)

// An install's or a removal's error says how it failed: wrapped by FailedForNow it is for now,
// by FailedLeavingUnknown it leaves what the agent has unknown, and otherwise it is for good. The
// wrap may sit anywhere in the error's chain.
func TestPackageAnswersSayHowTheyFailed(t *testing.T) {
	boom := errors.New("boom")
	for name, c := range map[string]struct {
		err  error
		want corepb.PackageFailure
	}{
		"for good":           {boom, corepb.PackageFailure_PACKAGE_FAILURE_FINAL},
		"for now":            {FailedForNow(boom), corepb.PackageFailure_PACKAGE_FAILURE_LATER},
		"unknown":            {FailedLeavingUnknown(boom), corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN},
		"for now, wrapped":   {fmt.Errorf("install: %w", FailedForNow(boom)), corepb.PackageFailure_PACKAGE_FAILURE_LATER},
		"unknown, in a join": {errors.Join(boom, FailedLeavingUnknown(errors.New("rollback"))), corepb.PackageFailure_PACKAGE_FAILURE_UNKNOWN},
	} {
		t.Run(name, func(t *testing.T) {
			in := packageInstalled("p", []byte{1}, c.err)
			if in.Ok || in.Failure != c.want || in.Detail != c.err.Error() {
				t.Fatalf("installed: %v, want failure %v with detail %q", in, c.want, c.err)
			}
			rm := packageRemoved("p", c.err)
			if !rm.Failed || rm.Failure != c.want || rm.Detail != c.err.Error() {
				t.Fatalf("removed: %v, want failure %v with detail %q", rm, c.want, c.err)
			}
		})
	}
	if in := packageInstalled("p", []byte{1}, nil); !in.Ok || in.Failure != corepb.PackageFailure_PACKAGE_FAILURE_FINAL {
		t.Fatalf("installed: %v", in)
	}
	if rm := packageRemoved("p", nil); rm.Failed {
		t.Fatalf("removed: %v", rm)
	}
	if !errors.Is(FailedForNow(boom), boom) || !errors.Is(FailedLeavingUnknown(boom), boom) {
		t.Fatal("the error wrapped isn't in the chain")
	}
}

// An install that failed for now is not reported failed: the package waits, INSTALL_PENDING, to
// be tried again. One that failed for good is INSTALL_FAILED, with why.
func TestAnInstallThatFailedForNowIsNotReportedFailed(t *testing.T) {
	file := []byte("the package")
	sum := sha256.Sum256(file)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(file) }))
	defer files.Close()
	for _, forNow := range []bool{true, false} {
		t.Run(fmt.Sprintf("for now %v", forNow), func(t *testing.T) {
			srv := &offerServer{answer: func(*protobufs.AgentToServer) *protobufs.ServerToAgent {
				return &protobufs.ServerToAgent{PackagesAvailable: &protobufs.PackagesAvailable{
					Packages: map[string]*protobufs.PackageAvailable{"p": {Type: protobufs.PackageType_PackageType_TopLevel,
						Version: "1", Hash: []byte{1}, File: &protobufs.DownloadableFile{DownloadUrl: files.URL + "/p", ContentHash: sum[:]}}},
					AllPackagesHash: []byte{9}}}
			}}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			answered := make(chan struct{}, 1)
			c, err := Start(context.Background(), Config{Mode: ModePlatform, Endpoint: ts.URL, HeartbeatInterval: time.Hour,
				StateFile: filepath.Join(t.TempDir(), "state.bin"), AcceptPackages: true}, Status{}, func(c *Client, n Notification) {
				if n.Kind == InstallPackage {
					err := errors.New("the signature service is down")
					if forNow {
						err = FailedForNow(err)
					}
					c.PackageInstalled(n.PackageName, n.Package.Hash, err)
					select {
					case answered <- struct{}{}:
					default:
					}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Stop(context.Background()) }()
			select {
			case <-answered:
			case <-time.After(10 * time.Second):
				t.Fatal("no InstallPackage")
			}
			status := func() (protobufs.PackageStatusEnum, string, bool) {
				msgs := srv.messages()
				for i := len(msgs) - 1; i >= 0; i-- {
					if p := msgs[i].GetPackageStatuses().GetPackages()["p"]; p != nil {
						return p.Status, p.ErrorMessage, true
					}
				}
				return 0, "", false
			}
			if forNow {
				// Given time to say otherwise, it doesn't.
				time.Sleep(time.Second)
				for _, m := range srv.messages() {
					if p := m.GetPackageStatuses().GetPackages()["p"]; p != nil && p.Status == protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed {
						t.Fatalf("reported failed: %v", p)
					}
				}
				return
			}
			if !eventually(5*time.Second, func() bool {
				s, why, ok := status()
				return ok && s == protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed && why != ""
			}) {
				s, why, _ := status()
				t.Fatalf("status %v %q, want INSTALL_FAILED with why", s, why)
			}
		})
	}
}
