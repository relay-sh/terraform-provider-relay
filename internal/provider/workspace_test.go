package provider

import (
	"errors"
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

func workspaceConfig(f *fakeServer, name, description string) string {
	desc := ""
	if description != "" {
		desc = fmt.Sprintf("  description = %q\n", description)
	}
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_workspace" "this" {
  name = %q
%s}
`, name, desc)
}

func noWorkspacesLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.workspaces) != 0 {
			return fmt.Errorf("workspaces left after destroy: %v", f.workspaces)
		}
		return nil
	}
}

func TestWorkspaceResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noWorkspacesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: workspaceConfig(f, "prod", "Production"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("relay_workspace.this", "id"),
					resource.TestCheckResourceAttr("relay_workspace.this", "name", "prod"),
					resource.TestCheckResourceAttr("relay_workspace.this", "description", "Production"),
					resource.TestCheckResourceAttr("relay_workspace.this", "slug", "prod"),
					resource.TestCheckResourceAttr("relay_workspace.this", "is_default", "false"),
					resource.TestCheckResourceAttr("relay_workspace.this", "managed_by", "terraform"),
					func(*terraform.State) error {
						calls := f.callsTo("CreateWorkspace")
						if len(calls) != 1 || calls[0].org != testOrgID || calls[0].workspace != "" {
							return fmt.Errorf("create must be org-scoped only: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config: workspaceConfig(f, "production", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_workspace.this", "name", "production"),
					resource.TestCheckResourceAttr("relay_workspace.this", "description", ""),
					func(*terraform.State) error {
						if len(f.callsTo("CreateWorkspace")) != 1 {
							return errors.New("a rename must update in place, not replace")
						}
						return nil
					},
				),
			},
			{
				ResourceName:      "relay_workspace.this",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestWorkspaceResource_DriftAndFailures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: workspaceConfig(f, "prod", "")},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for id := range f.workspaces {
						delete(f.workspaces, id)
					}
				},
				Config:             workspaceConfig(f, "prod", ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: workspaceConfig(f, "prod", "")},
			{
				PreConfig:   func() { f.failWith("GetWorkspace", unavailable) },
				Config:      workspaceConfig(f, "prod", ""),
				ExpectError: regexp.MustCompile("Cannot read workspace"),
			},
			{
				PreConfig: func() {
					f.failWith("GetWorkspace", nil)
					f.failWith("UpdateWorkspace", unavailable)
				},
				Config:      workspaceConfig(f, "prod", "changed"),
				ExpectError: regexp.MustCompile("Cannot update workspace"),
			},
			{
				PreConfig: func() {
					f.failWith("UpdateWorkspace", nil)
					f.failWith("DeleteWorkspace", unavailable)
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot delete workspace"),
			},
			{
				PreConfig: func() { f.failWith("DeleteWorkspace", status.Error(codes.NotFound, "gone")) },
				Config:    providerBlock(f),
			},
		},
	})
}

func TestWorkspaceResource_CreateConflict(t *testing.T) {
	f := startFakeServer(t)
	f.workspaces["ws-existing"] = &serverv1.Workspace{Id: "ws-existing", Name: "prod"}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:      workspaceConfig(f, "prod", ""),
			ExpectError: regexp.MustCompile(`(?s)Cannot create workspace.*already exists`),
		}},
	})
}

func TestWorkspaceDataSource(t *testing.T) {
	f := startFakeServer(t)
	f.workspaces["ws-1"] = &serverv1.Workspace{Id: "ws-1", Name: "prod", Slug: "prod", Description: "d", IsDefault: true}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + `
data "relay_workspace" "by_name" {
  name = "prod"
}

data "relay_workspace" "by_id" {
  id = "ws-1"
}
`,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.relay_workspace.by_name", "id", "ws-1"),
				resource.TestCheckResourceAttr("data.relay_workspace.by_name", "description", "d"),
				resource.TestCheckResourceAttr("data.relay_workspace.by_name", "is_default", "true"),
				resource.TestCheckResourceAttr("data.relay_workspace.by_name", "managed_by", ""),
				resource.TestCheckResourceAttr("data.relay_workspace.by_id", "name", "prod"),
			),
		}},
	})
}

func TestWorkspaceDataSource_Errors(t *testing.T) {
	cases := map[string]struct {
		config string
		fail   error
		want   string
	}{
		"not found":   {`data "relay_workspace" "x" { name = "nope" }`, nil, "Workspace not found"},
		"api failure": {`data "relay_workspace" "x" { name = "prod" }`, status.Error(codes.Unavailable, "down"), "Cannot read workspace"},
		"no key":      {`data "relay_workspace" "x" {}`, nil, "Exactly one of these attributes must be configured"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			f.failWith("GetWorkspace", tc.fail)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: providerBlock(f) + tc.config, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}
