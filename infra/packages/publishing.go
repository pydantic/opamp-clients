// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/dns"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/iam"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi-github/sdk/v6/go/github"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// githubIssuer is where GitHub Actions' OIDC tokens come from.
const githubIssuer = "https://token.actions.githubusercontent.com"

// environment is the GitHub environment the Packages workflow runs in: the only workflow runs
// that can publish, and only for the release tags below.
const environment = "packages"

// releaseTags are the tags whose runs may enter the environment, and so publish.
var releaseTags = []string{"go/opampsupervisor/v*", "bundle/v*"}

// PublishingArgs are the publishing side's settings; empty ones take the defaults below.
type PublishingArgs struct {
	Bucket     string // the bucket's name: pydantic-opamp-packages
	Location   string // where it lives: US (multi-region)
	Domain     string // where it's served: packages.pydantic.dev
	Repository string // the GitHub repository whose workflow publishes: pydantic/opamp-clients
	// The Cloud DNS managed zone to add the domain's A record to. Empty, no record is made: the
	// `address` output is what to point the domain at, wherever its DNS is.
	DNSZone string
}

// Publishing is everything the Packages workflow publishes to, and the GitHub environment that
// tells the workflow where it is. The signing key is not here: Pulumi state would hold it.
type Publishing struct {
	pulumi.ResourceState

	Bucket         pulumi.StringOutput `pulumi:"bucket"`
	Address        pulumi.StringOutput `pulumi:"address"`
	ServiceAccount pulumi.StringOutput `pulumi:"serviceAccount"`
	Provider       pulumi.StringOutput `pulumi:"provider"`
}

func (args PublishingArgs) withDefaults() PublishingArgs {
	or := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	args.Bucket = or(args.Bucket, "pydantic-opamp-packages")
	args.Location = or(args.Location, "US")
	args.Domain = or(args.Domain, "packages.pydantic.dev")
	args.Repository = or(args.Repository, "pydantic/opamp-clients")
	return args
}

