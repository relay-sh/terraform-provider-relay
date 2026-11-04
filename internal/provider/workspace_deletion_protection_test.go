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

	"github.com/relay-sh/relay-go/relay"
)

func TestWorkspaceDeletionProtection(t *testing.T) {
	const addr = "relay_workspace.this"
	f := startFakeServer(t)
	config := func(protected bool) string {
		return providerBlock(f) + fmt.Sprintf(`
resource "relay_workspace" "this" {
  name                = "prod"
  deletion_protection = %t
}
`, protected)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noWorkspacesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
					func(*terraform.State) error {
						f.mu.Lock()
						defer f.mu.Unlock()
						if w := f.workspaceByName("prod"); w == nil || !w.GetDeletionProtection() {
							return errors.New("deletion_protection must reach CreateWorkspace")
						}
						return nil
					},
				),
			},
			{
				Config:      config(true),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`(?s)Deletion protection is on.*set deletion_protection = false and apply before\s+destroying`),
			},
			{
				PreConfig:   func() { f.set(func() { f.denyDisable = true }) },
				Config:      config(false),
				ExpectError: regexp.MustCompile(`(?s)Cannot turn deletion protection off.*org.deletion_protection.disable`),
			},
			{
				PreConfig: func() { f.set(func() { f.denyDisable = false }) },
				Config:    config(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "deletion_protection", "false"),
					callCount(f, "DeleteWorkspace", 0),
				),
			},
			{
				PreConfig:   func() { f.failWith("UpdateWorkspace", status.Error(codes.FailedPrecondition, "Deletion protection not enabled for this organization")) },
				Config:      config(true),
				ExpectError: regexp.MustCompile(`(?s)Cannot update workspace.*not enabled\s+for this organization`),
			},
			{
				PreConfig: func() { f.failWith("UpdateWorkspace", nil) },
				Config:    config(true),
				Check:     resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
			},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: config(false),
				Check:  resource.TestCheckResourceAttr(addr, "deletion_protection", "false"),
			},
		},
	})
}
