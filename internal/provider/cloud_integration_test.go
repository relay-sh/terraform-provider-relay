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

const ciWorkspace = "ws-ci"

func TestCloudIntegrationDataSource(t *testing.T) {
	f := startFakeServer(t)
	f.cloudIntegrations = []*serverv1.CloudIntegration{
		{
			Id: "c1000000-0000-4000-8000-000000000001", Provider: "aws", Name: "backups", AwsAccountId: "123456789012",
			RoleArn: "arn:aws:iam::123456789012:role/relay-backups", ExternalId: "relay-org-1",
			Status: "error", LastError: "AccessDenied",
		},
		{Id: "ci-2", Provider: "aws", Name: "dup"},
		{Id: "ci-3", Provider: "aws", Name: "dup"},
	}
	ds := func(body string) string {
		return providerBlock(f) + fmt.Sprintf(`
data "relay_cloud_integration" "this" {
  workspace_id = %q
%s
}
`, ciWorkspace, body)
	}
	const addr = "data.relay_cloud_integration.this"
	scopedTo := func(method string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			calls := f.callsTo(method)
			if len(calls) == 0 {
				return fmt.Errorf("%s was not called", method)
			}
			for _, c := range calls {
				if c.workspace != ciWorkspace {
					return fmt.Errorf("%s scoped to workspace %q, want %q", method, c.workspace, ciWorkspace)
				}
			}
			return nil
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: ds(`  name = "backups"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", "c1000000-0000-4000-8000-000000000001"),
					resource.TestCheckResourceAttr(addr, "cloud", "aws"),
					resource.TestCheckResourceAttr(addr, "aws_account_id", "123456789012"),
					resource.TestCheckResourceAttr(addr, "role_arn", "arn:aws:iam::123456789012:role/relay-backups"),
					resource.TestCheckResourceAttr(addr, "external_id", "relay-org-1"),
					resource.TestCheckResourceAttr(addr, "status", "error"),
					resource.TestCheckResourceAttr(addr, "last_error", "AccessDenied"),
					scopedTo("ListCloudIntegrations"),
				),
			},
			{
				Config: ds(`  id = "c1000000-0000-4000-8000-000000000001"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "name", "backups"),
					resource.TestCheckResourceAttr(addr, "workspace_id", ciWorkspace),
					scopedTo("GetCloudIntegration"),
				),
			},
			{
				Config:      ds("  id = \"c1000000-0000-4000-8000-000000000001\"\n  name = \"backups\""),
				ExpectError: regexp.MustCompile("Invalid Attribute Combination"),
			},
			{
				Config:      ds(""),
				ExpectError: regexp.MustCompile("Missing Attribute Configuration"),
			},
			{
				Config:      ds(`  name = "dup"`),
				ExpectError: regexp.MustCompile("Cloud integration name is ambiguous"),
			},
			{
				Config:      ds(`  name = "missing"`),
				ExpectError: regexp.MustCompile("Cloud integration not found"),
			},
			{
				Config:      ds(`  id = "c1000000-0000-4000-8000-0000000000ff"`),
				ExpectError: regexp.MustCompile("Cloud integration not found"),
			},
			{
				PreConfig:   func() { f.failWith("GetCloudIntegration", status.Error(codes.PermissionDenied, "denied")) },
				Config:      ds(`  id = "c1000000-0000-4000-8000-000000000001"`),
				ExpectError: regexp.MustCompile("Cannot read cloud integration"),
			},
			{
				PreConfig:   func() { f.failWith("ListCloudIntegrations", status.Error(codes.PermissionDenied, "denied")) },
				Config:      ds(`  name = "backups"`),
				ExpectError: regexp.MustCompile("Cannot list cloud integrations"),
			},
		},
	})
}
