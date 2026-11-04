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

func TestPostgreSQLDatabaseDeletionProtection(t *testing.T) {
	const addr = "relay_postgresql_database.this"
	f := startFakeServer(t)
	config := func(protected bool) string {
		return logicalDatabaseConfig(f, "billing", fmt.Sprintf("  deletion_protection = %t\n", protected))
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noLogicalDatabasesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
					func(*terraform.State) error {
						if !f.lastLDBCreate.GetDeletionProtection() {
							return errors.New("deletion_protection must reach CreateLogicalDatabase")
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
					callCount(f, "DeleteLogicalDatabase", 0),
				),
			},
			{
				PreConfig:   func() { f.failWith("UpdateLogicalDatabase", status.Error(codes.FailedPrecondition, "Deletion protection not enabled for this organization")) },
				Config:      config(true),
				ExpectError: regexp.MustCompile(`(?s)Cannot update PostgreSQL database.*not enabled\s+for this organization`),
			},
			{
				PreConfig: func() { f.failWith("UpdateLogicalDatabase", nil) },
				Config:    config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
					func(*terraform.State) error {
						if !f.lastLDBUpdate.GetDeletionProtection() || f.lastLDBUpdate.Owner != nil {
							return fmt.Errorf("only deletion_protection changed, got %v", f.lastLDBUpdate)
						}
						return nil
					},
				),
			},
			{
				ResourceName:            addr,
				ImportState:             true,
				ImportStateId:           dbWorkspace + "/db-1/billing",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{
				Config: config(false),
				Check:  resource.TestCheckResourceAttr(addr, "deletion_protection", "false"),
			},
		},
	})
}
