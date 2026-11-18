package provider

import (
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

const documentDBUserAddr = "relay_documentdb_user.this"

func documentDBUserConfig(f *fakeServer, username, role, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_documentdb_user" "this" {
  workspace_id = %q
  cluster_id   = "db-1"
  username     = %q
  role         = %q
%s
}
`, dbWorkspace, username, role, body)
}

func TestDocumentDBUserResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: documentDBUserConfig(f, "app_reader", "readOnly", `
  password    = "first"
  description = "reports"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBUserAddr, "id", "db-1/app_reader"),
					resource.TestCheckResourceAttr(documentDBUserAddr, "phase", "Ready"),
					resource.TestCheckResourceAttr(documentDBUserAddr, "role", "readOnly"),
					resource.TestCheckResourceAttr(documentDBUserAddr, "generated_password", ""),
					resource.TestCheckResourceAttr(documentDBUserAddr, "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						c := f.lastUserCreate
						switch {
						case c.GetPassword() != "first" || c.GetAuthMethod() != "password" || c.GetGeneratePassword():
							return fmt.Errorf("create request = %+v", c)
						case len(c.GetRoles()) != 1 || c.GetRoles()[0].GetRole() != "readOnly" || c.GetRoles()[0].GetDb() != "":
							return fmt.Errorf("the role must be sent alone and without a database, got %+v", c.GetRoles())
						case c.GetPrincipalArn() != "" || c.GetExpiresAt() != nil || c.ConnectionLimit != nil || c.StatementTimeout != nil || len(c.GetRestrictedResources()) != 0:
							return fmt.Errorf("DocumentDB rejects IAM, expiry and PostgreSQL limits, got %+v", c)
						}
						if calls := f.callsTo("CreateDatabaseUser"); calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config: documentDBUserConfig(f, "app_reader", "admin", `
  password    = "second"
  description = "reports"
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(documentDBUserAddr, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBUserAddr, "role", "admin"),
					resource.TestCheckResourceAttr(documentDBUserAddr, "phase", "Ready"),
					resource.TestCheckResourceAttr(documentDBUserAddr, "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						u := f.lastUserUpdate
						if u.GetPassword() != "second" || len(u.GetRoles()) != 1 || u.GetRoles()[0].GetRole() != "admin" {
							return fmt.Errorf("a changed password and role must be sent, got %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: documentDBUserConfig(f, "app_reader", "admin", `
  password    = "second"
  description = "dashboards"
`),
				Check: func(*terraform.State) error {
					if u := f.lastUserUpdate; u.Password != nil || len(u.GetRoles()) != 0 || u.GetDescription() != "dashboards" {
						return fmt.Errorf("only changed fields may be sent, got %+v", u)
					}
					return nil
				},
			},
			{
				ResourceName:            documentDBUserAddr,
				ImportState:             true,
				ImportStateId:           dbWorkspace + "/db-1/app_reader",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password", "generate_password", "generated_password", "timeouts"},
			},
		},
	})
}

func TestDocumentDBUserResource_GeneratedPassword(t *testing.T) {
	f := startFakeServer(t)
	f.userWithoutTimes = true
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noUsersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: documentDBUserConfig(f, "worker", "admin", `  generate_password = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBUserAddr, "generated_password", "generated-secret"),
					resource.TestCheckResourceAttr(documentDBUserAddr, "created_at", ""),
					func(*terraform.State) error {
						if c := f.lastUserCreate; !c.GetGeneratePassword() || c.GetPassword() != "" {
							return fmt.Errorf("create request = %+v", c)
						}
						return nil
					},
				),
			},
			{
				Config: documentDBUserConfig(f, "worker", "admin", `  password = "mine"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(documentDBUserAddr, plancheck.ResourceActionDestroyBeforeCreate)},
				},
			},
		},
	})
}

func TestDocumentDBUserResource_ImportWithGeneratePasswordUpdatesInPlace(t *testing.T) {
	f := startFakeServer(t)
	f.users[userKey("db-1", "app")] = &serverv1.DatabaseUser{
		Username:   "app",
		AuthMethod: "password",
		Roles:      []*serverv1.DatabaseUserRole{{Role: "readOnly"}},
		Phase:      "Ready",
		CreatedAt:  timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	config := documentDBUserConfig(f, "app", "readOnly", `  generate_password = true`)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       documentDBUserAddr,
				ImportState:        true,
				ImportStateId:      dbWorkspace + "/db-1/app",
				ImportStatePersist: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(documentDBUserAddr, plancheck.ResourceActionUpdate)},
				},
				Check: callCount(f, "CreateDatabaseUser", 0),
			},
		},
	})
}

func TestDocumentDBUserResource_ClusterReference(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: documentDBCluster(f, "17", "", "") + fmt.Sprintf(`
resource "relay_documentdb_user" "this" {
  workspace_id      = %q
  cluster_id        = relay_documentdb_cluster.this.id
  username          = "app"
  role              = "admin"
  generate_password = true
}
`, dbWorkspace),
			Check: resource.TestCheckResourceAttrPair(documentDBUserAddr, "cluster_id", documentDBAddr, "id"),
		}},
	})
}

func TestDocumentDBUserResource_ConfigGuards(t *testing.T) {
	f := startFakeServer(t)
	cases := map[string]struct {
		username, role, body, want string
	}{
		"both passwords":    {"app", "admin", "  password = \"x\"\n  generate_password = true", "Conflicting password settings"},
		"no password":       {"app", "admin", "", "Missing password"},
		"read-write role":   {"app", "readWrite", `  password = "x"`, "value must be one of"},
		"hyphen":            {"app-user", "admin", `  password = "x"`, "only lowercase letters, digits and underscores"},
		"reserved postgres": {"postgres", "admin", `  password = "x"`, "value must be none of"},
		"reserved prefix":   {"pg_monitor", "admin", `  password = "x"`, "Reserved username"},
		"too long username": {"a" + fmt.Sprintf("%063d", 0), "admin", `  password = "x"`, "length must be at most 63"},
		"iam":               {"app", "admin", "  password = \"x\"\n  auth_method = \"aws_iam\"", `An argument named "auth_method" is not expected here`},
		"expiry":            {"app", "admin", "  password = \"x\"\n  expires_at = \"2030-01-01T00:00:00Z\"", `An argument named "expires_at" is not expected here`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      documentDBUserConfig(f, tc.username, tc.role, tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}

func TestDocumentDBUserResource_UnknownPasswordPlans(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: "resource \"terraform_data\" \"later\" {\n  input = true\n}\n" +
				documentDBUserConfig(f, "app", "admin", `  generate_password = terraform_data.later.output`),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		}},
	})
}

func TestDocumentDBUserResource_Failures(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	t.Run("create outcomes", func(t *testing.T) {
		cases := map[string]struct {
			prep  func(f *fakeServer)
			extra string
			want  string
		}{
			"rejected":     {func(f *fakeServer) { f.failWith("CreateDatabaseUser", unavailable) }, "", "Cannot create DocumentDB user"},
			"failed phase": {func(f *fakeServer) { f.failUser = "cluster not running" }, "", `(?s)did not become ready.*cluster not running`},
			"timeout": {func(f *fakeServer) { f.stuck = true }, `
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
					Steps: []resource.TestStep{
						{Config: documentDBUserConfig(f, "app", "admin", "  password = \"x\"\n"+tc.extra), ExpectError: regexp.MustCompile(tc.want)},
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
		cfg := documentDBUserConfig(f, "app", "admin", "  password = \"x\"\n  description = \"a\"")
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: factories(relay.Dial),
			Steps: []resource.TestStep{
				{Config: cfg},
				{
					PreConfig: func() {
						f.set(func() {
							for k := range f.users {
								delete(f.users, k)
							}
						})
					},
					Config:             cfg,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
				{Config: cfg},
				{
					PreConfig:   func() { f.failWith("GetDatabaseUser", unavailable) },
					Config:      cfg,
					ExpectError: regexp.MustCompile("Cannot read DocumentDB user"),
				},
				{
					PreConfig: func() {
						f.failWith("GetDatabaseUser", nil)
						f.failWith("UpdateDatabaseUser", unavailable)
					},
					Config:      documentDBUserConfig(f, "app", "admin", "  password = \"x\"\n  description = \"b\""),
					ExpectError: regexp.MustCompile("Cannot update DocumentDB user"),
				},
				{
					PreConfig:   func() { f.failWith("DeleteDatabaseUser", unavailable) },
					Config:      providerBlock(f),
					ExpectError: regexp.MustCompile("Cannot delete DocumentDB user"),
				},
				{
					PreConfig: func() { f.failWith("DeleteDatabaseUser", status.Error(codes.NotFound, "gone")) },
					Config:    providerBlock(f),
				},
				{
					Config:        documentDBUserConfig(f, "app", "admin", `  password = "x"`),
					ResourceName:  documentDBUserAddr,
					ImportState:   true,
					ImportStateId: "ws/db-only",
					ExpectError:   regexp.MustCompile("Invalid import ID"),
				},
			},
		})
	})
}

func TestDocumentDBUserFromProto(t *testing.T) {
	prior := documentDBUserModel{
		ClusterID: types.StringValue("db-1"), Role: types.StringValue("admin"),
		Phase: types.StringNull(), CreatedAt: types.StringNull(), GeneratedPassword: types.StringNull(),
	}
	got := documentDBUserFromProto(&serverv1.DatabaseUser{Username: "app"}, prior)
	if got.Role.ValueString() != "admin" {
		t.Errorf("a response without roles must keep the configured role, got %v", got.Role)
	}
	if got.Phase.ValueString() != "" || got.PhaseMessage.ValueString() != "" || got.CreatedAt.ValueString() != "" || got.GeneratedPassword.ValueString() != "" {
		t.Errorf("unknown computed values must settle to empty strings, got %+v", got)
	}
	prior.Phase, prior.PhaseMessage = types.StringValue("Ready"), types.StringValue("")
	if got := documentDBUserFromProto(&serverv1.DatabaseUser{Username: "app"}, prior); got.Phase.ValueString() != "Ready" {
		t.Errorf("a response without a phase must keep the known one, got %v", got.Phase)
	}
}
