// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package main

import (
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// made is a resource the program asked for, as the mocks saw it.
type made struct {
	typ, name string
	inputs    resource.PropertyMap
}

// mocks records every resource and answers with its inputs, plus the outputs GCP would compute.
type mocks struct {
	mu   sync.Mutex
	made []made
}

func (m *mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.made = append(m.made, made{args.TypeToken, args.Name, args.Inputs})
	m.mu.Unlock()
	outputs := args.Inputs.Copy()
	computed := map[string]resource.PropertyMap{
		"gcp:compute/globalAddress:GlobalAddress": {"address": resource.NewStringProperty("203.0.113.7")},
		"gcp:serviceaccount/account:Account": {
			"email": resource.NewStringProperty("pydantic-opamp-packages@project.iam.gserviceaccount.com"),
			"name":  resource.NewStringProperty("projects/project/serviceAccounts/pydantic-opamp-packages"),
		},
		"gcp:iam/workloadIdentityPool:WorkloadIdentityPool": {
			"name": resource.NewStringProperty("projects/123/locations/global/workloadIdentityPools/github-packages"),
		},
		"gcp:iam/workloadIdentityPoolProvider:WorkloadIdentityPoolProvider": {
			"name": resource.NewStringProperty("projects/123/locations/global/workloadIdentityPools/github-packages/providers/github"),
		},
	}
	for k, v := range computed[args.TypeToken] {
		outputs[resource.PropertyKey(k)] = v
	}
	outputs["selfLink"] = resource.NewStringProperty("https://compute/" + args.Name)
	return args.Name + "-id", outputs, nil
}

func (m *mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) { return args.Args, nil }

// run makes the publishing side with args under the mocks, and returns what it asked for.
func run(t *testing.T, args PublishingArgs) []made {
	t.Helper()
	m := &mocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := NewPublishing(ctx, "packages", args)
		return err
	}, pulumi.WithMocks("pydantic-opamp-packages", "test", m))
	require.NoError(t, err)
	return m.made
}

func of(all []made, typ string) []made {
	var found []made
	for _, r := range all {
		if r.typ == typ {
			found = append(found, r)
		}
	}
	return found
}

func str(r made, key string) string {
	v := r.inputs[resource.PropertyKey(key)]
	if v.IsString() {
		return v.StringValue()
	}
	return ""
}

const (
	bucketIAM = "gcp:storage/bucketIAMMember:BucketIAMMember"
	envVar    = "github:index/actionsEnvironmentVariable:ActionsEnvironmentVariable"
)

// Anyone may read the bucket; the publisher alone may write it, and nothing beyond it.
func TestPublishingBucketIsReadByAnyoneAndWrittenByThePublisherAlone(t *testing.T) {
	all := run(t, PublishingArgs{})
	buckets := of(all, "gcp:storage/bucket:Bucket")
	require.Len(t, buckets, 1)
	assert.Equal(t, "pydantic-opamp-packages", str(buckets[0], "name"))
	assert.True(t, buckets[0].inputs["uniformBucketLevelAccess"].BoolValue())

	grants := map[string]string{}
	for _, r := range of(all, bucketIAM) {
		grants[str(r, "member")] = str(r, "role")
	}
	assert.Equal(t, map[string]string{
		"allUsers": "roles/storage.objectViewer",
		"serviceAccount:pydantic-opamp-packages@project.iam.gserviceaccount.com": "roles/storage.objectAdmin",
	}, grants)
	for _, typ := range []string{"gcp:projects/iAMMember:IAMMember", "gcp:projects/iAMBinding:IAMBinding"} {
		assert.Empty(t, of(all, typ), "a project-wide grant")
	}
}

