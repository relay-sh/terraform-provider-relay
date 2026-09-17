package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func acceptanceFactories(t *testing.T) map[string]func() (tfprotov6.ProviderServer, error) {
	t.Helper()
	for _, env := range []string{"RELAY_ENDPOINT", "RELAY_API_KEY"} {
		if os.Getenv(env) == "" {
			t.Skipf("%s is required for acceptance tests", env)
		}
	}
	return map[string]func() (tfprotov6.ProviderServer, error){
		"relay": providerserver.NewProtocol6WithError(New("acceptance")()),
	}
}

func acceptanceName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tfacc-" + hex.EncodeToString(b)
}

func TestAccWorkspaceAndConnection(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 to run acceptance tests")
	}
	name := acceptanceName(t)
	config := func(description string) string {
		return fmt.Sprintf(`
data "relay_organization" "this" {}

resource "relay_workspace" "this" {
  name        = %[1]q
  description = %[2]q
}

data "relay_workspace" "this" {
  id = relay_workspace.this.id
}

resource "relay_connection" "this" {
  workspace_id   = relay_workspace.this.id
  name           = "pg"
  engine         = "postgres"
  host           = "db.invalid:5432"
  agent_pool     = "default"
  user           = "app"
  credential_ref = "tfacc/pg"
}
`, name, description)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceFactories(t),
		Steps: []resource.TestStep{
			{
				Config: config("created by acceptance tests"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("data.relay_organization.this", "slug"),
					resource.TestCheckResourceAttr("relay_workspace.this", "managed_by", "terraform"),
					resource.TestCheckResourceAttrPair("data.relay_workspace.this", "name", "relay_workspace.this", "name"),
					resource.TestCheckResourceAttr("relay_connection.this", "managed_by", "terraform"),
					resource.TestCheckResourceAttr("relay_connection.this", "password_set", "false"),
				),
			},
			{
				Config: config("updated by acceptance tests"),
				Check:  resource.TestCheckResourceAttr("relay_workspace.this", "description", "updated by acceptance tests"),
			},
			{
				ResourceName:      "relay_workspace.this",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccDatabaseAndUser(t *testing.T) {
	if os.Getenv("TF_ACC") == "" || os.Getenv("RELAY_ACC_DATABASES") == "" {
		t.Skip("set TF_ACC=1 and RELAY_ACC_DATABASES=1 to provision real clusters")
	}
	name := acceptanceName(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceFactories(t),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "relay_workspace" "this" {
  name = %[1]q
}

resource "relay_database" "this" {
  workspace_id = relay_workspace.this.id
  name         = "pg"
  engine       = "postgresql"
  version      = %[2]q
  nodes = [{
    role     = "data"
    count    = 1
    provider = %[3]q
    region   = %[4]q
    tier      = "R5"
    resources = { storage = "5Gi" }
  }]
}

resource "relay_database_user" "this" {
  workspace_id      = relay_workspace.this.id
  database_id       = relay_database.this.id
  username          = "tfacc"
  generate_password = true
  roles             = [{ role = "readOnly", db = "postgres" }]
}

data "relay_database" "this" {
  workspace_id = relay_workspace.this.id
  name         = relay_database.this.name
}
`, name, envOr("RELAY_ACC_PG_VERSION", "17"), envOr("RELAY_ACC_PROVIDER", "aws"), envOr("RELAY_ACC_REGION", "us-east-1")),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("relay_database.this", "phase", "running"),
				resource.TestCheckResourceAttr("relay_database_user.this", "phase", "Ready"),
				resource.TestCheckResourceAttrSet("relay_database_user.this", "generated_password"),
				resource.TestCheckResourceAttrPair("data.relay_database.this", "id", "relay_database.this", "id"),
			),
		}},
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
