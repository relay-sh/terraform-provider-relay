package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const (
	vpceA = "vpce-0123456789abcdef0"
	vpceB = "vpce-0fedcba9"
)

func endpointConfig(f *fakeServer, vpce, extra string) string {
	return serviceConfig(f, "us-east-1", "") + fmt.Sprintf(`
resource "relay_private_endpoint" "this" {
  workspace_id    = %q
  service_id      = relay_private_endpoint_service.this.id
  vpc_endpoint_id = %q
%s
}
`, netWorkspace, vpce, extra)
}

func TestPrivateEndpointResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	f.pendingFor = 2
	f.deletingFor = 2
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noServicesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: endpointConfig(f, vpceA, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "id", "pe-2"),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "service_id", "pes-1"),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "connection_status", "AVAILABLE"),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "dns_name", vpceA+".vpce-svc.us-east-1.vpce.amazonaws.com"),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "index", "2"),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "cloud", "aws"),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "region", "us-east-1"),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "error_message", ""),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						calls := f.callsTo("CreatePrivateEndpoint")
						if len(calls) != 1 || calls[0].workspace != netWorkspace || calls[0].org != testOrgID {
							return fmt.Errorf("register must be scoped to org and workspace: %+v", calls)
						}
						if n := len(f.callsTo("GetPrivateEndpoint")); n < 3 {
							return fmt.Errorf("create must wait while the connection is pending, got %d gets", n)
						}
						return nil
					},
				),
			},
			{
				ResourceName:            "relay_private_endpoint.this",
				ImportState:             true,
				ImportStateId:           netWorkspace + "/pes-1/pe-2",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{
				ResourceName:            "relay_private_endpoint.this",
				ImportState:             true,
				ImportStateId:           netWorkspace + "/pes-1/" + vpceA,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{
				Config: endpointConfig(f, vpceA, `  timeouts { delete = "1m" }`) + fmt.Sprintf(`
data "relay_private_endpoint" "by_id" {
  workspace_id = %[1]q
  service_id   = relay_private_endpoint.this.service_id
  id           = relay_private_endpoint.this.id
}

data "relay_private_endpoint" "by_vpce" {
  workspace_id    = %[1]q
  service_id      = relay_private_endpoint.this.service_id
  vpc_endpoint_id = relay_private_endpoint.this.vpc_endpoint_id
}

data "relay_private_endpoint_service" "this" {
  workspace_id = %[1]q
  id           = relay_private_endpoint.this.service_id
}
`, netWorkspace),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.relay_private_endpoint.by_id", "vpc_endpoint_id", vpceA),
					resource.TestCheckResourceAttr("data.relay_private_endpoint.by_vpce", "id", "pe-2"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint.by_vpce", "connection_status", "AVAILABLE"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "endpoints.#", "1"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "endpoints.0.id", "pe-2"),
					func(*terraform.State) error {
						if n := len(f.callsTo("CreatePrivateEndpoint")); n != 1 {
							return fmt.Errorf("a timeouts-only change must not replace the endpoint, got %d registrations", n)
						}
						return nil
					},
				),
			},
			{
				Config: endpointConfig(f, vpceB, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "vpc_endpoint_id", vpceB),
					resource.TestCheckResourceAttr("relay_private_endpoint.this", "id", "pe-3"),
					func(*terraform.State) error {
						if n := len(f.callsTo("DeletePrivateEndpoint")); n != 1 {
							return fmt.Errorf("a new vpc_endpoint_id must replace the registration, got %d deletes", n)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestPrivateEndpointResource_Failures(t *testing.T) {
	f := startFakeServer(t)
	denied := status.Error(codes.PermissionDenied, "denied")
	notFound := status.Error(codes.NotFound, "gone")
	clearEndpoints := func() {
		f.set(func() {
			for _, s := range f.peServices {
				s.Endpoints = nil
			}
		})
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:      endpointConfig(f, "vpc-0123", ""),
				ExpectError: regexp.MustCompile(`must be an AWS VPC endpoint ID`),
			},
			{
				PreConfig:   func() { f.failWith("CreatePrivateEndpoint", denied) },
				Config:      endpointConfig(f, vpceA, ""),
				ExpectError: regexp.MustCompile("Cannot register private endpoint"),
			},
			{
				PreConfig: func() {
					f.failWith("CreatePrivateEndpoint", nil)
					f.set(func() { f.rejectEndpoint = "the connection was rejected on the service" })
				},
				Config:      endpointConfig(f, vpceA, ""),
				ExpectError: regexp.MustCompile(`(?s)REJECTED: the connection was rejected on the service.*replace\s+the\s+aws_vpc_endpoint`),
			},
			{
				PreConfig: func() {
					clearEndpoints()
					f.set(func() { f.rejectEndpoint, f.pendingFor = "", 1<<20 })
				},
				Config:      endpointConfig(f, vpceA, `  timeouts { create = "1ms" }`),
				ExpectError: regexp.MustCompile(`(?s)timed out.*must exist and request a connection`),
			},
			{
				PreConfig: func() {
					clearEndpoints()
					f.set(func() { f.pendingFor = 0 })
				},
				Config: serviceConfig(f, "us-east-1", ""),
			},
			{
				PreConfig:   func() { f.failWith("GetPrivateEndpoint", denied) },
				Config:      endpointConfig(f, vpceA, ""),
				ExpectError: regexp.MustCompile("did not become available"),
			},
			{
				PreConfig: func() {
					clearEndpoints()
					f.failWith("GetPrivateEndpoint", nil)
				},
				Config: endpointConfig(f, vpceA, ""),
			},
			{
				PreConfig:   func() { f.failWith("GetPrivateEndpoint", denied) },
				Config:      endpointConfig(f, vpceA, ""),
				ExpectError: regexp.MustCompile("Cannot read private endpoint"),
			},
			{
				PreConfig: func() {
					f.failWith("GetPrivateEndpoint", nil)
					clearEndpoints()
				},
				Config:             endpointConfig(f, vpceA, ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: endpointConfig(f, vpceA, ""),
			},
			{
				PreConfig:   func() { f.failWith("DeletePrivateEndpoint", denied) },
				Config:      serviceConfig(f, "us-east-1", ""),
				ExpectError: regexp.MustCompile("Cannot delete private endpoint"),
			},
			{
				PreConfig: func() {
					f.failWith("DeletePrivateEndpoint", nil)
					f.set(func() { f.deletingFor = 1 })
					f.failAfter("GetPrivateEndpoint", 1, denied)
				},
				Config:      serviceConfig(f, "us-east-1", ""),
				ExpectError: regexp.MustCompile("Private endpoint was not deleted"),
			},
			{
				PreConfig: func() {
					f.failWith("GetPrivateEndpoint", nil)
					f.set(func() { f.deletingFor = 0 })
				},
				Config: serviceConfig(f, "us-east-1", ""),
			},
			{
				Config: endpointConfig(f, vpceA, ""),
			},
			{
				PreConfig: func() { f.failWith("DeletePrivateEndpoint", notFound) },
				Config:    serviceConfig(f, "us-east-1", ""),
				Check: func(*terraform.State) error {
					clearEndpoints()
					return nil
				},
			},
		},
	})
}

func TestPrivateEndpointResource_ImportErrors(t *testing.T) {
	f := startFakeServer(t)
	f.peServices["pes-seeded"] = &serverv1.PrivateEndpointService{Id: "pes-seeded", Provider: "aws", Region: "us-east-1", CreatedBy: netWorkspace}
	config := providerBlock(f) + fmt.Sprintf(`
resource "relay_private_endpoint" "this" {
  workspace_id    = %q
  service_id      = "pes-seeded"
  vpc_endpoint_id = %q
}
`, netWorkspace, vpceA)
	step := func(id, want string) resource.TestStep {
		return resource.TestStep{
			Config: config, ResourceName: "relay_private_endpoint.this",
			ImportState: true, ImportStateId: id, ExpectError: regexp.MustCompile(want),
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			step("a/b", "Invalid import ID"),
			step(netWorkspace+"//pe-1", "Invalid import ID"),
			step(netWorkspace+"/pes-seeded/"+vpceA, "is not registered on service pes-seeded"),
			step(netWorkspace+"/pes-missing/"+vpceA, "Cannot read private endpoint service"),
		},
	})
}

func TestPrivateEndpointDataSource_Failures(t *testing.T) {
	f := startFakeServer(t)
	f.peServices["pes-seeded"] = &serverv1.PrivateEndpointService{
		Id: "pes-seeded", Provider: "aws", Region: "us-east-1", CreatedBy: netWorkspace,
		Endpoints: []*serverv1.PrivateEndpoint{{Id: "pe-1", ServiceId: "pes-seeded", InterfaceEndpointId: vpceA, CreatedAt: timestamppb.New(fakeEpoch)}},
	}
	ds := func(body string) string {
		return providerBlock(f) + fmt.Sprintf(`
data "relay_private_endpoint" "this" {
  workspace_id = %q
  service_id   = "pes-seeded"
%s
}
`, netWorkspace, body)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:      ds("  id = \"pe-1\"\n  vpc_endpoint_id = \"" + vpceA + "\""),
				ExpectError: regexp.MustCompile("Invalid Attribute Combination"),
			},
			{
				Config:      ds(`  vpc_endpoint_id = "` + vpceB + `"`),
				ExpectError: regexp.MustCompile("Private endpoint not found"),
			},
			{
				Config:      ds(`  id = "pe-missing"`),
				ExpectError: regexp.MustCompile("Private endpoint not found"),
			},
			{
				PreConfig:   func() { f.failWith("GetPrivateEndpointService", status.Error(codes.PermissionDenied, "denied")) },
				Config:      ds(`  vpc_endpoint_id = "` + vpceA + `"`),
				ExpectError: regexp.MustCompile("Cannot read private endpoint"),
			},
		},
	})
}
