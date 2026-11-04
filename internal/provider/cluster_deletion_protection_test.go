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

type protectedCluster struct {
	addr         string
	config       func(f *fakeServer, protected bool) string
	importIgnore []string
}

var protectedClusters = map[string]protectedCluster{
	"postgresql": {
		addr: postgresAddr,
		config: func(f *fakeServer, protected bool) string {
			return postgresRestoreConfig(f, "", fmt.Sprintf("  deletion_protection = %t\n", protected), 1)
		},
		importIgnore: []string{"keep_backups_on_destroy", "timeouts", "nodes"},
	},
	"valkey": {
		addr: valkeyAddr,
		config: func(f *fakeServer, protected bool) string {
			return valkeyConfig(f, fmt.Sprintf("  deletion_protection = %t\n", protected)+valkeyGroup("cache", 1, "8Gi", ""))
		},
		importIgnore: []string{"keep_backups_on_destroy", "timeouts"},
	},
}

func createdProtected(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if !f.lastDBCreate.GetDeletionProtection() {
			return errors.New("deletion_protection must reach CreateDatabase")
		}
		return nil
	}
}

func TestClusterDeletionProtection(t *testing.T) {
	for engine, c := range protectedClusters {
		t.Run(engine, func(t *testing.T) {
			f := startFakeServer(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				CheckDestroy: func(*terraform.State) error {
					f.mu.Lock()
					defer f.mu.Unlock()
					if len(f.databases) != 0 {
						return fmt.Errorf("clusters left after destroy: %v", f.databases)
					}
					return nil
				},
				Steps: []resource.TestStep{
					{
						Config: c.config(f, true),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(c.addr, "deletion_protection", "true"),
							createdProtected(f),
							callCount(f, "SetDeletionProtection", 0),
						),
					},
					{
						Config:      c.config(f, true),
						Destroy:     true,
						ExpectError: regexp.MustCompile(`(?s)Deletion protection is on.*set deletion_protection = false and apply before\s+destroying`),
					},
					{
						PreConfig:   func() { f.set(func() { f.denyDisable = true }) },
						Config:      c.config(f, false),
						ExpectError: regexp.MustCompile(`(?s)Cannot turn deletion protection off.*org.deletion_protection.disable`),
					},
					{
						PreConfig: func() { f.set(func() { f.denyDisable = false }) },
						Config:    c.config(f, false),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(c.addr, "deletion_protection", "false"),
							callCount(f, "DeleteDatabase", 0),
						),
					},
					{
						PreConfig:   func() { f.failWith("SetDeletionProtection", status.Error(codes.FailedPrecondition, "Deletion protection not enabled for this organization")) },
						Config:      c.config(f, true),
						ExpectError: regexp.MustCompile(`(?s)Cannot change deletion protection.*not enabled\s+for this organization`),
					},
					{
						PreConfig: func() { f.failWith("SetDeletionProtection", nil) },
						Config:    c.config(f, true),
						Check:     resource.TestCheckResourceAttr(c.addr, "deletion_protection", "true"),
					},
					{
						ResourceName: c.addr,
						ImportState:  true,
						ImportStateIdFunc: func(s *terraform.State) (string, error) {
							return dbWorkspace + "/" + s.RootModule().Resources[c.addr].Primary.ID, nil
						},
						ImportStateVerify:       true,
						ImportStateVerifyIgnore: c.importIgnore,
					},
					{
						Config: c.config(f, false),
						Check:  resource.TestCheckResourceAttr(c.addr, "deletion_protection", "false"),
					},
				},
			})
		})
	}
}

func TestClusterDeletionProtectionOnRestore(t *testing.T) {
	configs := map[string]func(f *fakeServer) string{
		"postgresql": func(f *fakeServer) string {
			seedSources(f, postgresSource("db-src", "18"))
			return postgresRestoreConfig(f, restoreBlock("db-src", ""), "  deletion_protection = true\n", 1)
		},
		"valkey": func(f *fakeServer) string {
			seedSources(f, valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true))
			return valkeyConfig(f, restoreBlock("db-src", "")+"  deletion_protection = true\n"+valkeyGroup("cache", 1, "8Gi", ""))
		},
	}
	for engine, config := range configs {
		t.Run(engine, func(t *testing.T) {
			addr := protectedClusters[engine].addr
			f := startFakeServer(t)
			restored := config(f)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{
					{
						Config: restored,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
							callCount(f, "SetDeletionProtection", 1),
						),
					},
					{
						Config: regexp.MustCompile(`deletion_protection = true`).ReplaceAllString(restored, "deletion_protection = false"),
						Check:  resource.TestCheckResourceAttr(addr, "deletion_protection", "false"),
					},
				},
			})
		})
	}
}

func TestClusterDeletionProtectionFailsOnRestore(t *testing.T) {
	f := startFakeServer(t)
	seedSources(f, postgresSource("db-src", "18"))
	f.failWith("SetDeletionProtection", status.Error(codes.FailedPrecondition, "Deletion protection not enabled for this organization"))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:      postgresRestoreConfig(f, restoreBlock("db-src", ""), "  deletion_protection = true\n", 1),
			ExpectError: regexp.MustCompile(`Cannot change deletion protection`),
		}},
	})
}

func TestValkeyClusterDeletionProtectionFailsOnRestore(t *testing.T) {
	f := startFakeServer(t)
	seedSources(f, valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true))
	f.failWith("SetDeletionProtection", status.Error(codes.FailedPrecondition, "Deletion protection not enabled for this organization"))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:      valkeyConfig(f, restoreBlock("db-src", "")+"  deletion_protection = true\n"+valkeyGroup("cache", 1, "8Gi", "")),
			ExpectError: regexp.MustCompile(`Cannot change deletion protection`),
		}},
	})
}
