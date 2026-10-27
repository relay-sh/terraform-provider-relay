package provider

import (
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const (
	integrationAddr = "relay_cloud_integration.this"
	roleA           = "arn:aws:iam::123456789012:role/relay-backups"
	roleB           = "arn:aws:iam::210987654321:role/relay-backups"
)

func integrationConfig(f *fakeServer, name, role string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_cloud_integration" "this" {
  workspace_id = %q
  name         = %q
  role_arn     = %q
}
`, ciWorkspace, name, role)
}

func callCount(f *fakeServer, method string, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := len(f.callsTo(method)); got != want {
			return fmt.Errorf("%s called %d times, want %d", method, got, want)
		}
		return nil
	}
}

func allScoped(f *fakeServer, methods ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		for _, m := range methods {
			for _, c := range f.callsTo(m) {
				if c.workspace != ciWorkspace {
					return fmt.Errorf("%s scoped to workspace %q, want %q", m, c.workspace, ciWorkspace)
				}
			}
		}
		return nil
	}
}

func TestCloudIntegrationSetupDataSource(t *testing.T) {
	f := startFakeServer(t)
	ds := func(body string) string {
		return providerBlock(f) + fmt.Sprintf(`
data "relay_cloud_integration_setup" "this" {
  workspace_id = %q
%s
}
`, ciWorkspace, body)
	}
	const addr = "data.relay_cloud_integration_setup.this"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: ds(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "cloud", "aws"),
					resource.TestCheckResourceAttr(addr, "relay_aws_account_id", "111122223333"),
					resource.TestCheckResourceAttr(addr, "external_id", "relay-"+testOrgID),
					resource.TestMatchResourceAttr(addr, "trust_policy", regexp.MustCompile(`arn:aws:iam::111122223333:root.*relay-`+testOrgID)),
					resource.TestCheckResourceAttr(addr, "permissions_policy", fakePermissionsPolicy),
					allScoped(f, "GetCloudIntegrationSetup"),
				),
			},
			{
				Config: ds(`  cloud = "aws"`),
				Check:  resource.TestCheckResourceAttr(addr, "cloud", "aws"),
			},
			{
				Config:      ds(`  cloud = "gcp"`),
				ExpectError: regexp.MustCompile("Invalid Attribute Value Match"),
			},
			{
				PreConfig:   func() { f.set(func() { f.relayAWSAccountID = "" }) },
				Config:      ds(""),
				ExpectError: regexp.MustCompile("Cloud integrations are not available(.|\n)*not configured on this server"),
			},
			{
				PreConfig:   func() { f.failWith("GetCloudIntegrationSetup", status.Error(codes.PermissionDenied, "denied")) },
				Config:      ds(""),
				ExpectError: regexp.MustCompile("Cannot read cloud integration setup"),
			},
		},
	})
}

func TestCloudIntegrationResource(t *testing.T) {
	f := startFakeServer(t)
	var firstID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: integrationConfig(f, "backups", roleA),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(integrationAddr, "id", uuidPattern),
					resource.TestCheckResourceAttr(integrationAddr, "cloud", "aws"),
					resource.TestCheckResourceAttr(integrationAddr, "aws_account_id", "123456789012"),
					resource.TestCheckResourceAttr(integrationAddr, "external_id", "relay-"+testOrgID),
					resource.TestCheckResourceAttr(integrationAddr, "status", "connected"),
					resource.TestCheckResourceAttr(integrationAddr, "last_error", ""),
					resource.TestCheckResourceAttrWith(integrationAddr, "id", func(v string) error { firstID = v; return nil }),
					callCount(f, "TestCloudIntegration", 1),
					allScoped(f, "CreateCloudIntegration", "TestCloudIntegration", "GetCloudIntegration"),
				),
			},
			{
				Config: integrationConfig(f, "exports", roleA),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(integrationAddr, "name", "exports"),
					resource.TestCheckResourceAttr(integrationAddr, "status", "connected"),
					callCount(f, "TestCloudIntegration", 1),
					allScoped(f, "UpdateCloudIntegration"),
				),
			},
			{
				PreConfig: func() { f.set(func() { f.assumeRoleError = "AccessDenied: not authorized to perform sts:AssumeRole" }) },
				Config:    integrationConfig(f, "exports", roleB),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(integrationAddr, "role_arn", roleB),
					resource.TestCheckResourceAttr(integrationAddr, "aws_account_id", "210987654321"),
					resource.TestCheckResourceAttr(integrationAddr, "status", "error"),
					resource.TestCheckResourceAttr(integrationAddr, "last_error", "AccessDenied: not authorized to perform sts:AssumeRole"),
					callCount(f, "TestCloudIntegration", 2),
				),
			},
			{
				ResourceName:      integrationAddr,
				ImportState:       true,
				ImportStateIdFunc: workspaceScopedID(integrationAddr, "id"),
				ImportStateVerify: true,
			},
			{
				ResourceName:  integrationAddr,
				ImportState:   true,
				ImportStateId: ciWorkspace + "/not-a-uuid",
				ExpectError:   regexp.MustCompile("Invalid import ID"),
			},
			{
				ResourceName:  integrationAddr,
				ImportState:   true,
				ImportStateId: "00000000-0000-4000-8000-000000000001",
				ExpectError:   regexp.MustCompile("Invalid import ID"),
			},
			{
				PreConfig: func() {
					f.set(func() {
						f.cloudIntegrations = nil
						f.assumeRoleError = ""
						f.relayAWSAccountID = ""
					})
				},
				Config: integrationConfig(f, "exports", roleB),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(integrationAddr, "id", func(v string) error {
						if v == firstID {
							return fmt.Errorf("integration deleted out of band was not recreated")
						}
						return nil
					}),
					resource.TestCheckResourceAttr(integrationAddr, "status", "pending"),
				),
			},
			{
				PreConfig: func() {
					f.set(func() {
						f.relayAWSAccountID = "111122223333"
						f.autoExport = &serverv1.BackupAutoExport{IntegrationId: f.cloudIntegrations[0].GetId(), Enabled: false}
					})
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile(`Cloud integration is still in use(.|\n)*still used by one or more exports(.|\n)*integration_id`),
			},
			{
				PreConfig:   func() { f.set(func() { f.autoExport = nil }); f.failWith("DeleteCloudIntegration", status.Error(codes.Internal, "boom")) },
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot delete cloud integration"),
			},
			{
				PreConfig: func() { f.failWith("DeleteCloudIntegration", nil) },
				Config:    providerBlock(f),
				Check: func(*terraform.State) error {
					if len(f.cloudIntegrations) != 0 {
						return fmt.Errorf("integration was not deleted")
					}
					return nil
				},
			},
		},
	})
}

func TestCloudIntegrationResourceErrors(t *testing.T) {
	f := startFakeServer(t)
	f.cloudIntegrations = []*serverv1.CloudIntegration{{Id: "c1000000-0000-4000-8000-000000000009", Provider: "aws", Name: "taken"}}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:      integrationConfig(f, "backups", "arn:aws:iam::123:role/x"),
				ExpectError: regexp.MustCompile("must look like arn:aws:iam::123456789012:role/name"),
			},
			{
				Config:      integrationConfig(f, " backups", roleA),
				ExpectError: regexp.MustCompile("must not start or end with whitespace"),
			},
			{
				Config:      integrationConfig(f, "taken", roleA),
				ExpectError: regexp.MustCompile("Cannot create cloud integration(.|\n)*AlreadyExists"),
			},
			{
				Config: integrationConfig(f, "backups", roleA),
			},
			{
				Config:      integrationConfig(f, "taken", roleA),
				ExpectError: regexp.MustCompile("Cannot update cloud integration(.|\n)*AlreadyExists"),
			},
			{
				PreConfig:   func() { f.failWith("GetCloudIntegration", status.Error(codes.PermissionDenied, "denied")) },
				Config:      integrationConfig(f, "backups", roleA),
				ExpectError: regexp.MustCompile("Cannot read cloud integration"),
			},
			{
				PreConfig: func() {
					f.failWith("GetCloudIntegration", nil)
					f.set(func() {
						f.cloudIntegrations = slices.DeleteFunc(f.cloudIntegrations, func(c *serverv1.CloudIntegration) bool { return c.GetName() == "backups" })
					})
				},
				Config:             integrationConfig(f, "backups", roleA),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
