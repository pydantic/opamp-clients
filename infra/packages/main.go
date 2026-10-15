// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

// Command packages is the Pulumi program for where the Packages workflow publishes the signed apt
// and yum repositories (docs/package-hosting.md): the publishing bucket, served at the domain
// through a CDN, and the keyless identity the workflow publishes as. Its settings are the stack's
// config (Pulumi.<stack>.yaml); gcp:project picks the project.
package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		conf := config.New(ctx, "")
		p, err := NewPublishing(ctx, "packages", PublishingArgs{
			Bucket:     conf.Get("bucket"),
			Location:   conf.Get("location"),
			Domain:     conf.Get("domain"),
			Repository: conf.Get("repository"),
			DNSZone:    conf.Get("dnsZone"),
		})
		if err != nil {
			return err
		}
		// The address is what the domain's A record points at, when Pulumi doesn't make it.
		ctx.Export("address", p.Address)
		ctx.Export("bucket", p.Bucket)
		ctx.Export("serviceAccount", p.ServiceAccount)
		ctx.Export("workloadIdentityProvider", p.Provider)
		return nil
	})
}
