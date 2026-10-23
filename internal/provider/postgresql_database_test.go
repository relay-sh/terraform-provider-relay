package provider

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/relay-sh/relay-go/relay"
)

func logicalDatabaseConfig(f *fakeServer, name, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_database" "this" {
  workspace_id = %q
  cluster_id   = "db-1"
  name         = %q
%s
}
`, dbWorkspace, name, body)
}

func noLogicalDatabasesLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.logicalDBs) != 0 {
			return fmt.Errorf("databases left after destroy: %v", f.logicalDBs)
		}
		return nil
	}
}

func TestPostgreSQLDatabaseResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	f.unlistedFor = 1
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noLogicalDatabasesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: logicalDatabaseConfig(f, "billing", `
  owner       = "app"
  description = "invoices"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_database.this", "id", "db-1/billing"),
					resource.TestCheckResourceAttr("relay_postgresql_database.this", "phase", "Ready"),
					resource.TestCheckResourceAttr("relay_postgresql_database.this", "origin", "Created"),
					resource.TestCheckResourceAttr("relay_postgresql_database.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						c := f.lastLDBCreate
						if c.GetDatabaseId() != "db-1" || c.GetOwner() != "app" || c.GetDescription() != "invoices" {
							return fmt.Errorf("create request = %+v", c)
						}
						if calls := f.callsTo("CreateLogicalDatabase"); calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config: logicalDatabaseConfig(f, "billing", `  description = "ledger"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("relay_postgresql_database.this", "owner"),
					resource.TestCheckResourceAttr("relay_postgresql_database.this", "description", "ledger"),
					resource.TestCheckResourceAttr("relay_postgresql_database.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						u := f.lastLDBUpdate
						switch {
						case u.Owner == nil || u.GetOwner() != "":
							return errors.New("removing owner must send an empty owner")
						case u.GetDescription() != "ledger":
							return errors.New("a changed description must be sent")
						}
						return nil
					},
				),
			},
			{
				Config: logicalDatabaseConfig(f, "billing", `  owner = "app"`),
				Check: func(*terraform.State) error {
					u := f.lastLDBUpdate
					if u.GetOwner() != "app" || u.Description == nil || u.GetDescription() != "" {
						return fmt.Errorf("update request = %+v", u)
					}
					return nil
				},
			},
			{
				ResourceName:            "relay_postgresql_database.this",
				ImportState:             true,
				ImportStateId:           dbWorkspace + "/db-1/billing",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
		},
	})
}

func TestPostgreSQLDatabaseResource_CreatedBeforeTheUserThatUsesIt(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			noLogicalDatabasesLeft(f),
			noUsersLeft(f),
		),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_database" "billing" {
  workspace_id = %[1]q
  cluster_id   = "db-1"
  name         = "billing"
}

resource "relay_postgresql_user" "app" {
  workspace_id      = %[1]q
  cluster_id        = relay_postgresql_database.billing.cluster_id
  username          = "app"
  generate_password = true
  roles             = [{ role = "readWrite", database = relay_postgresql_database.billing.name }]
}
`, dbWorkspace),
			Check: func(*terraform.State) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				db := slices.IndexFunc(f.calls, func(c call) bool { return c.method == "CreateLogicalDatabase" })
				user := slices.IndexFunc(f.calls, func(c call) bool { return c.method == "CreateDatabaseUser" })
				if db < 0 || user < db {
					return fmt.Errorf("the database must be created before the user, got calls %v", f.calls)
				}
				return nil
			},
		}},
	})
}

func TestPostgreSQLDatabaseResource_ConfigGuards(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:      logicalDatabaseConfig(f, "billing", `  owner = ""`),
				ExpectError: regexp.MustCompile("length must be at least 1"),
			},
			{
				Config:      logicalDatabaseConfig(f, "", ""),
				ExpectError: regexp.MustCompile("length must be at least 1"),
			},
		},
	})
}

