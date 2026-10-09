package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

func regionsFake(t *testing.T) *fakeServer {
	f := startFakeServer(t)
	f.peRegions = append(f.peRegions, &serverv1.PrivateEndpointRegion{Provider: "gcp", Region: "us-central1"})
	f.providers = append(f.providers, &serverv1.DatabaseProvider{Name: "aws-use1-b", Cloud: "aws", Region: "us-east-1", Engines: []string{"postgresql"}})
	return f
}

func TestRegionsDataSource(t *testing.T) {
	f := regionsFake(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + `
data "relay_regions" "all" {}

data "relay_regions" "aws" {
  cloud = "aws"
}

data "relay_regions" "postgresql" {
  engine = "PostgreSQL"
}

data "relay_regions" "private" {
  private_endpoints = true
}

data "relay_regions" "public_only" {
  private_endpoints = false
}

data "relay_regions" "nowhere" {
  cloud  = "aws"
  engine = "cassandra"
}
`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.#", "3"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.0.cloud", "aws"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.0.region", "eu-west-1"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.0.engines.#", "2"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.0.engines.0", "mongodb"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.0.engines.1", "postgresql"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.0.private_endpoints", "false"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.1.region", "us-east-1"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.1.engines.#", "1"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.1.private_endpoints", "true"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.2.cloud", "gcp"),
				resource.TestCheckResourceAttr("data.relay_regions.all", "regions.2.engines.#", "0"),
				resource.TestCheckResourceAttr("data.relay_regions.aws", "regions.#", "2"),
				resource.TestCheckResourceAttr("data.relay_regions.postgresql", "regions.#", "2"),
				resource.TestCheckResourceAttr("data.relay_regions.private", "regions.#", "2"),
				resource.TestCheckResourceAttr("data.relay_regions.private", "regions.0.region", "us-east-1"),
				resource.TestCheckResourceAttr("data.relay_regions.public_only", "regions.#", "1"),
				resource.TestCheckResourceAttr("data.relay_regions.nowhere", "regions.#", "0"),
				func(*terraform.State) error {
					if calls := f.callsTo("ListPrivateEndpointRegions"); len(calls) == 0 || calls[0].org != testOrgID {
						return fmt.Errorf("the catalog must be read in the provider's organization: %+v", calls)
					}
					return nil
				},
			),
		}},
	})
}

func TestCloudProvidersDataSource(t *testing.T) {
	f := regionsFake(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + `data "relay_cloud_providers" "this" {}`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.relay_cloud_providers.this", "cloud_providers.#", "2"),
				resource.TestCheckResourceAttr("data.relay_cloud_providers.this", "cloud_providers.0.name", "aws"),
				resource.TestCheckResourceAttr("data.relay_cloud_providers.this", "cloud_providers.0.regions.#", "2"),
				resource.TestCheckResourceAttr("data.relay_cloud_providers.this", "cloud_providers.0.regions.0", "eu-west-1"),
				resource.TestCheckResourceAttr("data.relay_cloud_providers.this", "cloud_providers.0.regions.1", "us-east-1"),
				resource.TestCheckResourceAttr("data.relay_cloud_providers.this", "cloud_providers.1.name", "gcp"),
				resource.TestCheckResourceAttr("data.relay_cloud_providers.this", "cloud_providers.1.regions.0", "us-central1"),
			),
		}},
	})
}

func TestRegionDataSources_Failures(t *testing.T) {
	f := regionsFake(t)
	denied := status.Error(codes.PermissionDenied, "denied")
	steps := []resource.TestStep{}
	for _, method := range []string{"ListDatabaseProviders", "ListPrivateEndpointRegions"} {
		for ds, want := range map[string]string{"relay_regions": "Cannot list regions", "relay_cloud_providers": "Cannot list cloud providers"} {
			steps = append(steps, resource.TestStep{
				PreConfig: func() {
					f.failWith("ListDatabaseProviders", nil)
					f.failWith("ListPrivateEndpointRegions", nil)
					f.failWith(method, denied)
				},
				Config:      providerBlock(f) + fmt.Sprintf(`data %q "this" {}`, ds),
				ExpectError: regexp.MustCompile(want),
			})
		}
	}
	resource.UnitTest(t, resource.TestCase{ProtoV6ProviderFactories: factories(relay.Dial), Steps: steps})
}