// Only the Packages workflow, in this repository's packages environment, becomes the publisher.
func TestPublishingTrustsThePackagesEnvironmentAlone(t *testing.T) {
	all := run(t, PublishingArgs{})
	providers := of(all, "gcp:iam/workloadIdentityPoolProvider:WorkloadIdentityPoolProvider")
	require.Len(t, providers, 1)
	assert.Equal(t, `assertion.repository == "pydantic/opamp-clients" && assertion.environment == "packages"`,
		str(providers[0], "attributeCondition"))
	assert.Equal(t, githubIssuer, providers[0].inputs["oidc"].ObjectValue()["issuerUri"].StringValue())

	members := of(all, "gcp:serviceaccount/iAMMember:IAMMember")
	require.Len(t, members, 1)
	assert.Equal(t, "roles/iam.workloadIdentityUser", str(members[0], "role"))
	assert.Equal(t, "principalSet://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/"+
		"github-packages/attribute.repository/pydantic/opamp-clients", str(members[0], "member"))

	// The environment admits release tags, not branches.
	envs := of(all, "github:index/repositoryEnvironment:RepositoryEnvironment")
	require.Len(t, envs, 1)
	assert.Equal(t, "opamp-clients", str(envs[0], "repository"))
	policy := envs[0].inputs["deploymentBranchPolicy"].ObjectValue()
	assert.False(t, policy["protectedBranches"].BoolValue())
	assert.True(t, policy["customBranchPolicies"].BoolValue())
	var tags []string
	for _, r := range of(all, "github:index/repositoryEnvironmentDeploymentPolicy:RepositoryEnvironmentDeploymentPolicy") {
		tags = append(tags, str(r, "tagPattern"))
		assert.Empty(t, str(r, "branchPattern"))
	}
	assert.ElementsMatch(t, releaseTags, tags)
}

// The CDN serves the bucket at the domain, keeping publish.sh's Cache-Control.
func TestPublishingServesTheBucketAtTheDomain(t *testing.T) {
	all := run(t, PublishingArgs{})
	backends := of(all, "gcp:compute/backendBucket:BackendBucket")
	require.Len(t, backends, 1)
	assert.True(t, backends[0].inputs["enableCdn"].BoolValue())
	assert.Equal(t, "USE_ORIGIN_HEADERS", backends[0].inputs["cdnPolicy"].ObjectValue()["cacheMode"].StringValue())

	certs := of(all, "gcp:compute/managedSslCertificate:ManagedSslCertificate")
	require.Len(t, certs, 1)
	domains := certs[0].inputs["managed"].ObjectValue()["domains"].ArrayValue()
	require.Len(t, domains, 1)
	assert.Equal(t, "packages.pydantic.dev", domains[0].StringValue())

	rules := of(all, "gcp:compute/globalForwardingRule:GlobalForwardingRule")
	require.Len(t, rules, 1)
	assert.Equal(t, "443", str(rules[0], "portRange"))
	assert.Equal(t, "203.0.113.7", str(rules[0], "ipAddress"))
}

// The workflow learns where to publish from the environment's variables.
func TestPublishingTellsTheWorkflowWhereToPublish(t *testing.T) {
	vars := map[string]string{}
	for _, r := range of(run(t, PublishingArgs{}), envVar) {
		assert.Equal(t, "packages", str(r, "environment"))
		vars[str(r, "variableName")] = str(r, "value")
	}
	assert.Equal(t, map[string]string{
		"PACKAGES_BUCKET":          "gs://pydantic-opamp-packages",
		"PACKAGES_URL":             "https://packages.pydantic.dev",
		"PACKAGES_SERVICE_ACCOUNT": "pydantic-opamp-packages@project.iam.gserviceaccount.com",
		"PACKAGES_WORKLOAD_IDENTITY_PROVIDER": "projects/123/locations/global/workloadIdentityPools/" +
			"github-packages/providers/github",
	}, vars)
}

// A DNS record only with a zone to put it in; otherwise the address is exported to point at.
func TestPublishingMakesTheDNSRecordOnlyInAZone(t *testing.T) {
	assert.Empty(t, of(run(t, PublishingArgs{}), "gcp:dns/recordSet:RecordSet"))

	records := of(run(t, PublishingArgs{DNSZone: "pydantic-dev", Domain: "packages.example.com"}), "gcp:dns/recordSet:RecordSet")
	require.Len(t, records, 1)
	assert.Equal(t, "pydantic-dev", str(records[0], "managedZone"))
	assert.Equal(t, "packages.example.com.", str(records[0], "name"))
	assert.Equal(t, "A", str(records[0], "type"))
	addresses := records[0].inputs["rrdatas"].ArrayValue()
	require.Len(t, addresses, 1)
	assert.Equal(t, "203.0.113.7", addresses[0].StringValue())
}

func TestPublishingRefusesARepositoryWithoutAnOwner(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := NewPublishing(ctx, "packages", PublishingArgs{Repository: "opamp"})
		return err
	}, pulumi.WithMocks("pydantic-opamp-packages", "test", &mocks{}))
	assert.ErrorContains(t, err, `repository "opamp" isn't owner/name`)
}
