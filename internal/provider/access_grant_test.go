package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const (
	grantWorkspace = "ws-grants"
	grantAddr      = "relay_access_grant.this"
)

func accessGrantConfig(f *fakeServer, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_access_grant" "this" {
  workspace_id = %q
%s
}
`, grantWorkspace, body)
}

const teamGrant = `
  grantee_type = "team"
  grantee      = "payments"
  target_type  = "connection"
  target       = "payments-app"
`

func seedAccessGrant(f *fakeServer, g *serverv1.AccessGrant) {
	f.set(func() { f.grants[grantWorkspace] = append(f.grants[grantWorkspace], g) })
}

func noAccessGrantsLeft(f *fakeServer) tfresource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if n := len(f.grants[grantWorkspace]); n != 0 {
			return fmt.Errorf("%d grants left after destroy", n)
		}
		return nil
	}
}

func TestAccessGrantResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	temporary := teamGrant + `  expires_at   = "2099-01-01T00:00:00Z"` + "\n"
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noAccessGrantsLeft(f),
		Steps: []tfresource.TestStep{
			{
				Config: accessGrantConfig(f, temporary),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(grantAddr, "id", "grant-1"),
					tfresource.TestCheckResourceAttr(grantAddr, "grantee_type", "team"),
					tfresource.TestCheckResourceAttr(grantAddr, "grantee", "payments"),
					tfresource.TestCheckResourceAttr(grantAddr, "target_type", "connection"),
					tfresource.TestCheckResourceAttr(grantAddr, "target", "payments-app"),
					tfresource.TestCheckResourceAttr(grantAddr, "expires_at", "2099-01-01T00:00:00Z"),
					tfresource.TestCheckResourceAttr(grantAddr, "granted_by", "admin@example.com"),
					tfresource.TestCheckResourceAttr(grantAddr, "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						calls := f.callsTo("CreateAccessGrant")
						if len(calls) != 1 || calls[0].workspace != grantWorkspace || calls[0].org != testOrgID {
							return fmt.Errorf("create must be scoped to org and workspace: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				ResourceName:      grantAddr,
				ImportState:       true,
				ImportStateId:     grantWorkspace + "/grant-1",
				ImportStateVerify: true,
			},
			{
				Config: accessGrantConfig(f, `
  grantee_type = "team"
  grantee      = "payments"
  target_type  = "connection"
  target       = "payments-batch"
`),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(grantAddr, "id", "grant-2"),
					tfresource.TestCheckNoResourceAttr(grantAddr, "expires_at"),
					func(*terraform.State) error {
						if n := len(f.callsTo("DeleteAccessGrant")); n != 1 {
							return fmt.Errorf("a changed target must replace the grant, got %d deletes", n)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccessGrantResource_UserGrantee(t *testing.T) {
	f := startFakeServer(t)
	config := accessGrantConfig(f, `
  grantee_type = "user"
  grantee      = "Dev@Example.com"
  target_type  = "connection_group"
  target       = "analytics"
`)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noAccessGrantsLeft(f),
		Steps: []tfresource.TestStep{
			{
				Config: config,
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(grantAddr, "grantee", "Dev@Example.com"),
					func(*terraform.State) error {
						if got := f.lastGrant; got.GetGranteeType() != "user" || got.GetTargetType() != "connection_group" || got.ExpiresAt != nil {
							return fmt.Errorf("unexpected request: %+v", got)
						}
						return nil
					},
				),
			},
			{Config: config, PlanOnly: true},
			{
				ResourceName:            grantAddr,
				ImportState:             true,
				ImportStateId:           grantWorkspace + "/grant-1",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"grantee"},
			},
		},
	})
}

func TestAccessGrantResource_Drift(t *testing.T) {
	f := startFakeServer(t)
	config := accessGrantConfig(f, teamGrant)
	gone := func(drift func()) tfresource.TestStep {
		return tfresource.TestStep{
			PreConfig:          drift,
			Config:             config,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		}
	}
	recreate := tfresource.TestStep{Config: config}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noAccessGrantsLeft(f),
		Steps: []tfresource.TestStep{
			recreate,
			gone(func() {
				f.set(func() { f.grants[grantWorkspace][0].ExpiresAt = timestamppb.New(fakeEpoch) })
			}),
			{Config: config, Check: tfresource.TestCheckResourceAttr(grantAddr, "id", "grant-2")},
			gone(func() { f.set(func() { f.grants[grantWorkspace] = nil }) }),
			recreate,
			gone(func() { f.set(func() { f.grantTargetGone = true }) }),
			{PreConfig: func() { f.set(func() { f.grantTargetGone = false }) }, Config: config},
			{
				PreConfig:   func() { f.failWith("ListAccessGrants", status.Error(codes.Unavailable, "down")) },
				Config:      config,
				ExpectError: regexp.MustCompile("Cannot read access grant"),
			},
			{
				PreConfig: func() {
					f.failWith("ListAccessGrants", nil)
					f.failWith("DeleteAccessGrant", status.Error(codes.PermissionDenied, "denied"))
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot revoke access grant"),
			},
			{
				PreConfig: func() { f.failWith("DeleteAccessGrant", status.Error(codes.NotFound, "gone")) },
				Config:    providerBlock(f),
			},
		},
	})
}

func TestAccessGrantResource_CreateErrors(t *testing.T) {
	f := startFakeServer(t)
	seedAccessGrant(f, &serverv1.AccessGrant{Id: "grant-x", GranteeType: "team", GranteeName: "payments", TargetType: "connection", TargetName: "payments-app"})
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{
				Config:      accessGrantConfig(f, teamGrant),
				ExpectError: regexp.MustCompile("(?s)Cannot create access grant.*already exists"),
			},
			{
				Config: accessGrantConfig(f, `
  grantee_type = "team"
  grantee      = "ops"
  target_type  = "connection"
  target       = "payments-app"
  expires_at   = "2001-01-01T00:00:00Z"
