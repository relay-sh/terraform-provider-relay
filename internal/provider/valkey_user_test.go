package provider

import (
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

func valkeyUserConfig(f *fakeServer, username, role, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_valkey_user" "this" {
  workspace_id = %q
  cluster_id   = "db-1"
  username     = %q
  role         = %q
%s
}
`, dbWorkspace, username, role, body)
}

func TestValkeyUserResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: valkeyUserConfig(f, "app", "readWrite", `
  password    = "first"
  description = "sessions"
  expires_at  = "2027-06-01T00:00:00-03:00"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_user.this", "id", "db-1/app"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "phase", "Ready"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "role", "readWrite"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "expires_at", "2027-06-01T00:00:00-03:00"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "generated_password", ""),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						c := f.lastUserCreate
						switch {
						case c.GetPassword() != "first" || c.GetAuthMethod() != "password" || c.GetGeneratePassword():
							return fmt.Errorf("create request = %+v", c)
						case len(c.GetRoles()) != 1 || c.GetRoles()[0].GetRole() != "readWrite" || c.GetRoles()[0].GetDb() != "":
							return fmt.Errorf("the role must be sent alone and without a database, got %+v", c.GetRoles())
						case c.ConnectionLimit != nil || c.StatementTimeout != nil || len(c.GetRestrictedResources()) != 0:
							return errors.New("Valkey rejects PostgreSQL user limits, so none may be sent")
						}
						if calls := f.callsTo("CreateDatabaseUser"); calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config: valkeyUserConfig(f, "app", "readOnly", `
  password    = "second"
  description = "sessions"
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_user.this", plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_user.this", "role", "readOnly"),
					resource.TestCheckNoResourceAttr("relay_valkey_user.this", "expires_at"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "phase", "Ready"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						u := f.lastUserUpdate
						switch {
						case u.GetPassword() != "second":
							return errors.New("a changed password must be sent")
						case len(u.GetRoles()) != 1 || u.GetRoles()[0].GetRole() != "readOnly":
							return fmt.Errorf("a changed role must be sent, got %+v", u.GetRoles())
						case u.GetExpiresAt() == nil || !u.GetExpiresAt().AsTime().IsZero():
							return errors.New("removing expires_at must send a zero expiry")
						}
						return nil
					},
				),
			},
			{
				Config: valkeyUserConfig(f, "app", "readOnly", `
  password    = "second"
  description = "cache"
  expires_at  = "2028-01-01T00:00:00Z"
`),
				Check: func(*terraform.State) error {
					u := f.lastUserUpdate
					if u.Password != nil || len(u.GetRoles()) != 0 || u.GetDescription() != "cache" || u.GetExpiresAt().AsTime().Year() != 2028 {
						return fmt.Errorf("only changed fields may be sent, got %+v", u)
					}
					return nil
				},
			},
			{
				ResourceName:            "relay_valkey_user.this",
				ImportState:             true,
				ImportStateId:           dbWorkspace + "/db-1/app",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password", "generate_password", "generated_password", "timeouts"},
			},
		},
	})
}

func TestValkeyUserResource_GeneratedPassword(t *testing.T) {
	f := startFakeServer(t)
	f.userWithoutTimes = true
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: valkeyUserConfig(f, "worker", "admin", `  generate_password = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_user.this", "generated_password", "generated-secret"),
					resource.TestCheckResourceAttr("relay_valkey_user.this", "created_at", ""),
					func(*terraform.State) error {
						if c := f.lastUserCreate; !c.GetGeneratePassword() || c.GetPassword() != "" {
							return fmt.Errorf("create request = %+v", c)
						}
						return nil
					},
				),
			},
			{
				Config: valkeyUserConfig(f, "worker", "admin", `  password = "mine"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_user.this", plancheck.ResourceActionDestroyBeforeCreate)},
				},
			},
		},
	})
}

func TestValkeyUserResource_ImportWithGeneratePasswordUpdatesInPlace(t *testing.T) {
	f := startFakeServer(t)
	f.users[userKey("db-1", "app")] = &serverv1.DatabaseUser{
		Username:   "app",
		AuthMethod: "password",
		Roles:      []*serverv1.DatabaseUserRole{{Role: "readWrite"}},
		Phase:      "Ready",
		CreatedAt:  timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	config := valkeyUserConfig(f, "app", "readWrite", `  generate_password = true`)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       "relay_valkey_user.this",
				ImportState:        true,
				ImportStateId:      dbWorkspace + "/db-1/app",
				ImportStatePersist: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_user.this", plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if n := len(f.callsTo("CreateDatabaseUser")); n != 0 {
						return fmt.Errorf("an imported user must not be recreated, got %d creates", n)
					}
					return nil
				},
			},
		},
	})
}

func TestValkeyUserResource_ClusterReference(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: valkeyConfig(f, valkeyGroup("cache", 1, "1Gi", "")) + fmt.Sprintf(`
resource "relay_valkey_user" "this" {
  workspace_id      = %q
  cluster_id        = relay_valkey_cluster.this.id
  username          = "app"
  role              = "readWrite"
  generate_password = true
}
`, dbWorkspace),
			Check: resource.TestCheckResourceAttrPair("relay_valkey_user.this", "cluster_id", "relay_valkey_cluster.this", "id"),
		}},
	})
}

func TestValkeyUserResource_ConfigGuards(t *testing.T) {
	f := startFakeServer(t)
	cases := map[string]struct {
		username, role, body, want string
	}{
		"both passwords":    {"app", "admin", "  password = \"x\"\n  generate_password = true", "Conflicting password settings"},
		"no password":       {"app", "admin", "", "Missing password"},
		"unknown role":      {"app", "owner", `  password = "x"`, "value must be one of"},
		"uppercase":         {"App", "admin", `  password = "x"`, "must start with a lowercase letter"},
		"reserved default":  {"default", "admin", `  password = "x"`, "value must be none of"},
		"reserved prefix":   {"relay-backup", "admin", `  password = "x"`, "Reserved username"},
		"too long username": {"a" + fmt.Sprintf("%063d", 0), "admin", `  password = "x"`, "length must be at most 63"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      valkeyUserConfig(f, tc.username, tc.role, tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}

func TestValkeyUserResource_UnknownPasswordPlans(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: "resource \"terraform_data\" \"later\" {\n  input = true\n}\n" +
				valkeyUserConfig(f, "app", "admin", `  generate_password = terraform_data.later.output`),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		}},
	})
}

func TestValkeyUserResource_Failures(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	t.Run("create outcomes", func(t *testing.T) {
		cases := map[string]struct {
			prep  func(f *fakeServer)
			extra string
			want  string
		}{
			"rejected":     {func(f *fakeServer) { f.failWith("CreateDatabaseUser", unavailable) }, "", "Cannot create Valkey user"},
			"failed phase": {func(f *fakeServer) { f.failUser = "cluster not running" }, "", `(?s)did not become ready.*cluster not running`},
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
						{Config: valkeyUserConfig(f, "app", "admin", "  password = \"x\"\n"+tc.extra), ExpectError: regexp.MustCompile(tc.want)},
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
		cfg := valkeyUserConfig(f, "app", "admin", "  password = \"x\"\n  description = \"a\"")
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
					ExpectError: regexp.MustCompile("Cannot read Valkey user"),
				},
				{
					PreConfig: func() {
						f.failWith("GetDatabaseUser", nil)
						f.failWith("UpdateDatabaseUser", unavailable)
					},
					Config:      valkeyUserConfig(f, "app", "admin", "  password = \"x\"\n  description = \"b\""),
					ExpectError: regexp.MustCompile("Cannot update Valkey user"),
				},
				{
					PreConfig:   func() { f.failWith("UpdateDatabaseUser", nil) },
					Config:      valkeyUserConfig(f, "app", "admin", "  password = \"x\"\n  description = \"b\"\n  expires_at = \"soon\""),
					ExpectError: regexp.MustCompile("Invalid expires_at"),
				},
				{
					PreConfig:   func() { f.failWith("DeleteDatabaseUser", unavailable) },
					Config:      providerBlock(f),
					ExpectError: regexp.MustCompile("Cannot delete Valkey user"),
				},
				{
					PreConfig: func() { f.failWith("DeleteDatabaseUser", status.Error(codes.NotFound, "gone")) },
					Config:    providerBlock(f),
				},
				{
					Config:        valkeyUserConfig(f, "app", "admin", `  password = "x"`),
					ResourceName:  "relay_valkey_user.this",
					ImportState:   true,
					ImportStateId: "ws/db-only",
					ExpectError:   regexp.MustCompile("Invalid import ID"),
				},
			},
		})
	})
}

func TestValkeyUserFromProto(t *testing.T) {
	prior := valkeyUserModel{
		ClusterID: types.StringValue("db-1"), Role: types.StringValue("admin"),
		Phase: types.StringNull(), CreatedAt: types.StringNull(), GeneratedPassword: types.StringNull(),
	}
	got := valkeyUserFromProto(&serverv1.DatabaseUser{Username: "app"}, prior)
	if got.Role.ValueString() != "admin" {
		t.Errorf("a response without roles must keep the configured role, got %v", got.Role)
	}
	if got.Phase.ValueString() != "" || got.PhaseMessage.ValueString() != "" || got.CreatedAt.ValueString() != "" || got.GeneratedPassword.ValueString() != "" {
		t.Errorf("unknown computed values must settle to empty strings, got %+v", got)
	}
	prior.Phase, prior.PhaseMessage = types.StringValue("Ready"), types.StringValue("")
	if got := valkeyUserFromProto(&serverv1.DatabaseUser{Username: "app"}, prior); got.Phase.ValueString() != "Ready" {
		t.Errorf("a response without a phase must keep the known one, got %v", got.Phase)
	}
}
