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

const connWorkspace = "ws-conn"

func connectionConfig(f *fakeServer, name, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_connection" "this" {
  workspace_id = %q
  name         = %q
  engine       = "postgres"
  host         = "db.internal:5432"
  agent_pool   = "default"
%s
}
`, connWorkspace, name, body)
}

func TestConnectionResource_ManagedByIsConfigurable(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: connectionConfig(f, "pg", `  managed_by = "pulumi"`),
				Check:  resource.TestCheckResourceAttr("relay_connection.this", "managed_by", "pulumi"),
			},
			{
				Config: connectionConfig(f, "pg", `  managed_by = "crossplane"`),
				Check:  resource.TestCheckResourceAttr("relay_connection.this", "managed_by", "crossplane"),
			},
		},
	})
}

const fullConnectionBody = `
  user                = "app"
  password            = "first"
  credential_source   = "vault"
  database            = "app"
  listen              = "127.0.0.1:5433"
  kind                = "service"
  managed_database_id = "db-1"
  redact_enabled      = true
  audit_enabled       = false
  policy_fail_mode    = "open"
  tls_cert            = "CERT"
  tls_key             = "KEY"
  tls_ca              = "CA"
  host_tls_ca         = "HOSTCA"

  redact_rules = [
    { column_pattern = "email", strategy = "hash" },
    { column_pattern = "card", strategy = "partial", partial_show = 4 },
  ]
`

const trimmedConnectionBody = `
  user     = "app"
  password = "second"
  database = "app"
  kind     = "service"
`

func noConnectionsLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.connections) != 0 {
			return fmt.Errorf("connections left after destroy: %v", f.connections)
		}
		return nil
	}
}

func TestConnectionResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noConnectionsLeft(f),
		Steps: []resource.TestStep{
			{
				Config: connectionConfig(f, "pg", fullConnectionBody),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("relay_connection.this", "id"),
					resource.TestCheckResourceAttr("relay_connection.this", "kind", "service"),
					resource.TestCheckResourceAttr("relay_connection.this", "credential_source", "vault"),
					resource.TestCheckResourceAttr("relay_connection.this", "managed_database_id", "db-1"),
					resource.TestCheckResourceAttr("relay_connection.this", "redact_rules.#", "2"),
					resource.TestCheckResourceAttr("relay_connection.this", "redact_rules.0.partial_show", "0"),
					resource.TestCheckResourceAttr("relay_connection.this", "redact_rules.1.partial_show", "4"),
					resource.TestCheckResourceAttr("relay_connection.this", "audit_enabled", "false"),
					resource.TestCheckResourceAttr("relay_connection.this", "tls_key", "KEY"),
					resource.TestCheckResourceAttr("relay_connection.this", "host_tls_ca", "HOSTCA"),
					resource.TestCheckResourceAttr("relay_connection.this", "password_set", "true"),
					resource.TestCheckResourceAttr("relay_connection.this", "status", "unknown"),
					resource.TestCheckResourceAttr("relay_connection.this", "managed_by", "terraform"),
					func(*terraform.State) error {
						calls := f.callsTo("CreateConnection")
						if len(calls) != 1 || calls[0].workspace != connWorkspace || calls[0].org != testOrgID {
							return fmt.Errorf("create must be scoped to org and workspace: %+v", calls)
						}
						if !f.lastCreate.GetConnection().GetHostTls().GetEnabled() {
							return errors.New("upstream TLS must always be enabled")
						}
						return nil
					},
				),
			},
			{
				Config: connectionConfig(f, "pg", trimmedConnectionBody),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("relay_connection.this", "redact_rules.#"),
					resource.TestCheckNoResourceAttr("relay_connection.this", "managed_database_id"),
					resource.TestCheckResourceAttr("relay_connection.this", "tls_cert", ""),
					resource.TestCheckResourceAttr("relay_connection.this", "credential_source", ""),
					func(*terraform.State) error {
						u := f.lastUpdate
						switch {
						case !u.GetClearRedactRules():
							return errors.New("removing redact_rules must clear them")
						case !u.GetClearManagedDatabaseId():
							return errors.New("removing managed_database_id must clear it")
						case u.Password == nil || u.GetPassword() != "second":
							return errors.New("a changed password must be sent")
						case !u.GetHostTls().GetEnabled():
							return errors.New("upstream TLS must stay enabled on update")
						}
						return nil
					},
				),
			},
			{
				Config: connectionConfig(f, "pg", trimmedConnectionBody+`  managed_database_id = "db-2"
`),
				Check: func(*terraform.State) error {
					u := f.lastUpdate
					if u.Password != nil {
						return errors.New("an unchanged password must not be resent")
					}
					if u.GetClearManagedDatabaseId() || u.GetManagedDatabaseId() != "db-2" {
						return fmt.Errorf("managed database update = %+v", u)
					}
					return nil
				},
			},
			{
				ResourceName:            "relay_connection.this",
				ImportState:             true,
				ImportStateId:           connWorkspace + "/pg",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password"},
			},
			{
				Config: connectionConfig(f, "pg-renamed", trimmedConnectionBody),
				Check: func(*terraform.State) error {
					if n := len(f.callsTo("CreateConnection")); n != 2 {
						return fmt.Errorf("a rename must replace the connection, got %d creates", n)
					}
					return nil
				},
			},
		},
	})
}

func TestConnectionResource_DriftAndFailures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: connectionConfig(f, "pg", "")},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for k := range f.connections {
						delete(f.connections, k)
					}
				},
				Config:             connectionConfig(f, "pg", ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: connectionConfig(f, "pg", "")},
			{
				PreConfig:   func() { f.failWith("GetConnections", unavailable) },
				Config:      connectionConfig(f, "pg", ""),
				ExpectError: regexp.MustCompile("Cannot read connection"),
			},
			{
				PreConfig: func() {
					f.failWith("GetConnections", nil)
					f.failWith("UpdateConnection", unavailable)
				},
				Config:      connectionConfig(f, "pg", `  database = "other"`),
				ExpectError: regexp.MustCompile("Cannot update connection"),
			},
			{
				PreConfig: func() {
					f.failWith("UpdateConnection", nil)
					f.failWith("DeleteConnection", unavailable)
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot delete connection"),
			},
			{
				PreConfig: func() { f.failWith("DeleteConnection", status.Error(codes.NotFound, "gone")) },
				Config:    providerBlock(f),
			},
		},
	})
}

func TestConnectionResource_CreateAndImportErrors(t *testing.T) {
	f := startFakeServer(t)
	f.failWith("CreateConnection", status.Error(codes.InvalidArgument, `pool "default" does not exist`))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:      connectionConfig(f, "pg", ""),
				ExpectError: regexp.MustCompile(`(?s)Cannot create connection.*does not exist`),
			},
			{
				PreConfig:     func() { f.failWith("CreateConnection", nil) },
				Config:        connectionConfig(f, "pg", ""),
				ResourceName:  "relay_connection.this",
				ImportState:   true,
				ImportStateId: "no-slash",
				ExpectError:   regexp.MustCompile("Invalid import ID"),
			},
		},
	})
}

func TestConnectionResource_EmptyRedactRulesStayEmpty(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: connectionConfig(f, "pg", `  redact_rules = []`),
			Check:  resource.TestCheckResourceAttr("relay_connection.this", "redact_rules.#", "0"),
		}},
	})
}
