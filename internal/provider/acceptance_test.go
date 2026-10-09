package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

var liveNow, livePollInterval = now, pollInterval

func acceptanceFactories(t *testing.T) map[string]func() (tfprotov6.ProviderServer, error) {
	t.Helper()
	for _, env := range []string{"RELAY_ENDPOINT", "RELAY_API_KEY"} {
		if os.Getenv(env) == "" {
			t.Skipf("%s is required for acceptance tests", env)
		}
	}
	fakeClock, fakePoll := now, pollInterval
	now, pollInterval = liveNow, livePollInterval
	t.Cleanup(func() { now, pollInterval = fakeClock, fakePoll })
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

func TestAccAPIKeys(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 to run acceptance tests")
	}
	name := acceptanceName(t)
	config := func(role, cidr, lifetime string) string {
		return fmt.Sprintf(`
resource "relay_workspace" "this" {
  name = %[1]q
}

resource "relay_api_key" "org" {
  name          = %[1]q
  role          = %[2]q
  allowed_cidrs = [%[3]q]
  expires_in    = %[4]q
}

resource "relay_api_key" "workspace" {
  name         = %[1]q
  workspace_id = relay_workspace.this.id
  role         = "viewer"
}

data "relay_api_key" "workspace" {
  id           = relay_api_key.workspace.id
  workspace_id = relay_workspace.this.id
}
`, name, role, cidr, lifetime)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceFactories(t),
		Steps: []resource.TestStep{
			{
				Config: config("viewer", "192.168.1.10", "720h"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("relay_api_key.org", "secret"),
					resource.TestCheckResourceAttrSet("relay_api_key.org", "expires_at"),
					resource.TestCheckTypeSetElemAttr("relay_api_key.org", "effective_allowed_cidrs.*", "192.168.1.10/32"),
					resource.TestCheckResourceAttrPair("data.relay_api_key.workspace", "name", "relay_api_key.workspace", "name"),
				),
			},
			{
				Config: config("admin", "10.1.0.0/16", "48h"),
				Check:  resource.TestCheckResourceAttr("relay_api_key.org", "role", "admin"),
			},
			{
				ResourceName: "relay_api_key.workspace",
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					r := s.RootModule().Resources["relay_api_key.workspace"].Primary
					return r.Attributes["workspace_id"] + "/" + r.ID, nil
				},
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
		},
	})
}

