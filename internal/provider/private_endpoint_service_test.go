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

func serviceConfig(f *fakeServer, region, extra string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_private_endpoint_service" "this" {
  workspace_id = %q
  cloud        = "aws"
  region       = %q
%s
}
`, netWorkspace, region, extra)
}

func noServicesLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.peServices) != 0 {
			return fmt.Errorf("services left after destroy: %v", f.peServices)
		}
		return nil
	}
}

func TestPrivateEndpointServiceResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	f.initiatingFor = 2
	f.serviceBlocked = 2
	f.peRegions = append(f.peRegions, &serverv1.PrivateEndpointRegion{Provider: "aws", Region: "eu-west-1"})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noServicesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: serviceConfig(f, "us-east-1", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_private_endpoint_service.this", "id", "pes-1"),
					resource.TestCheckResourceAttr("relay_private_endpoint_service.this", "status", "AVAILABLE"),
					resource.TestCheckResourceAttr("relay_private_endpoint_service.this", "endpoint_service_name", "com.amazonaws.vpce.us-east-1.vpce-svc-0123456789abcdef0"),
					resource.TestCheckResourceAttr("relay_private_endpoint_service.this", "error_message", ""),
					resource.TestCheckResourceAttr("relay_private_endpoint_service.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						calls := f.callsTo("CreatePrivateEndpointService")
						if len(calls) != 1 || calls[0].workspace != netWorkspace || calls[0].org != testOrgID {
							return fmt.Errorf("create must be scoped to org and workspace: %+v", calls)
						}
						if n := len(f.callsTo("GetPrivateEndpointService")); n < 2 {
							return fmt.Errorf("create must wait while the service initiates, got %d gets", n)
						}
						return nil
					},
				),
			},
			{
				ResourceName:            "relay_private_endpoint_service.this",
				ImportState:             true,
				ImportStateId:           netWorkspace + "/aws/us-east-1",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{
				ResourceName:            "relay_private_endpoint_service.this",
				ImportState:             true,
				ImportStateId:           netWorkspace + "/pes-1",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{
				Config: serviceConfig(f, "us-east-1", `  timeouts { delete = "1m" }`) + fmt.Sprintf(`
data "relay_private_endpoint_service" "by_region" {
  workspace_id = %[1]q
  cloud        = "aws"
  region       = "us-east-1"
  depends_on   = [relay_private_endpoint_service.this]
}