// NewPublishing makes the publishing side (docs/package-hosting.md, "What has to exist").
func NewPublishing(ctx *pulumi.Context, name string, args PublishingArgs, opts ...pulumi.ResourceOption) (*Publishing, error) {
	args = args.withDefaults()
	owner, repo, ok := strings.Cut(args.Repository, "/")
	if !ok {
		return nil, fmt.Errorf("repository %q isn't owner/name", args.Repository)
	}
	p := &Publishing{}
	if err := ctx.RegisterComponentResource("pydantic:packages:Publishing", name, p, opts...); err != nil {
		return nil, err
	}
	parent := pulumi.Parent(p)

	// The bucket, readable by anyone: every file in it is signed, or listed in a signed index.
	bucket, err := storage.NewBucket(ctx, name, &storage.BucketArgs{
		Name:                     pulumi.String(args.Bucket),
		Location:                 pulumi.String(args.Location),
		UniformBucketLevelAccess: pulumi.Bool(true),
	}, parent)
	if err != nil {
		return nil, err
	}
	if _, err := storage.NewBucketIAMMember(ctx, name+"-public", &storage.BucketIAMMemberArgs{
		Bucket: bucket.Name,
		Role:   pulumi.String("roles/storage.objectViewer"),
		Member: pulumi.String("allUsers"),
	}, parent); err != nil {
		return nil, err
	}

	// HTTPS at the domain, through Cloud CDN. The CDN keeps the Cache-Control publish.sh sets:
	// packages cached for good, indexes not at all.
	backend, err := compute.NewBackendBucket(ctx, name, &compute.BackendBucketArgs{
		BucketName: bucket.Name,
		EnableCdn:  pulumi.Bool(true),
		CdnPolicy:  &compute.BackendBucketCdnPolicyArgs{CacheMode: pulumi.String("USE_ORIGIN_HEADERS")},
	}, parent)
	if err != nil {
		return nil, err
	}
	urlMap, err := compute.NewURLMap(ctx, name, &compute.URLMapArgs{DefaultService: backend.SelfLink}, parent)
	if err != nil {
		return nil, err
	}
	certificate, err := compute.NewManagedSslCertificate(ctx, name, &compute.ManagedSslCertificateArgs{
		Managed: &compute.ManagedSslCertificateManagedArgs{Domains: pulumi.StringArray{pulumi.String(args.Domain)}},
	}, parent)
	if err != nil {
		return nil, err
	}
	proxy, err := compute.NewTargetHttpsProxy(ctx, name, &compute.TargetHttpsProxyArgs{
		UrlMap:          urlMap.SelfLink,
		SslCertificates: pulumi.StringArray{certificate.SelfLink},
	}, parent)
	if err != nil {
		return nil, err
	}
	address, err := compute.NewGlobalAddress(ctx, name, &compute.GlobalAddressArgs{}, parent)
	if err != nil {
		return nil, err
	}
	if _, err := compute.NewGlobalForwardingRule(ctx, name, &compute.GlobalForwardingRuleArgs{
		Target:              proxy.SelfLink,
		IpAddress:           address.Address,
		PortRange:           pulumi.String("443"),
		LoadBalancingScheme: pulumi.String("EXTERNAL_MANAGED"),
	}, parent); err != nil {
		return nil, err
	}
	if args.DNSZone != "" {
		if _, err := dns.NewRecordSet(ctx, name, &dns.RecordSetArgs{
			ManagedZone: pulumi.String(args.DNSZone),
			Name:        pulumi.String(args.Domain + "."),
			Type:        pulumi.String("A"),
			Ttl:         pulumi.Int(300),
			Rrdatas:     pulumi.StringArray{address.Address},
		}, parent); err != nil {
			return nil, err
		}
	}

	// The publisher: it can write this bucket, nothing else.
	publisher, err := serviceaccount.NewAccount(ctx, name+"-publisher", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String("pydantic-opamp-packages"),
		DisplayName: pulumi.String(args.Repository + " package publishing"),
	}, parent)
	if err != nil {
		return nil, err
	}
	if _, err := storage.NewBucketIAMMember(ctx, name+"-publisher", &storage.BucketIAMMemberArgs{
		Bucket: bucket.Name,
		Role:   pulumi.String("roles/storage.objectAdmin"),
		Member: pulumi.Sprintf("serviceAccount:%s", publisher.Email),
	}, parent); err != nil {
		return nil, err
	}

	// Keyless: the Packages workflow's OIDC token becomes the publisher, from this repository's
	// packages environment alone (which only release tags can enter).
	pool, err := iam.NewWorkloadIdentityPool(ctx, name, &iam.WorkloadIdentityPoolArgs{
		WorkloadIdentityPoolId: pulumi.String("github-packages"),
		DisplayName:            pulumi.String("GitHub package publishing"),
	}, parent)
	if err != nil {
		return nil, err
	}
	provider, err := iam.NewWorkloadIdentityPoolProvider(ctx, name, &iam.WorkloadIdentityPoolProviderArgs{
		WorkloadIdentityPoolId:         pool.WorkloadIdentityPoolId,
		WorkloadIdentityPoolProviderId: pulumi.String("github"),
		AttributeMapping: pulumi.StringMap{
			"google.subject":        pulumi.String("assertion.sub"),
			"attribute.repository":  pulumi.String("assertion.repository"),
			"attribute.environment": pulumi.String("assertion.environment"),
		},
		AttributeCondition: pulumi.String(fmt.Sprintf(
			"assertion.repository == %q && assertion.environment == %q", args.Repository, environment)),
		Oidc: &iam.WorkloadIdentityPoolProviderOidcArgs{IssuerUri: pulumi.String(githubIssuer)},
	}, parent)
	if err != nil {
		return nil, err
	}
	if _, err := serviceaccount.NewIAMMember(ctx, name+"-workflow", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: publisher.Name,
		Role:             pulumi.String("roles/iam.workloadIdentityUser"),
		Member:           pulumi.Sprintf("principalSet://iam.googleapis.com/%s/attribute.repository/%s", pool.Name, args.Repository),
	}, parent); err != nil {
		return nil, err
	}

	// The environment, entered by release tags alone, and what the workflow reads from it.
	gh, err := github.NewProvider(ctx, name, &github.ProviderArgs{Owner: pulumi.String(owner)}, parent)
	if err != nil {
		return nil, err
	}
	onGitHub := []pulumi.ResourceOption{parent, pulumi.Provider(gh)}
	env, err := github.NewRepositoryEnvironment(ctx, name, &github.RepositoryEnvironmentArgs{
		Repository:  pulumi.String(repo),
		Environment: pulumi.String(environment),
		DeploymentBranchPolicy: &github.RepositoryEnvironmentDeploymentBranchPolicyArgs{
			ProtectedBranches:    pulumi.Bool(false),
			CustomBranchPolicies: pulumi.Bool(true),
		},
	}, onGitHub...)
	if err != nil {
		return nil, err
	}
	for i, tag := range releaseTags {
		if _, err := github.NewRepositoryEnvironmentDeploymentPolicy(ctx, fmt.Sprintf("%s-tag-%d", name, i),
			&github.RepositoryEnvironmentDeploymentPolicyArgs{
				Repository:  pulumi.String(repo),
				Environment: env.Environment,
				TagPattern:  pulumi.String(tag),
			}, onGitHub...); err != nil {
			return nil, err
		}
	}
	for variable, value := range map[string]pulumi.StringInput{
		"PACKAGES_BUCKET":                     pulumi.Sprintf("gs://%s", bucket.Name),
		"PACKAGES_URL":                        pulumi.String("https://" + args.Domain),
		"PACKAGES_SERVICE_ACCOUNT":            publisher.Email,
		"PACKAGES_WORKLOAD_IDENTITY_PROVIDER": provider.Name,
	} {
		if _, err := github.NewActionsEnvironmentVariable(ctx, name+"-"+strings.ToLower(variable), &github.ActionsEnvironmentVariableArgs{
			Repository:   pulumi.String(repo),
			Environment:  env.Environment,
			VariableName: pulumi.String(variable),
			Value:        value,
		}, onGitHub...); err != nil {
			return nil, err
		}
	}
	p.Bucket = bucket.Name
	p.Address = address.Address
	p.ServiceAccount = publisher.Email
	p.Provider = provider.Name
	return p, ctx.RegisterResourceOutputs(p, pulumi.Map{
		"bucket": p.Bucket, "address": p.Address, "serviceAccount": p.ServiceAccount, "provider": p.Provider,
	})
}
