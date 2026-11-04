package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/relay-sh/relay-go/relay"
)

const connectionAddr = "relay_connection.this"

func perUserConnectionConfig(f *fakeServer, engine, managedDatabaseID, user, database, agentPool string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_connection" "this" {
  workspace_id        = %q
  name                = "acct"
  engine              = %q
  agent_pool          = %q
  managed_database_id = %q
  user                = %q
  database            = %q
}
`, connWorkspace, engine, agentPool, managedDatabaseID, user, database)
}

func expectConnectionAction(action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(connectionAddr, action)}}
}

func TestConnectionResource_DatabaseUserConnectionReplacesOnPlatformInputs(t *testing.T) {
	f := startFakeServer(t)
	replaced := plancheck.ResourceActionDestroyBeforeCreate
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noConnectionsLeft(f),
		Steps: []resource.TestStep{
			{Config: perUserConnectionConfig(f, "postgres", "db-1", "usr_a", "account", "default")},
			{Config: perUserConnectionConfig(f, "postgres", "db-1", "usr_a", "account", "pool-2"), ConfigPlanChecks: expectConnectionAction(plancheck.ResourceActionUpdate)},
			{Config: perUserConnectionConfig(f, "postgres", "db-1", "usr_b", "account", "pool-2"), ConfigPlanChecks: expectConnectionAction(replaced)},
			{Config: perUserConnectionConfig(f, "postgres", "db-1", "usr_b", "billing", "pool-2"), ConfigPlanChecks: expectConnectionAction(replaced)},
			{Config: perUserConnectionConfig(f, "postgres", "db-2", "usr_b", "billing", "pool-2"), ConfigPlanChecks: expectConnectionAction(replaced)},
			{
				Config:           perUserConnectionConfig(f, "mysql", "db-2", "usr_b", "billing", "pool-2"),
				ConfigPlanChecks: expectConnectionAction(replaced),
				Check: func(*terraform.State) error {
					if n := len(f.callsTo("CreateConnection")); n != 5 {
						return fmt.Errorf("want 5 creates, got %d", n)
					}
					if n := len(f.callsTo("UpdateConnection")); n != 1 {
						return fmt.Errorf("want 1 update, got %d", n)
					}
					return nil
				},
			},
		},
	})
}

func TestConnectionResource_SwitchingModesReplaces(t *testing.T) {
	f := startFakeServer(t)
	hostBased := connectionConfig(f, "acct", "")
	perUser := perUserConnectionConfig(f, "postgres", "db-1", "usr_a", "account", "default")
	replaced := plancheck.ResourceActionDestroyBeforeCreate
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noConnectionsLeft(f),
		Steps: []resource.TestStep{
			{Config: hostBased},
			{Config: perUser, ConfigPlanChecks: expectConnectionAction(replaced), Check: resource.TestCheckResourceAttr(connectionAddr, "database_user", "usr_a")},
			{Config: hostBased, ConfigPlanChecks: expectConnectionAction(replaced), Check: resource.TestCheckResourceAttr(connectionAddr, "database_user", "")},
		},
	})
}

func TestConnectionResource_DatabaseUserConnectionImport(t *testing.T) {
	f := startFakeServer(t)
	config := perUserConnectionConfig(f, "postgres", "db-1", "usr_a", "account", "default")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noConnectionsLeft(f),
		Steps: []resource.TestStep{
			{Config: config},
			{
				ResourceName:      connectionAddr,
				ImportState:       true,
				ImportStateId:     connWorkspace + "/acct",
				ImportStateVerify: true,
			},
			{
				Config: providerBlock(f) + `
removed {
  from = relay_connection.this
  lifecycle {
    destroy = false
  }
}
`,
			},
			{
				Config:             config,
				ResourceName:       connectionAddr,
				ImportState:        true,
				ImportStateId:      connWorkspace + "/acct",
				ImportStatePersist: true,
			},
			{Config: config, PlanOnly: true},
		},
	})
}
