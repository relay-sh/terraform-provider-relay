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

func userConfig(f *fakeServer, username, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_database_user" "this" {
  workspace_id = %q
  database_id  = "db-1"
  username     = %q
  roles        = [{ role = "readWrite", db = "app" }]
%s
}
`, dbWorkspace, username, body)
}

func TestDatabaseUserResource_WithoutCreatedAt(t *testing.T) {
	f := startFakeServer(t)
	f.userWithoutTimes = true
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{{
			Config: userConfig(f, "app", `  password = "first"`),
			Check:  resource.TestCheckResourceAttr("relay_database_user.this", "created_at", ""),
		}},
	})
}

func noUsersLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.users) != 0 {
			return fmt.Errorf("users left after destroy: %v", f.users)
		}
		return nil
	}
}

func TestDatabaseUserResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: userConfig(f, "app", `
  password             = "first"
  description          = "application"
  restricted_resources = ["public.orders"]
  expires_at           = "2027-06-01T00:00:00-03:00"
  connection_limit     = 20
  statement_timeout    = "30s"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_database_user.this", "id", "db-1/app"),
					resource.TestCheckResourceAttr("relay_database_user.this", "phase", "Ready"),
					resource.TestCheckResourceAttr("relay_database_user.this", "expires_at", "2027-06-01T00:00:00-03:00"),
					resource.TestCheckResourceAttr("relay_database_user.this", "roles.0.collection", ""),
					resource.TestCheckResourceAttr("relay_database_user.this", "generated_password", ""),
					resource.TestCheckResourceAttr("relay_database_user.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						c := f.lastUserCreate
						if c.GetPassword() != "first" || c.GetAuthMethod() != "password" || c.GetConnectionLimit() != 20 {
							return fmt.Errorf("create request = %+v", c)
						}
						if calls := f.callsTo("CreateDatabaseUser"); calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config: userConfig(f, "app", `
  password             = "second"
  description          = "application"
  restricted_resources = ["public.orders"]
  connection_limit     = 40
  statement_timeout    = "30s"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("relay_database_user.this", "expires_at"),
					resource.TestCheckResourceAttr("relay_database_user.this", "connection_limit", "40"),
					resource.TestCheckResourceAttr("relay_database_user.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						u := f.lastUserUpdate
						switch {
						case u.GetPassword() != "second":
							return errors.New("a changed password must be sent")
						case u.GetExpiresAt() == nil || !u.GetExpiresAt().AsTime().IsZero():
							return errors.New("removing expires_at must send a zero expiry")
						case len(u.GetRoles()) != 0:
							return errors.New("roles are immutable and must never be sent on update")
						}
						return nil
					},
				),
			},
			{
				Config: userConfig(f, "app", `
  password             = "second"
  description          = "application"
  restricted_resources = ["public.orders"]
  connection_limit     = 40
  statement_timeout    = "1m"
  expires_at           = "2028-01-01T00:00:00Z"
`),
				Check: func(*terraform.State) error {
					u := f.lastUserUpdate
					if u.Password != nil || u.GetStatementTimeout() != "1m" || u.GetExpiresAt().AsTime().Year() != 2028 {
						return fmt.Errorf("update request = %+v", u)
					}
					return nil
				},
			},
			{
				ResourceName:            "relay_database_user.this",
				ImportState:             true,
				ImportStateId:           dbWorkspace + "/db-1/app",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password", "generated_password", "timeouts"},
			},
		},
	})
}

func TestDatabaseUserResource_GeneratedPasswordAndReplace(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: userConfig(f, "reporting", `  generate_password = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_database_user.this", "generated_password", "generated-secret"),
					resource.TestCheckNoResourceAttr("relay_database_user.this", "restricted_resources"),
				),
			},
			{
				Config: providerBlock(f) + fmt.Sprintf(`
resource "relay_database_user" "this" {
  workspace_id      = %q
  database_id       = "db-1"
  username          = "reporting"
  generate_password = true
  roles             = [{ role = "readOnly", db = "app" }]
  restricted_resources = []
}
`, dbWorkspace),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_database_user.this", "roles.0.role", "readOnly"),
					resource.TestCheckResourceAttr("relay_database_user.this", "restricted_resources.#", "0"),
					func(*terraform.State) error {
						if n := len(f.callsTo("CreateDatabaseUser")); n != 2 {
							return fmt.Errorf("a role change must replace the user, got %d creates", n)
						}
						if len(f.callsTo("UpdateDatabaseUser")) != 0 {
							return errors.New("a role change must never be sent as an update")
						}
						return nil
					},
				),
			},
		},
	})
}

