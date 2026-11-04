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

func TestConnectionDeletionProtection(t *testing.T) {
	const addr = "relay_connection.this"
	f := startFakeServer(t)
	config := func(protected bool) string {
		return connectionConfig(f, "pg", fmt.Sprintf("  deletion_protection = %t\n", protected))
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noConnectionsLeft(f),
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
					func(*terraform.State) error {
						if !f.lastCreate.GetConnection().GetDeletionProtection() {
							return errors.New("deletion_protection must reach CreateConnection")
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
					callCount(f, "DeleteConnection", 0),
				),
			},
			{
				PreConfig:   func() { f.failWith("UpdateConnection", status.Error(codes.FailedPrecondition, "Deletion protection not enabled for this organization")) },
				Config:      config(true),
				ExpectError: regexp.MustCompile(`(?s)Cannot update connection.*not enabled\s+for this organization`),
			},
			{
				PreConfig: func() { f.failWith("UpdateConnection", nil) },
				Config:    config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
					func(*terraform.State) error {
						if !f.lastUpdate.GetDeletionProtection() {
							return errors.New("deletion_protection must reach UpdateConnection")
						}
						return nil
					},
				),
			},
			{
				Config: connectionConfig(f, "pg", "  deletion_protection = true\n  database = \"app\"\n"),
				Check: func(*terraform.State) error {
					if f.lastUpdate.DeletionProtection != nil {
						return errors.New("an unchanged deletion_protection must not be sent")
					}
					return nil
				},
			},
			{
				ResourceName:            addr,
				ImportState:             true,
				ImportStateId:           connWorkspace + "/pg",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password", "host"},
			},
			{
				Config: config(false),
				Check:  resource.TestCheckResourceAttr(addr, "deletion_protection", "false"),
			},
		},
	})
}
