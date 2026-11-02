package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

func apiKeyConfig(f *fakeServer, body string) string {
	return providerBlock(f) + `
resource "relay_api_key" "this" {
` + body + `
}
`
}

func noAPIKeysLeft(f *fakeServer) tfresource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		left := len(f.apiKeys)
		if f.callerKey != nil && f.apiKeys[f.callerKey.GetId()] != nil {
			left--
		}
		if left != 0 {
			return fmt.Errorf("API keys left after destroy: %v", f.apiKeys)
		}
		return nil
	}
}

func TestAPIKeyResource_OrganizationLifecycle(t *testing.T) {
	f := startFakeServer(t)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noAPIKeysLeft(f),
		Steps: []tfresource.TestStep{
			{
				Config: apiKeyConfig(f, `
  name          = "ci"
  role          = "viewer"
  allowed_cidrs = ["10.0.0.0/8", "192.168.1.10"]
  expires_in    = "720h"
`),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "id", "key-1"),
					tfresource.TestCheckNoResourceAttr("relay_api_key.this", "workspace_id"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "secret", "rly_key_key-1"),
					tfresource.TestCheckTypeSetElemAttr("relay_api_key.this", "allowed_cidrs.*", "192.168.1.10"),
					tfresource.TestCheckTypeSetElemAttr("relay_api_key.this", "effective_allowed_cidrs.*", "192.168.1.10/32"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "expires_at", "2027-02-14T08:00:00Z"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "created_at", "2027-01-15T08:00:00Z"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "parent_key_id", ""),
					func(*terraform.State) error {
						if got := f.lastKeyCreate.GetExpiresInSeconds(); got != 720*3600 {
							return fmt.Errorf("expires_in_seconds = %d", got)
						}
						if calls := f.callsTo("CreateAPIKey"); calls[0].org != testOrgID || calls[0].workspace != "" {
							return fmt.Errorf("create must be org-scoped only: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config: apiKeyConfig(f, `
  name          = "implantação contínua de produção — integração e configuração da esteira de entrega ção ção"
  role          = "admin"
  allowed_cidrs = ["10.0.0.0/8", "192.168.1.10"]
  expires_in    = "720h"
`),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "role", "admin"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "secret", "rly_key_key-1"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "expires_at", "2027-02-14T08:00:00Z"),
					func(*terraform.State) error {
						u := f.lastKeyUpdate
						if u.GetRoleSlug() != "admin" || u.ExpiresInSeconds != nil || u.GetName() != "implantação contínua de produção — integração e configuração da esteira de entrega ção ção" {
							return fmt.Errorf("update = %+v", u)
						}
						if len(f.callsTo("CreateAPIKey")) != 1 {
							return errors.New("role and name must update in place")
						}
						return nil
					},
				),
			},
			{
				Config: apiKeyConfig(f, `
  name = "ci-renamed"
  role = "admin"
`),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckNoResourceAttr("relay_api_key.this", "expires_at"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "effective_allowed_cidrs.#", "0"),
					func(*terraform.State) error {
						u := f.lastKeyUpdate
						if u.RoleSlug != nil || u.ExpiresInSeconds == nil || u.GetExpiresInSeconds() != 0 || len(u.GetAllowedCidrs()) != 0 {
							return fmt.Errorf("removing expires_in and allowed_cidrs must send 0 and an empty list: %+v", u)
						}
						return nil
					},
				),
			},
			{
				ResourceName:            "relay_api_key.this",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
		},
	})
}

func TestAPIKeyResource_WorkspaceKeyInheritsFromParent(t *testing.T) {
	f := startFakeServer(t)
	f.workspaces["ws-1"] = &serverv1.Workspace{Id: "ws-1", Name: "prod"}
	f.workspaces["ws-2"] = &serverv1.Workspace{Id: "ws-2", Name: "staging"}
	f.callerKey = &serverv1.APIKey{Id: "parent", AllowedCidrs: []string{"0.0.0.0/0"}, ExpiresAt: timestamppb.New(fakeNow.AddDate(0, 1, 0))}
	f.apiKeys["parent"] = f.callerKey
	workspaceKey := func(ws string) string {
		return apiKeyConfig(f, fmt.Sprintf(`
  name         = "deployer"
  workspace_id = %q
  role         = "viewer"
`, ws))
	}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noAPIKeysLeft(f),
		Steps: []tfresource.TestStep{
			{
				Config: workspaceKey("ws-1"),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "workspace_id", "ws-1"),
					tfresource.TestCheckNoResourceAttr("relay_api_key.this", "allowed_cidrs"),
					tfresource.TestCheckTypeSetElemAttr("relay_api_key.this", "effective_allowed_cidrs.*", "0.0.0.0/0"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "expires_at", "2027-02-15T08:00:00Z"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "parent_key_id", "parent"),
				),
			},
			{Config: workspaceKey("ws-1"), PlanOnly: true},
			{
				ResourceName:            "relay_api_key.this",
				ImportState:             true,
				ImportStateIdFunc:       func(*terraform.State) (string, error) { return "ws-1/key-1", nil },
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
			{
				Config: workspaceKey("ws-2"),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "workspace_id", "ws-2"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "id", "key-2"),
				),
			},
		},
	})
}

func TestAPIKeyResource_ImportOfUserKeyKeepsItsAddresses(t *testing.T) {
	f := startFakeServer(t)
	f.apiKeys["key-9"] = &serverv1.APIKey{Id: "key-9", Name: "dash", RoleSlug: "viewer", AllowedCidrs: []string{"10.0.0.0/8"}, CreatedAt: timestamppb.New(fakeNow)}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config: apiKeyConfig(f, `
  name          = "dash"
  role          = "viewer"
  allowed_cidrs = ["10.0.0.0/8"]
`),
			ResourceName:       "relay_api_key.this",
			ImportState:        true,
			ImportStateId:      "key-9",
			ImportStatePersist: true,
		}, {
			Config: apiKeyConfig(f, `
  name          = "dash"
  role          = "viewer"
  allowed_cidrs = ["10.0.0.0/8"]
`),
			PlanOnly: true,
		}, {
			Config: apiKeyConfig(f, `
  name          = "dashboard"
  role          = "viewer"
  allowed_cidrs = ["10.0.0.0/8"]
`),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr("relay_api_key.this", "name", "dashboard"),
				tfresource.TestCheckNoResourceAttr("relay_api_key.this", "secret"),
			),
		}},
	})
}

func TestAPIKeyResource_ImportOfChildKeyPlansNoChange(t *testing.T) {
	f := startFakeServer(t)
	f.callerKey = &serverv1.APIKey{Id: "tf", RoleSlug: "admin", CreatedAt: timestamppb.New(fakeNow.AddDate(0, -1, 0))}
	f.apiKeys["tf"] = f.callerKey
	expiresAt := fakeNow.Add(24 * time.Hour)
	f.apiKeys["key-7"] = &serverv1.APIKey{
		Id: "key-7", Name: "ci", RoleSlug: "viewer", AllowedCidrs: []string{"203.0.113.0/24"},
		CreatedAt: timestamppb.New(fakeNow.Add(-3 * time.Millisecond)), ExpiresAt: timestamppb.New(expiresAt), ParentKeyId: "tf",
	}
	config := func(name, lifetime string) string {
		return apiKeyConfig(f, fmt.Sprintf(`
  name          = %q
  role          = "viewer"
  allowed_cidrs = ["203.0.113.0/24"]
  expires_in    = %q
`, name, lifetime))
	}
	expiryKept := func(*terraform.State) error {
		if u := f.lastKeyUpdate; u.ExpiresInSeconds != nil {
			return fmt.Errorf("an imported key's expiry must not restart: %+v", u)
		}
		return nil
	}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{
				Config:             config("ci", "24h"),
				ResourceName:       "relay_api_key.this",
				ImportState:        true,
				ImportStateId:      "key-7",
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if got := states[0].Attributes["expires_in"]; got != "24h" {
						return fmt.Errorf("expires_in = %q", got)
					}
					if got := states[0].Attributes["allowed_cidrs.0"]; got != "203.0.113.0/24" {
						return fmt.Errorf("allowed_cidrs = %q", got)
					}
					return nil
				},
			},
			{Config: config("ci", "24h"), PlanOnly: true},
			{
				Config: config("ci", "1440m"),
				ConfigPlanChecks: tfresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("relay_api_key.this", plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue("relay_api_key.this", tfjsonpath.New("expires_at"), knownvalue.StringExact("2027-01-16T08:00:00Z")),
				}},
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "expires_at", "2027-01-16T08:00:00Z"),
					expiryKept,
				),
			},
			{
				Config: config("ci-renamed", "1440m"),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "expires_at", "2027-01-16T08:00:00Z"),
					expiryKept,
				),
			},
			{
				Config: config("ci-renamed", "48h"),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "expires_at", "2027-01-17T08:00:00Z"),
					func(*terraform.State) error {
						if got := f.lastKeyUpdate.GetExpiresInSeconds(); got != 48*3600 {
							return fmt.Errorf("a changed lifetime must be sent: %d", got)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAPIKeyResource_ImportOfKeyInheritingItsParent(t *testing.T) {
	f := startFakeServer(t)
	f.workspaces["ws-1"] = &serverv1.Workspace{Id: "ws-1", Name: "prod"}
	parentExpiry := timestamppb.New(fakeNow.AddDate(0, 2, 0))
	f.apiKeys["org-parent"] = &serverv1.APIKey{Id: "org-parent", AllowedCidrs: []string{"10.0.0.0/8"}, ExpiresAt: parentExpiry}
	seed := func() {
		f.set(func() {
			f.apiKeys["key-5"] = &serverv1.APIKey{
				Id: "key-5", Name: "deployer", WorkspaceId: "ws-1", RoleSlug: "viewer", AllowedCidrs: []string{"10.0.0.0/8"},
				CreatedAt: timestamppb.New(fakeNow), ExpiresAt: parentExpiry, ParentKeyId: "org-parent",
			}
		})
	}
	importAndPlan := func(config string, parentHidden bool) []tfresource.TestStep {
		return []tfresource.TestStep{
			{
				PreConfig: func() {
					seed()
					if parentHidden {
						f.failAfter("ListAPIKeys", 1, status.Error(codes.PermissionDenied, "workspace-scoped caller"))
					}
				},
				Config:             config,
				ResourceName:       "relay_api_key.this",
				ImportState:        true,
				ImportStateId:      "ws-1/key-5",
				ImportStatePersist: true,
			},
			{
				PreConfig: func() { f.failWith("ListAPIKeys", nil) },
				Config:    config,
				PlanOnly:  true,
			},
		}
	}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: importAndPlan(apiKeyConfig(f, `
  name         = "deployer"
  workspace_id = "ws-1"
  role         = "viewer"
`), false),
	})
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: importAndPlan(apiKeyConfig(f, `
  name          = "deployer"
  workspace_id  = "ws-1"
  role          = "viewer"
  allowed_cidrs = ["10.0.0.0/8"]
  expires_in    = "1416h"
`), true),
	})
}

func TestAPIKeyLifetimeHelpers(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:           "45s",
		30 * time.Minute:           "30m",
		90 * time.Second:           "1m30s",
		24 * time.Hour:             "24h",
		90 * time.Minute:           "1h30m",
		time.Hour + 30*time.Second: "1h0m30s",
		8760 * time.Hour:           "8760h",
		2*time.Hour + 5*time.Minute + 7*time.Second: "2h5m7s",
	} {
		if got := formatLifetime(d); got != want {
			t.Errorf("formatLifetime(%s) = %q, want %q", d, got, want)
		}
	}
	for _, tc := range []struct {
		a, b types.String
		want bool
	}{
		{types.StringValue("24h"), types.StringValue("1440m"), true},
		{types.StringValue("24h"), types.StringValue("48h"), false},
		{types.StringNull(), types.StringNull(), true},
		{types.StringNull(), types.StringValue("24h"), false},
		{types.StringUnknown(), types.StringValue("24h"), false},
		{types.StringValue("soon"), types.StringValue("soon"), true},
		{types.StringValue("24h"), types.StringValue("soon"), false},
	} {
		if got := sameLifetime(tc.a, tc.b); got != tc.want {
			t.Errorf("sameLifetime(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
	created := timestamppb.New(fakeNow)
	for name, key := range map[string]*serverv1.APIKey{
		"no expiry":      {CreatedAt: created},
		"no created_at":  {ExpiresAt: timestamppb.New(fakeNow.Add(time.Hour))},
		"too long":       {CreatedAt: created, ExpiresAt: timestamppb.New(fakeNow.Add(9000 * time.Hour))},
		"already passed": {CreatedAt: created, ExpiresAt: created},
	} {
		if got := ownLifetime(key, nil); !got.IsNull() {
			t.Errorf("%s: ownLifetime = %s, want null", name, got)
		}
	}
}

func TestAPIKeyValidators(t *testing.T) {
	ctx := context.Background()
	for _, v := range []interface {
		Description(context.Context) string
		MarkdownDescription(context.Context) string
	}{lifetimeValidator{}, addressValidator{}} {
		if v.MarkdownDescription(ctx) == "" || v.MarkdownDescription(ctx) != v.Description(ctx) {
			t.Errorf("%T must describe itself", v)
		}
	}
	for _, value := range []types.String{types.StringNull(), types.StringUnknown()} {
		var resp validator.StringResponse
		addressValidator{}.ValidateString(ctx, validator.StringRequest{ConfigValue: value}, &resp)
		lifetimeValidator{}.ValidateString(ctx, validator.StringRequest{ConfigValue: value}, &resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("%s must not be judged", value)
		}
	}
}

func TestAPIKeyResource_ExpiredKeyIsReplaced(t *testing.T) {
	f := startFakeServer(t)
	config := apiKeyConfig(f, `
  name       = "ci"
  role       = "viewer"
  expires_in = "1h"
`)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noAPIKeysLeft(f),
		Steps: []tfresource.TestStep{
			{Config: config},
			{Config: config, PlanOnly: true},
			{
				PreConfig: func() { f.set(func() { f.apiKeys["key-1"].ExpiresAt = timestamppb.New(fakeNow) }) },
				Config:    config,
				ConfigPlanChecks: tfresource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_api_key.this", plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("relay_api_key.this", "id", "key-2"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "secret", "rly_key_key-2"),
					tfresource.TestCheckResourceAttr("relay_api_key.this", "expires_at", "2027-01-15T09:00:00Z"),
					func(*terraform.State) error {
						if _, ok := f.apiKeys["key-1"]; ok || len(f.callsTo("RevokeAPIKey")) != 1 {
							return errors.New("the expired key must be revoked, not left behind")
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAPIKeyResource_Drift(t *testing.T) {
	f := startFakeServer(t)
	config := apiKeyConfig(f, `
  name          = "ci"
  role          = "viewer"
  allowed_cidrs = ["10.0.0.0/8"]
`)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{Config: config},
			{
				PreConfig:          func() { f.set(func() { f.apiKeys["key-1"].AllowedCidrs = nil }) },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig:          func() { f.set(func() { f.apiKeys["key-1"].AllowedCidrs = []string{"172.16.0.0/12"} }) },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				Check:  tfresource.TestCheckTypeSetElemAttr("relay_api_key.this", "effective_allowed_cidrs.*", "10.0.0.0/8"),
			},
			{
				PreConfig:          func() { f.set(func() { delete(f.apiKeys, "key-1") }) },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAPIKeyResource_WorkspaceGoneRemovesKey(t *testing.T) {
	f := startFakeServer(t)
	f.workspaces["ws-1"] = &serverv1.Workspace{Id: "ws-1", Name: "prod"}
	config := apiKeyConfig(f, `
  name         = "ci"
  workspace_id = "ws-1"
  role         = "viewer"
`)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					f.set(func() {
						delete(f.workspaces, "ws-1")
						delete(f.apiKeys, "key-1")
					})
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAPIKeyResource_Failures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	config := func(role string) string {
		return apiKeyConfig(f, fmt.Sprintf(`
  name = "ci"
  role = %q
`, role))
	}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{Config: config("owner"), ExpectError: regexp.MustCompile(`(?s)Cannot create API key.*owner-level`)},
			{Config: config("viewer")},
			{
				PreConfig:   func() { f.failWith("ListAPIKeys", unavailable) },
				Config:      config("viewer"),
				ExpectError: regexp.MustCompile("Cannot read API key"),
			},
			{
				PreConfig: func() {
					f.failWith("ListAPIKeys", nil)
					f.failWith("UpdateAPIKey", unavailable)
				},
				Config:      config("admin"),
				ExpectError: regexp.MustCompile("Cannot update API key"),
			},
			{
				PreConfig: func() {
					f.failWith("UpdateAPIKey", nil)
					f.failWith("RevokeAPIKey", unavailable)
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot revoke API key"),
			},
			{
				PreConfig: func() { f.failWith("RevokeAPIKey", status.Error(codes.NotFound, "gone")) },
				Config:    providerBlock(f),
			},
		},
	})
}

func TestAPIKeyResource_InvalidConfig(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"bad address":       {`name = "ci"` + "\n" + `role = "viewer"` + "\n" + `allowed_cidrs = ["10.0.0.0/33"]`, "is not an IP address or CIDR block"},
		"bad lifetime":      {`name = "ci"` + "\n" + `role = "viewer"` + "\n" + `expires_in = "30d"`, "Invalid expires_in"},
		"lifetime too long": {`name = "ci"` + "\n" + `role = "viewer"` + "\n" + `expires_in = "8761h"`, "at most 8760h"},
		"padded name":       {`name = "ci "` + "\n" + `role = "viewer"`, "must not be empty or start or end with whitespace"},
		"padded role":       {`name = "ci"` + "\n" + `role = " viewer"`, "must not be empty or start or end with whitespace"},
		"long name":         {`name = "` + strings.Repeat("a", 101) + `"` + "\n" + `role = "viewer"`, "UTF-8 character count must be between 1 and 100"},
		"empty name":        {`name = ""` + "\n" + `role = "viewer"`, "UTF-8 character count must be between 1 and 100"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			tfresource.UnitTest(t, tfresource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []tfresource.TestStep{{Config: apiKeyConfig(f, tc.body), PlanOnly: true, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestAPIKeyResource_InvalidImportID(t *testing.T) {
	for _, id := range []string{"a/b/c", "ws/", "/key"} {
		t.Run(id, func(t *testing.T) {
			f := startFakeServer(t)
			tfresource.UnitTest(t, tfresource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []tfresource.TestStep{{
					Config:        apiKeyConfig(f, `name = "ci"`+"\n"+`role = "viewer"`),
					ResourceName:  "relay_api_key.this",
					ImportState:   true,
					ImportStateId: id,
					ExpectError:   regexp.MustCompile("Invalid import ID"),
				}},
			})
		})
	}
}

func TestAPIKeyDataSource(t *testing.T) {
	f := startFakeServer(t)
	f.workspaces["ws-1"] = &serverv1.Workspace{Id: "ws-1", Name: "prod"}
	f.apiKeys["key-1"] = &serverv1.APIKey{
		Id: "key-1", Name: "org", RoleSlug: "admin", CreatedAt: timestamppb.New(fakeNow),
		LastUsedAt: timestamppb.New(fakeNow), AllowedCidrs: []string{"10.0.0.0/8"},
	}
	f.apiKeys["key-2"] = &serverv1.APIKey{Id: "key-2", Name: "ws", WorkspaceId: "ws-1", RoleSlug: "viewer", ParentKeyId: "key-1"}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config: providerBlock(f) + `
data "relay_api_key" "by_name" {
  name = "org"
}

data "relay_api_key" "by_id" {
  id           = "key-2"
  workspace_id = "ws-1"
}
`,
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr("data.relay_api_key.by_name", "id", "key-1"),
				tfresource.TestCheckResourceAttr("data.relay_api_key.by_name", "role", "admin"),
				tfresource.TestCheckResourceAttr("data.relay_api_key.by_name", "last_used_at", "2027-01-15T08:00:00Z"),
				tfresource.TestCheckTypeSetElemAttr("data.relay_api_key.by_name", "allowed_cidrs.*", "10.0.0.0/8"),
				tfresource.TestCheckNoResourceAttr("data.relay_api_key.by_name", "expires_at"),
				tfresource.TestCheckResourceAttr("data.relay_api_key.by_id", "name", "ws"),
				tfresource.TestCheckResourceAttr("data.relay_api_key.by_id", "parent_key_id", "key-1"),
				tfresource.TestCheckNoResourceAttr("data.relay_api_key.by_id", "last_used_at"),
			),
		}},
	})
}

func TestAPIKeyDataSource_Errors(t *testing.T) {
	cases := map[string]struct {
		config string
		fail   error
		want   string
	}{
		"not found":   {`data "relay_api_key" "x" { name = "nope" }`, nil, "API key not found"},
		"ambiguous":   {`data "relay_api_key" "x" { name = "dup" }`, nil, "Ambiguous API key name"},
		"api failure": {`data "relay_api_key" "x" { id = "key-1" }`, status.Error(codes.Unavailable, "down"), "Cannot read API key"},
		"no key":      {`data "relay_api_key" "x" {}`, nil, "Exactly one of these attributes must be configured"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			f.apiKeys["key-1"] = &serverv1.APIKey{Id: "key-1", Name: "dup"}
			f.apiKeys["key-2"] = &serverv1.APIKey{Id: "key-2", Name: "dup"}
			f.failWith("ListAPIKeys", tc.fail)
			tfresource.UnitTest(t, tfresource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []tfresource.TestStep{{Config: providerBlock(f) + tc.config, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestAPIKeyModifyPlanStopsOnMalformedInput(t *testing.T) {
	ctx := context.Background()
	r := newAPIKeyResource().(*apiKeyResource)
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	var resp resource.ModifyPlanResponse
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: schemaResp.Schema, Raw: malformed},
		State: tfsdk.State{Schema: schemaResp.Schema, Raw: malformed},
	}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Error("ModifyPlan must stop on malformed input")
	}
}