func TestPostgreSQLDatabaseResource_Failures(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	t.Run("create outcomes", func(t *testing.T) {
		cases := map[string]struct {
			prep  func(f *fakeServer)
			name  string
			extra string
			want  string
		}{
			"rejected":     {func(f *fakeServer) { f.failWith("CreateLogicalDatabase", unavailable) }, "billing", "", "Cannot create PostgreSQL database"},
			"reserved":     {func(*fakeServer) {}, "postgres", "", `(?s)Cannot create PostgreSQL database.*reserved`},
			"failed phase": {func(f *fakeServer) { f.failLogicalDB = "owner missing" }, "billing", "", `(?s)did not become ready.*owner missing`},
			"poll error": {func(f *fakeServer) { f.failWith("ListLogicalDatabases", unavailable) }, "billing", `
  timeouts {
    create = "30ms"
  }
`, `(?s)did not become ready.*timed out.*down`},
			"poll rejected": {func(f *fakeServer) {
				f.failWith("ListLogicalDatabases", status.Error(codes.PermissionDenied, "denied"))
			}, "billing", "", `(?s)did not become ready.*denied`},
			"timeout": {func(f *fakeServer) { f.stuck = true }, "billing", `
  timeouts {
    create = "30ms"
  }
`, `(?s)did not become ready.*timed out`},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				f := startFakeServer(t)
				tc.prep(f)
				resource.UnitTest(t, resource.TestCase{
					ProtoV6ProviderFactories: factories(relay.Dial),
					CheckDestroy:             noLogicalDatabasesLeft(f),
					Steps: []resource.TestStep{
						{Config: logicalDatabaseConfig(f, tc.name, tc.extra), ExpectError: regexp.MustCompile(tc.want)},
						{
							PreConfig: func() {
								f.mu.Lock()
								defer f.mu.Unlock()
								f.errs, f.stuck, f.failLogicalDB = map[string]error{}, false, ""
							},
							Config: providerBlock(f),
						},
					},
				})
			})
		}
	})

	t.Run("read update delete", func(t *testing.T) {
		f := startFakeServer(t)
		cfg := logicalDatabaseConfig(f, "billing", `  description = "a"`)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: factories(relay.Dial),
			Steps: []resource.TestStep{
				{Config: cfg},
				{
					PreConfig: func() {
						f.mu.Lock()
						defer f.mu.Unlock()
						for k := range f.logicalDBs {
							delete(f.logicalDBs, k)
						}
					},
					Config:             cfg,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
				{Config: cfg},
				{
					PreConfig:          func() { f.failWith("ListLogicalDatabases", status.Error(codes.NotFound, "cluster gone")) },
					Config:             cfg,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
				{
					PreConfig:   func() { f.failWith("ListLogicalDatabases", unavailable) },
					Config:      cfg,
					ExpectError: regexp.MustCompile("Cannot read PostgreSQL database"),
				},
				{
					PreConfig: func() {
						f.failWith("ListLogicalDatabases", nil)
						f.failWith("UpdateLogicalDatabase", unavailable)
					},
					Config:      logicalDatabaseConfig(f, "billing", `  description = "b"`),
					ExpectError: regexp.MustCompile("Cannot update PostgreSQL database"),
				},
				{
					PreConfig:   func() { f.failWith("DeleteLogicalDatabase", unavailable) },
					Config:      providerBlock(f),
					ExpectError: regexp.MustCompile("Cannot delete PostgreSQL database"),
				},
				{
					PreConfig: func() { f.failWith("DeleteLogicalDatabase", status.Error(codes.NotFound, "gone")) },
					Config:    providerBlock(f),
				},
				{
					Config:        logicalDatabaseConfig(f, "billing", ""),
					ResourceName:  "relay_postgresql_database.this",
					ImportState:   true,
					ImportStateId: "ws/db-only",
					ExpectError:   regexp.MustCompile("Invalid import ID"),
				},
			},
		})
	})
}

func TestPostgreSQLDatabaseResource_DestroyAfterAFailedCreate(t *testing.T) {
	f := startFakeServer(t)
	f.failLogicalDB = "owner missing"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noLogicalDatabasesLeft(f),
		Steps: []resource.TestStep{{
			Config:      logicalDatabaseConfig(f, "billing", ""),
			ExpectError: regexp.MustCompile(`(?s)did not become ready.*owner missing`),
		}},
	})
}
