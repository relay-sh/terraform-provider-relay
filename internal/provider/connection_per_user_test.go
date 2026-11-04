package provider

import (
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/relay-sh/relay-go/relay"
)

const connAddr = "relay_connection.this"

func perUserConfig(f *fakeServer, name, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_connection" "this" {
  workspace_id        = %q
  name                = %q
  engine              = "postgres"
  agent_pool          = "default"
  managed_database_id = "db-1"
  user                = "usr_account_rw"
  database            = "account"
%s
}
`, connWorkspace, name, body)
}

func TestConnectionResource_PerUserLifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noConnectionsLeft(f),
		Steps: []resource.TestStep{
			{
				Config: perUserConfig(f, "account-rw", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(connAddr, "database_user", "usr_account_rw"),
					resource.TestCheckNoResourceAttr(connAddr, "host"),
					resource.TestCheckResourceAttr(connAddr, "credential_ref", ""),
					resource.TestCheckResourceAttr(connAddr, "host_tls_ca", ""),
					resource.TestCheckResourceAttr(connAddr, "tls_cert", ""),
					resource.TestCheckResourceAttr(connAddr, "managed_by", "terraform"),
					func(*terraform.State) error {
						c := f.lastCreate.GetConnection()
						if c.GetHost() != "" || c.GetCredentialRef() != "" || c.GetCredentialSource() != "" || c.GetPassword() != "" ||
							c.GetTls() != nil || c.GetHostTls() != nil || c.GetDatabaseUser() != "" || c.GetInternalHost() != "" {
							return fmt.Errorf("a per-user create must carry only user-level fields: %+v", c)
						}
						return nil
					},
				),
			},
			{
				Config: perUserConfig(f, "account-rw", `
  redact_enabled   = true
  audit_enabled    = false
  policy_fail_mode = "open"
  managed_by       = "pulumi"
  redact_rules     = [{ column_pattern = "email", strategy = "hash" }]
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(connAddr, "database_user", "usr_account_rw"),
					resource.TestCheckResourceAttr(connAddr, "redact_rules.#", "1"),
					func(*terraform.State) error {
						u := f.lastUpdate
						if u.Engine != nil || u.Host != nil || u.User != nil || u.CredentialRef != nil || u.CredentialSource != nil ||
							u.Database != nil || u.AgentPool != nil || u.Password != nil || u.Tls != nil || u.HostTls != nil ||
							u.ManagedDatabaseId != nil || u.GetClearManagedDatabaseId() || u.DatabaseUser != nil {
							return fmt.Errorf("a per-user update must only touch tenant settings: %+v", u)
						}
						if !u.GetRedactEnabled() || u.GetAuditEnabled() || u.GetPolicyFailMode() != "open" || u.GetManagedBy() != "pulumi" || len(u.GetRedactRules()) != 1 {
							return fmt.Errorf("tenant settings missing from update: %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: perUserConfig(f, "account-rw", `
  deletion_protection = true
`),
				Check: func(*terraform.State) error {
					if u := f.lastUpdate; !u.GetDeletionProtection() || !u.GetClearRedactRules() {
						return errors.New("deletion protection and cleared rules must be sent")
					}
					return nil
				},
			},
			{
				ResourceName:      connAddr,
				ImportState:       true,
				ImportStateId:     connWorkspace + "/account-rw",
				ImportStateVerify: true,
			},
			{
				Config: perUserConfig(f, "account-rw", `  deletion_protection = false`),
			},
		},
	})
}

func TestConnectionResource_PerUserReplacements(t *testing.T) {
	f := startFakeServer(t)
	base := func(managed, user, database, pool string) string {
		return providerBlock(f) + fmt.Sprintf(`
resource "relay_connection" "this" {
  workspace_id        = %q
  name                = "account"
  engine              = "postgres"
  agent_pool          = %q
  managed_database_id = %q
  user                = %q
  database            = %q
}
`, connWorkspace, pool, managed, user, database)
	}
	replace := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(connAddr, plancheck.ResourceActionDestroyBeforeCreate)}}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noConnectionsLeft(f),
		Steps: []resource.TestStep{
			{Config: base("db-1", "usr_account_rw", "account", "default")},
			{Config: base("db-2", "usr_account_rw", "account", "default"), ConfigPlanChecks: replace},
			{Config: base("db-2", "usr_account_ro", "account", "default"), ConfigPlanChecks: replace,
				Check: resource.TestCheckResourceAttr(connAddr, "database_user", "usr_account_ro")},
			{Config: base("db-2", "usr_account_ro", "billing", "default"), ConfigPlanChecks: replace},
			{Config: base("db-2", "usr_account_ro", "billing", "edge"), ConfigPlanChecks: replace},
			{
				Config: connectionConfig(f, "account", `  managed_database_id = "db-2"
  user = "usr_account_ro"
  database = "billing"`),
				ConfigPlanChecks: replace,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(connAddr, "database_user", ""),
					resource.TestCheckResourceAttr(connAddr, "host", "db.internal:5432"),
				),
			},
			{Config: base("db-2", "usr_account_ro", "billing", "default"), ConfigPlanChecks: replace},
		},
	})
}

func TestConnectionResource_PerUserValidation(t *testing.T) {
	f := startFakeServer(t)
	noHost := func(body string) string {
		return providerBlock(f) + fmt.Sprintf(`
resource "relay_connection" "this" {
  workspace_id = %q
  name         = "account"
  engine       = "postgres"
  agent_pool   = "default"
%s
}
`, connWorkspace, body)
	}
	cases := map[string]struct {
		config string
		expect string
	}{
		"no managed database": {noHost(`  user = "u"
  database = "d"`), `host is required unless both managed_database_id and user are set`},
		"no user": {noHost(`  managed_database_id = "db-1"
  database = "d"`), `host is required unless both managed_database_id and user are set`},
		"empty user": {noHost(`  managed_database_id = "db-1"
  user = ""
  database = "d"`), `host is required unless both managed_database_id and user are set`},
		"empty host":        {noHost(`  host = ""`), `string length must be at least 1`},
		"credential_ref":    {perUserConfig(f, "a", `  credential_ref = "x"`), `credential_ref cannot be set when host is omitted`},
		"credential_source": {perUserConfig(f, "a", `  credential_source = "x"`), `credential_source cannot be set when host is omitted`},
		"password":          {perUserConfig(f, "a", `  password = "x"`), `password cannot be set when host is omitted`},
		"host_tls_ca":       {perUserConfig(f, "a", `  host_tls_ca = "x"`), `host_tls_ca cannot be set when host is omitted`},
		"tls_cert":          {perUserConfig(f, "a", `  tls_cert = "x"`), `tls_cert cannot be set when host is omitted`},
		"tls_key":           {perUserConfig(f, "a", `  tls_key = "x"`), `tls_key cannot be set when host is omitted`},
		"tls_ca":            {perUserConfig(f, "a", `  tls_ca = "x"`), `tls_ca cannot be set when host is omitted`},
		"database_user":     {perUserConfig(f, "a", `  database_user = "x"`), `database_user`},
		"engine": {perUserConfig(f, "a", ``) + `
resource "relay_connection" "other" {
  workspace_id        = "ws"
  name                = "other"
  engine              = "mysql"
  agent_pool          = "default"
  managed_database_id = "db-1"
  user                = "u"
  database            = "d"
}
`, `engine must be "postgres"`},
		"database": {noHost(`  managed_database_id = "db-1"
  user = "u"`), `database is required when host is omitted`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      tc.config,
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(`(?s)` + tc.expect),
				}},
			})
		})
	}
}

func TestConnectionResource_PerUserUnknownValuesDefer(t *testing.T) {
	f := startFakeServer(t)
	config := providerBlock(f) + fmt.Sprintf(`
resource "terraform_data" "ref" {
  input = "x"
}

resource "relay_connection" "unknown_host" {
  workspace_id = %[1]q
  name         = "unknown-host"
  engine       = "postgres"
  agent_pool   = "default"
  host         = terraform_data.ref.output
}

resource "relay_connection" "unknown_secrets" {
  workspace_id        = %[1]q
  name                = "unknown-secrets"
  engine              = terraform_data.ref.output
  agent_pool          = "default"
  managed_database_id = terraform_data.ref.output
  user                = terraform_data.ref.output
  database            = terraform_data.ref.output
  credential_ref      = terraform_data.ref.output
}
`, connWorkspace)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:             config,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		}},
	})
}

func TestConnectionResource_PerUserRejectedByServer(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:      perUserConfig(f, "account-rw", ""),
			PreConfig:   func() { f.failWith("CreateConnection", errors.New("boom")) },
			ExpectError: regexp.MustCompile("Cannot create connection"),
		}},
	})
}
