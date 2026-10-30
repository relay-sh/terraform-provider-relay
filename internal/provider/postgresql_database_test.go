package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
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

func TestPostgreSQLDatabaseResource_SchemasExtensionsAndLockPublic(t *testing.T) {
	f := startFakeServer(t)
	const res = "relay_postgresql_database.this"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noLogicalDatabasesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: logicalDatabaseConfig(f, "balance", `
  schemas     = ["balance", "audit"]
  extensions  = [{ name = "pg_trgm", schema = "balance" }, { name = "pgcrypto" }, { name = "citext", schema = "public" }]
  lock_public = false
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(res, "schemas.#", "2"),
					resource.TestCheckResourceAttr(res, "schemas.1", "audit"),
					resource.TestCheckResourceAttr(res, "extensions.#", "3"),
					resource.TestCheckResourceAttr(res, "extensions.0.schema", "balance"),
					resource.TestCheckNoResourceAttr(res, "extensions.1.schema"),
					resource.TestCheckResourceAttr(res, "extensions.2.schema", "public"),
					resource.TestCheckResourceAttr(res, "lock_public", "false"),
					func(*terraform.State) error {
						c := f.lastLDBCreate
						if len(c.GetSchemas()) != 2 || len(c.GetExtensions()) != 3 || c.GetExtensions()[0].GetSchema() != "balance" || c.GetExtensions()[1].GetSchema() != "" || c.GetLockPublic() {
							return fmt.Errorf("create request = %+v", c)
						}
						return nil
					},
				),
			},
			{
				Config: logicalDatabaseConfig(f, "balance", `
  schemas     = ["balance", "audit"]
  extensions  = [{ name = "pg_trgm", schema = "balance" }]
  lock_public = true
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(res, "extensions.#", "1"),
					resource.TestCheckResourceAttr(res, "lock_public", "true"),
					func(*terraform.State) error {
						u := f.lastLDBUpdate
						if u.Schemas != nil || len(u.GetExtensions().GetItems()) != 1 || !u.GetLockPublic() || u.LockPublic == nil {
							return fmt.Errorf("only changed fields must be sent, got %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: logicalDatabaseConfig(f, "balance", `
  schemas     = ["balance"]
  lock_public = true
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(res, "schemas.#", "1"),
					resource.TestCheckNoResourceAttr(res, "extensions.#"),
					func(*terraform.State) error {
						u := f.lastLDBUpdate
						if len(u.GetSchemas().GetNames()) != 1 || u.Extensions == nil || len(u.GetExtensions().GetItems()) != 0 || u.LockPublic != nil {
							return fmt.Errorf("removing extensions must send an empty wrapper, got %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: logicalDatabaseConfig(f, "balance", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(res, "schemas.#"),
					resource.TestCheckResourceAttr(res, "lock_public", "false"),
					func(*terraform.State) error {
						u := f.lastLDBUpdate
						if u.Schemas == nil || len(u.GetSchemas().GetNames()) != 0 || u.GetLockPublic() {
							return fmt.Errorf("removing schemas must send an empty wrapper, got %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: logicalDatabaseConfig(f, "balance", `
  schemas    = []
  extensions = []
`),
				Check: resource.TestCheckResourceAttr(res, "schemas.#", "0"),
			},
			{
				ResourceName:            res,
				ImportState:             true,
				ImportStateId:           dbWorkspace + "/db-1/balance",
				ImportStateVerify:       false,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
		},
	})
}

func TestPostgreSQLDatabaseResource_LayoutGuards(t *testing.T) {
	f := startFakeServer(t)
	many := `["s1","s2","s3","s4","s5","s6","s7","s8","s9","s10","s11","s12","s13","s14","s15","s16","s17","s18","s19","s20","s21"]`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: logicalDatabaseConfig(f, "b", `  schemas = ["Bad"]`), ExpectError: regexp.MustCompile("Invalid schema name")},
			{Config: logicalDatabaseConfig(f, "b", `  schemas = ["public"]`), ExpectError: regexp.MustCompile("reserved by PostgreSQL")},
			{Config: logicalDatabaseConfig(f, "b", `  schemas = ["pg_extra"]`), ExpectError: regexp.MustCompile("reserved by PostgreSQL")},
			{Config: logicalDatabaseConfig(f, "b", `  schemas = ["`+fmt.Sprintf("%064d", 0)+`"]`), ExpectError: regexp.MustCompile("Invalid schema name")},
			{Config: logicalDatabaseConfig(f, "b", `  schemas = `+many), ExpectError: regexp.MustCompile("(?s)at most 20")},
			{Config: logicalDatabaseConfig(f, "b", `  schemas = ["a", "a"]`), ExpectError: regexp.MustCompile("(?s)duplicate")},
			{Config: logicalDatabaseConfig(f, "b", `  extensions = [{ name = "pg_trgm", schema = "pg_ext" }]`), ExpectError: regexp.MustCompile("Invalid extension schema")},
			{Config: logicalDatabaseConfig(f, "b", "  lock_public = true\n  extensions = [{ name = \"pg_trgm\" }]"), ExpectError: regexp.MustCompile("Extension needs a schema")},
			{Config: logicalDatabaseConfig(f, "b", "  lock_public = true\n  schemas = [\"a\"]\n  extensions = [{ name = \"pg_trgm\", schema = \"public\" }]"), ExpectError: regexp.MustCompile("Extension needs a schema")},
			{Config: logicalDatabaseConfig(f, "b", `  extensions = [{ name = "", schema = "a" }]`), ExpectError: regexp.MustCompile("length must be at least 1")},
		},
	})
}

func TestValidateDatabaseLayoutSkipsUnknowns(t *testing.T) {
	ctx := context.Background()
	extType := types.ObjectType{AttrTypes: databaseExtensionAttrTypes}
	ext := func(name string, schema types.String) attr.Value {
		return types.ObjectValueMust(databaseExtensionAttrTypes, map[string]attr.Value{"name": types.StringValue(name), "schema": schema})
	}
	strs := func(v ...attr.Value) types.List { return types.ListValueMust(types.StringType, v) }
	exts := func(v ...attr.Value) types.List { return types.ListValueMust(extType, v) }
	unknown := types.StringUnknown()

	cases := map[string]struct {
		schemas, extensions types.List
		lock                types.Bool
		errors              int
	}{
		"unknown extensions":        {strs(), types.ListUnknown(extType), types.BoolNull(), 0},
		"unknown schemas":           {types.ListUnknown(types.StringType), exts(ext("a", types.StringValue("x"))), types.BoolNull(), 0},
		"unknown schema element":    {strs(unknown), exts(ext("a", types.StringValue("x"))), types.BoolNull(), 0},
		"unknown extension schema":  {strs(), exts(ext("a", unknown)), types.BoolValue(true), 0},
		"unknown lock":              {strs(), exts(ext("a", types.StringNull())), types.BoolUnknown(), 0},
		"declared schema":           {strs(types.StringValue("x")), exts(ext("a", types.StringValue("x"))), types.BoolValue(true), 0},
		"undeclared schema":         {strs(types.StringValue("y")), exts(ext("a", types.StringValue("x"))), types.BoolValue(true), 0},
		"reserved extension schema": {strs(), exts(ext("a", types.StringValue("pg_x"))), types.BoolNull(), 1},
	}
	for name, tc := range cases {
		if got := validateDatabaseLayout(ctx, tc.schemas, tc.extensions, tc.lock).ErrorsCount(); got != tc.errors {
			t.Errorf("%s: %d errors, want %d", name, got, tc.errors)
		}
	}
}

func TestExtensionsFromProtoKeepsExplicitPublic(t *testing.T) {
	ext := func(name, schema string) attr.Value {
		return types.ObjectValueMust(databaseExtensionAttrTypes, map[string]attr.Value{"name": types.StringValue(name), "schema": types.StringValue(schema)})
	}
	prior := types.ListValueMust(types.ObjectType{AttrTypes: databaseExtensionAttrTypes}, []attr.Value{ext("citext", "public")})
	got := extensionsFromProto([]*serverv1.LogicalDatabaseExtension{
		{Name: "citext", Schema: "public"},
		{Name: "hstore", Schema: "public"},
	}, prior)
	schemas := []attr.Value{
		got.Elements()[0].(types.Object).Attributes()["schema"],
		got.Elements()[1].(types.Object).Attributes()["schema"],
	}
	if !schemas[0].Equal(types.StringValue("public")) || !schemas[1].IsNull() {
		t.Fatalf("schemas = %v", schemas)
	}
}