data "relay_private_endpoint_service" "by_id" {
  workspace_id = %[1]q
  id           = relay_private_endpoint_service.this.id
}
`, netWorkspace),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.by_region", "id", "pes-1"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.by_id", "endpoint_service_name", "com.amazonaws.vpce.us-east-1.vpce-svc-0123456789abcdef0"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.by_id", "endpoints.#", "0"),
					func(*terraform.State) error {
						if n := len(f.callsTo("CreatePrivateEndpointService")); n != 1 {
							return fmt.Errorf("a timeouts-only change must not replace the service, got %d creates", n)
						}
						return nil
					},
				),
			},
			{
				Config: serviceConfig(f, "eu-west-1", ""),
				Check: func(*terraform.State) error {
					if n := len(f.callsTo("DeletePrivateEndpointService")); n != 3 {
						return fmt.Errorf("delete must retry while endpoints remain, got %d calls", n)
					}
					return nil
				},
			},
		},
	})
}

func TestPrivateEndpointServiceResource_Failures(t *testing.T) {
	f := startFakeServer(t)
	denied := status.Error(codes.PermissionDenied, "denied")
	east := serviceConfig(f, "us-east-1", "")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: providerBlock(f) + `resource "relay_private_endpoint_service" "this" {
  workspace_id = "ws-net"
  cloud        = "gcp"
  region       = "us-central1"
}`,
				ExpectError: regexp.MustCompile(`value must be one of: \["aws"\]`),
			},
			{
				Config:      serviceConfig(f, "ap-south-1", ""),
				ExpectError: regexp.MustCompile(`no private endpoint service is\s+available in aws/ap-south-1`),
			},
			{
				PreConfig: func() {
					f.set(func() { f.initiatingFor, f.failService = 1, "quota exceeded" })
				},
				Config:      east,
				ExpectError: regexp.MustCompile("(?s)did not become available.*quota exceeded"),
			},
			{
				PreConfig: func() {
					f.set(func() {
						f.peServices = map[string]*serverv1.PrivateEndpointService{}
						f.failService = ""
					})
				},
				Config: providerBlock(f),
			},
			{
				PreConfig:   func() { f.failWith("GetPrivateEndpointService", denied) },
				Config:      east,
				ExpectError: regexp.MustCompile("did not become available"),
			},
			{
				PreConfig: func() {
					f.set(func() { f.peServices, f.initiatingFor = map[string]*serverv1.PrivateEndpointService{}, 0 })
					f.failWith("GetPrivateEndpointService", nil)
				},
				Config: east,
			},
			{
				PreConfig:   func() { f.failWith("GetPrivateEndpointService", denied) },
				Config:      east,
				ExpectError: regexp.MustCompile("Cannot read private endpoint service"),
			},
			{
				PreConfig: func() {
					f.failWith("GetPrivateEndpointService", nil)
					f.set(func() { f.peServices = map[string]*serverv1.PrivateEndpointService{} })
				},
				Config:             east,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: serviceConfig(f, "us-east-1", `  timeouts { delete = "1ms" }`),
			},
			{
				PreConfig:   func() { f.set(func() { f.serviceBlocked = 1 << 20 }) },
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("(?s)Cannot delete private endpoint service.*timed out"),
			},
			{
				PreConfig: func() {
					f.set(func() { f.serviceBlocked = 0 })
					f.failWith("DeletePrivateEndpointService", denied)
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot delete private endpoint service"),
			},
			{
				PreConfig: func() { f.failWith("DeletePrivateEndpointService", nil) },
				Config:    east,
			},
			{
				PreConfig: func() { f.failWith("DeletePrivateEndpointService", status.Error(codes.NotFound, "gone")) },
				Config:    providerBlock(f),
			},
		},
	})
}

func TestPrivateEndpointServiceResource_ImportErrors(t *testing.T) {
	f := startFakeServer(t)
	config := serviceConfig(f, "us-east-1", "")
	step := func(id, want string) resource.TestStep {
		return resource.TestStep{
			Config: config, ResourceName: "relay_private_endpoint_service.this",
			ImportState: true, ImportStateId: id, ExpectError: regexp.MustCompile(want),
		}
	}
	listFails := step(netWorkspace+"/aws/us-east-1", "Cannot list private endpoint services")
	listFails.PreConfig = func() { f.failWith("ListPrivateEndpointServices", status.Error(codes.PermissionDenied, "denied")) }
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			step("no-slash", "Invalid import ID"),
			step(netWorkspace+"//us-east-1", "Invalid import ID"),
			step("a/b/c/d", "Invalid import ID"),
			step(netWorkspace+"/aws/eu-west-1", "has no service in aws/eu-west-1"),
			listFails,
		},
	})
}

func TestPrivateEndpointServiceDataSource(t *testing.T) {
	f := startFakeServer(t)
	f.peServices["pes-seeded"] = &serverv1.PrivateEndpointService{
		Id: "pes-seeded", Provider: "aws", Region: "us-east-1", Status: "AVAILABLE", CreatedBy: netWorkspace,
		CreatedAt: timestamppb.New(fakeEpoch),
		Endpoints: []*serverv1.PrivateEndpoint{{
			Id: "pe-1", ServiceId: "pes-seeded", Index: 3, InterfaceEndpointId: "vpce-0123456789abcdef0",
			ConnectionStatus: "AVAILABLE", DnsName: "vpce-0123.vpce-svc.us-east-1.vpce.amazonaws.com",
		}},
	}
	ds := func(body string) string {
		return providerBlock(f) + fmt.Sprintf(`
data "relay_private_endpoint_service" "this" {
  workspace_id = %q
%s
}
`, netWorkspace, body)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: ds(`  id = "pes-seeded"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "cloud", "aws"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "endpoints.#", "1"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "endpoints.0.vpc_endpoint_id", "vpce-0123456789abcdef0"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "endpoints.0.index", "3"),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "endpoints.0.connection_status", "AVAILABLE"),
				),
			},
			{
				Config:      ds(`  cloud = "aws"`),
				ExpectError: regexp.MustCompile("Invalid Attribute Combination"),
			},
			{
				Config:      ds("  id = \"pes-seeded\"\n  cloud = \"aws\"\n  region = \"us-east-1\""),
				ExpectError: regexp.MustCompile("Invalid Attribute Combination"),
			},
			{
				Config:      ds("  cloud = \"aws\"\n  region = \"eu-west-1\""),
				ExpectError: regexp.MustCompile("Private endpoint service not found"),
			},
			{
				Config:      ds(`  id = "pes-missing"`),
				ExpectError: regexp.MustCompile("Private endpoint service not found"),
			},
			{
				PreConfig:   func() { f.failWith("ListPrivateEndpointServices", status.Error(codes.PermissionDenied, "denied")) },
				Config:      ds("  cloud = \"aws\"\n  region = \"us-east-1\""),
				ExpectError: regexp.MustCompile("Cannot read private endpoint service"),
			},
		},
	})
}