func TestAccPostgreSQLClusterAndUser(t *testing.T) {
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

resource "relay_postgresql_cluster" "this" {
  workspace_id = relay_workspace.this.id
  name         = "pg"
  version      = %[2]q

  nodes = {
    writers = {
      role      = "electable"
      cloud     = %[3]q
      region    = %[4]q
      tier      = "R5"
      resources = { storage = "5Gi" }
    }
  }
}

resource "relay_postgresql_user" "this" {
  workspace_id      = relay_workspace.this.id
  cluster_id        = relay_postgresql_cluster.this.id
  username          = "tfacc"
  generate_password = true
  roles             = [{ role = "readOnly", database = "relay" }]
}

data "relay_postgresql_cluster" "this" {
  workspace_id = relay_workspace.this.id
  name         = relay_postgresql_cluster.this.name
}
`, name, envOr("RELAY_ACC_PG_VERSION", "17"), envOr("RELAY_ACC_CLOUD", "aws"), envOr("RELAY_ACC_REGION", "us-east-1")),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "phase", "running"),
				resource.TestCheckResourceAttr("relay_postgresql_user.this", "phase", "Ready"),
				resource.TestCheckResourceAttrSet("relay_postgresql_user.this", "generated_password"),
				resource.TestCheckResourceAttrPair("data.relay_postgresql_cluster.this", "id", "relay_postgresql_cluster.this", "id"),
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

func TestAccIPAccessList(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 to run acceptance tests")
	}
	name := acceptanceName(t)
	config := func(cidr, description string) string {
		return fmt.Sprintf(`
resource "relay_workspace" "this" {
  name = %[1]q
}

resource "relay_ip_access_list" "this" {
  workspace_id = relay_workspace.this.id
  cidr         = %[2]q
  description  = %[3]q
}

data "relay_ip_access_list" "this" {
  workspace_id = relay_workspace.this.id
  cidr         = relay_ip_access_list.this.cidr
}
`, name, cidr, description)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceFactories(t),
		Steps: []resource.TestStep{
			{
				Config: config("203.0.113.0/24", "created by acceptance tests"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("relay_ip_access_list.this", "id"),
					resource.TestCheckResourceAttrPair("data.relay_ip_access_list.this", "id", "relay_ip_access_list.this", "id"),
				),
			},
			{
				Config: config("198.51.100.7/32", "updated by acceptance tests"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "cidr", "198.51.100.7/32"),
					resource.TestCheckResourceAttr("data.relay_ip_access_list.this", "description", "updated by acceptance tests"),
				),
			},
			{
				ResourceName:      "relay_ip_access_list.this",
				ImportState:       true,
				ImportStateIdFunc: workspaceScopedID("relay_ip_access_list.this", "cidr"),
				ImportStateVerify: true,
			},
		},
	})
}

func workspaceScopedID(address string, attrs ...string) resource.ImportStateIdFunc {
	return func(s *terraform.State) (string, error) {
		r, ok := s.RootModule().Resources[address]
		if !ok {
			return "", fmt.Errorf("%s is not in state", address)
		}
		parts := []string{r.Primary.Attributes["workspace_id"]}
		for _, a := range attrs {
			parts = append(parts, r.Primary.Attributes[a])
		}
		return strings.Join(parts, "/"), nil
	}
}

func skipUnlessPrivateEndpointsServed(t *testing.T, cloud, region string) {
	t.Helper()
	c, err := relay.Dial(relay.Options{Endpoint: os.Getenv("RELAY_ENDPOINT"), Credentials: relay.APIKey(os.Getenv("RELAY_API_KEY"))})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	org, err := c.Server.GetOrganization(ctx, &serverv1.GetOrganizationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Server.ListPrivateEndpointRegions(relay.WithOrg(ctx, org.GetOrganization().GetId()), &serverv1.ListPrivateEndpointRegionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.GetRegions() {
		if r.GetProvider() == cloud && r.GetRegion() == region {
			return
		}
	}
	t.Skipf("the platform serves no private endpoints in %s/%s", cloud, region)
}

func TestAccPrivateEndpointService(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 to run acceptance tests")
	}
	factories := acceptanceFactories(t)
	cloud, region := envOr("RELAY_ACC_CLOUD", "aws"), envOr("RELAY_ACC_REGION", "us-east-1")
	skipUnlessPrivateEndpointsServed(t, cloud, region)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "relay_workspace" "this" {
  name = %[1]q
}

resource "relay_private_endpoint_service" "this" {
  workspace_id = relay_workspace.this.id
  cloud        = %[2]q
  region       = %[3]q
}

data "relay_private_endpoint_service" "this" {
  workspace_id = relay_workspace.this.id
  id           = relay_private_endpoint_service.this.id
}
`, acceptanceName(t), cloud, region),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_private_endpoint_service.this", "status", "AVAILABLE"),
					resource.TestMatchResourceAttr("relay_private_endpoint_service.this", "endpoint_service_name", regexp.MustCompile(`^com\.amazonaws\.vpce\.`)),
					resource.TestCheckResourceAttr("data.relay_private_endpoint_service.this", "endpoints.#", "0"),
				),
			},
			{
				ResourceName:      "relay_private_endpoint_service.this",
				ImportState:       true,
				ImportStateIdFunc: workspaceScopedID("relay_private_endpoint_service.this", "cloud", "region"),
				ImportStateVerify: true,
			},
		},
	})
}