func TestDatabaseUserResource_PasswordConflict(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:      userConfig(f, "reporting", "  password = \"x\"\n  generate_password = true"),
			ExpectError: regexp.MustCompile("Conflicting password settings"),
		}, {
			Config: userConfig(f, "reporting", "  password = \"x\"\n  generate_password = false"),
			Check:  resource.TestCheckResourceAttr("relay_database_user.this", "generate_password", "false"),
		}},
	})
}

func TestDatabaseUserResource_Failures(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	t.Run("create outcomes", func(t *testing.T) {
		cases := map[string]struct {
			prep  func(f *fakeServer)
			extra string
			want  string
		}{
			"rejected":     {func(f *fakeServer) { f.failWith("CreateDatabaseUser", unavailable) }, "", "Cannot create database user"},
			"failed phase": {func(f *fakeServer) { f.failUser = "role missing" }, "", `(?s)did not become ready.*role missing`},
			"poll error":   {func(f *fakeServer) { f.failWith("GetDatabaseUser", unavailable) }, "", `(?s)did not become ready.*down`},
			"timeout": {func(f *fakeServer) { f.stuck = true }, `
  timeouts {
    create = "30ms"
  }
`, `(?s)did not become ready.*timed out`},
			"bad expiry": {func(*fakeServer) {}, `  expires_at = "tomorrow"`, "Invalid expires_at"},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				f := startFakeServer(t)
				tc.prep(f)
				resource.UnitTest(t, resource.TestCase{
					ProtoV6ProviderFactories: factories(relay.Dial),
					Steps: []resource.TestStep{
						{Config: userConfig(f, "app", tc.extra), ExpectError: regexp.MustCompile(tc.want)},
						{
							PreConfig: func() {
								f.mu.Lock()
								defer f.mu.Unlock()
								f.errs, f.stuck, f.failUser = map[string]error{}, false, ""
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
		cfg := userConfig(f, "app", `  description = "a"`)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: factories(relay.Dial),
			Steps: []resource.TestStep{
				{Config: cfg},
				{
					PreConfig: func() {
						f.mu.Lock()
						defer f.mu.Unlock()
						for k := range f.users {
							delete(f.users, k)
						}
					},
					Config:             cfg,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
				{Config: cfg},
				{
					PreConfig:   func() { f.failWith("GetDatabaseUser", unavailable) },
					Config:      cfg,
					ExpectError: regexp.MustCompile("Cannot read database user"),
				},
				{
					PreConfig: func() {
						f.failWith("GetDatabaseUser", nil)
						f.failWith("UpdateDatabaseUser", unavailable)
					},
					Config:      userConfig(f, "app", `  description = "b"`),
					ExpectError: regexp.MustCompile("Cannot update database user"),
				},
				{
					PreConfig:   func() { f.failWith("UpdateDatabaseUser", nil) },
					Config:      userConfig(f, "app", "  description = \"b\"\n  expires_at = \"soon\""),
					ExpectError: regexp.MustCompile("Invalid expires_at"),
				},
				{
					PreConfig:   func() { f.failWith("DeleteDatabaseUser", unavailable) },
					Config:      providerBlock(f),
					ExpectError: regexp.MustCompile("Cannot delete database user"),
				},
				{
					PreConfig: func() { f.failWith("DeleteDatabaseUser", status.Error(codes.NotFound, "gone")) },
					Config:    providerBlock(f),
				},
				{
					Config:        userConfig(f, "app", ""),
					ResourceName:  "relay_database_user.this",
					ImportState:   true,
					ImportStateId: "ws/db-only",
					ExpectError:   regexp.MustCompile("Invalid import ID"),
				},
			},
		})
	})
}