`),
				ExpectError: regexp.MustCompile("must be in the future"),
			},
		},
	})
}

func TestAccessGrantResource_CreateOverExpired(t *testing.T) {
	f := startFakeServer(t)
	seedAccessGrant(f, &serverv1.AccessGrant{Id: "grant-old", GranteeType: "team", GranteeName: "payments", TargetType: "connection", TargetName: "payments-app", ExpiresAt: timestamppb.New(fakeEpoch)})
	conflict := regexp.MustCompile("already exists")
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noAccessGrantsLeft(f),
		Steps: []tfresource.TestStep{
			{
				PreConfig:   func() { f.failWith("ListAccessGrants", status.Error(codes.Unavailable, "down")) },
				Config:      accessGrantConfig(f, teamGrant),
				ExpectError: conflict,
			},
			{
				PreConfig: func() {
					f.failWith("ListAccessGrants", nil)
					f.failWith("DeleteAccessGrant", status.Error(codes.PermissionDenied, "denied"))
				},
				Config:      accessGrantConfig(f, teamGrant),
				ExpectError: conflict,
			},
			{
				PreConfig: func() { f.failWith("DeleteAccessGrant", nil) },
				Config:    accessGrantConfig(f, teamGrant),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(grantAddr, "id", "grant-1"),
					func(*terraform.State) error {
						if n := len(f.grants[grantWorkspace]); n != 1 {
							return fmt.Errorf("the expired row must be gone, %d grants", n)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccessGrantResource_Validation(t *testing.T) {
	f := startFakeServer(t)
	base := func(extra string) string {
		return accessGrantConfig(f, teamGrant+extra)
	}
	step := func(config, want string) tfresource.TestStep {
		return tfresource.TestStep{Config: config, PlanOnly: true, ExpectError: regexp.MustCompile(want)}
	}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			step(base(`  expires_at = "tomorrow"`), "Invalid timestamp"),
			step(base(`  expires_at = "2099-01-01T00:00:00+02:00"`), "Timestamp is not canonical"),
			step(accessGrantConfig(f, `
  grantee_type = "group"
  grantee      = "payments"
  target_type  = "connection"
  target       = "payments-app"
`), "grantee_type value must be one of"),
			step(accessGrantConfig(f, `
  grantee_type = "team"
  grantee      = "payments"
  target_type  = "database"
  target       = "payments-app"
`), "target_type value must be one of"),
		},
	})
}

func TestUTCTimestamp_Skips(t *testing.T) {
	v := utcTimestamp{}
	for _, in := range []types.String{types.StringNull(), types.StringUnknown()} {
		resp := &validator.StringResponse{}
		v.ValidateString(context.Background(), validator.StringRequest{ConfigValue: in}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%v must be skipped", in)
		}
	}
	if v.MarkdownDescription(context.Background()) != v.Description(context.Background()) {
		t.Fatal("descriptions must match")
	}
}

func TestAccessGrantResource_UpdateIsRejected(t *testing.T) {
	var resp resource.UpdateResponse
	(&accessGrantResource{}).Update(context.Background(), resource.UpdateRequest{}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update must fail")
	}
}

func TestAccessGrantResource_Import(t *testing.T) {
	f := startFakeServer(t)
	seedAccessGrant(f, &serverv1.AccessGrant{Id: "grant-live", GranteeType: "team", GranteeName: "payments", TargetType: "connection", TargetName: "payments-app", CreatedAt: timestamppb.New(fakeEpoch)})
	seedAccessGrant(f, &serverv1.AccessGrant{Id: "grant-dead", GranteeType: "team", GranteeName: "ops", TargetType: "connection", TargetName: "payments-app", ExpiresAt: timestamppb.New(fakeEpoch)})
	config := accessGrantConfig(f, teamGrant)
	importStep := func(id, want string) tfresource.TestStep {
		return tfresource.TestStep{Config: config, ResourceName: grantAddr, ImportState: true, ImportStateId: id, ExpectError: regexp.MustCompile(want)}
	}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			importStep("no-slash", "Invalid import ID"),
			importStep(grantWorkspace+"/missing", "has no access grant missing"),
			importStep(grantWorkspace+"/grant-dead", "Expired access grant"),
			{
				PreConfig:    func() { f.failWith("ListAccessGrants", status.Error(codes.PermissionDenied, "denied")) },
				Config:       config,
				ResourceName: grantAddr, ImportState: true, ImportStateId: grantWorkspace + "/grant-live",
				ExpectError: regexp.MustCompile("Cannot read access grants"),
			},
			{
				PreConfig:     func() { f.failWith("ListAccessGrants", nil) },
				Config:        config,
				ResourceName:  grantAddr,
				ImportState:   true,
				ImportStateId: grantWorkspace + "/grant-live",
				ImportStateCheck: func(s []*terraform.InstanceState) error {
					if len(s) != 1 || s[0].Attributes["target"] != "payments-app" || s[0].Attributes["workspace_id"] != grantWorkspace {
						return fmt.Errorf("unexpected import: %+v", s)
					}
					return nil
				},
			},
		},
	})
}
