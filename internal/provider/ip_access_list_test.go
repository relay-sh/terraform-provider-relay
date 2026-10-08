package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const netWorkspace = "ws-net"

func ipAccessListConfig(f *fakeServer, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_ip_access_list" "this" {
  workspace_id = %q
%s
}
`, netWorkspace, body)
}

func noIPAccessListLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if n := len(f.allowlist[netWorkspace]); n != 0 {
			return fmt.Errorf("%d entries left after destroy", n)
		}
		return nil
	}
}

func seedIPAccessListEntry(f *fakeServer, e *serverv1.IPAllowlistEntry) {
	f.set(func() { f.allowlist[netWorkspace] = append(f.allowlist[netWorkspace], e) })
}

func TestIPAccessListResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	f.syncingFor = 2
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noIPAccessListLeft(f),
		Steps: []resource.TestStep{
			{
				Config: ipAccessListConfig(f, `
  cidr        = "203.0.113.0/24"
  description = "office"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "id", "ip-1"),
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "cidr", "203.0.113.0/24"),
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "description", "office"),
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "created_by", "key-1"),
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "created_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						calls := f.callsTo("AddIPAllowlistEntry")
						if len(calls) != 1 || calls[0].workspace != netWorkspace || calls[0].org != testOrgID {
							return fmt.Errorf("add must be scoped to org and workspace: %+v", calls)
						}
						if f.syncCountdown[netWorkspace] != 0 {
							return errors.New("create must wait until the change is synced")
						}
						return nil
					},
				),
			},
			{
				ResourceName:      "relay_ip_access_list.this",
				ImportState:       true,
				ImportStateId:     netWorkspace + "/203.0.113.0/24",
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"timeouts",
				},
			},
			{
				Config: ipAccessListConfig(f, `
  cidr        = "198.51.100.7/32"
  description = "office"
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "id", "ip-1"),
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "cidr", "198.51.100.7/32"),
					func(*terraform.State) error {
						u := f.lastAllowUpdate
						if u.GetCidr() != "198.51.100.7/32" || u.Description != nil {
							return fmt.Errorf("update must send only what changed: %+v", u)
						}
						if n := len(f.callsTo("AddIPAllowlistEntry")); n != 1 {
							return fmt.Errorf("a CIDR change must update in place, got %d adds", n)
						}
						return nil
					},
				),
			},
			{
				Config: ipAccessListConfig(f, `  cidr = "198.51.100.7/32"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_ip_access_list.this", "description", ""),
					func(*terraform.State) error {
						u := f.lastAllowUpdate
						if u.Cidr != nil || u.Description == nil || u.GetDescription() != "" {
							return fmt.Errorf("removing the description must clear it alone: %+v", u)
						}
						return nil
					},
				),
			},
			{
				Config: ipAccessListConfig(f, `
  cidr = "198.51.100.7/32"
  timeouts {
    delete = "1m"
  }
`),
				Check: func(*terraform.State) error {
					if n := len(f.callsTo("UpdateIPAllowlistEntry")); n != 2 {
						return fmt.Errorf("a timeouts-only change must not call the API, got %d updates", n)
					}
					return nil
				},
			},
			{
				Config: ipAccessListConfig(f, `  cidr = "2001:db8::/48"`) + fmt.Sprintf(`
data "relay_ip_access_list" "by_cidr" {
  workspace_id = %[1]q
  cidr         = relay_ip_access_list.this.cidr
}

data "relay_ip_access_list" "by_id" {
  workspace_id = %[1]q
  id           = relay_ip_access_list.this.id
}
`, netWorkspace),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.relay_ip_access_list.by_cidr", "id", "ip-1"),
					resource.TestCheckResourceAttr("data.relay_ip_access_list.by_id", "cidr", "2001:db8::/48"),
					resource.TestCheckResourceAttr("data.relay_ip_access_list.by_id", "created_by", "key-1"),
				),
			},
		},
	})
}

func TestIPAccessListResource_RejectsWhatTheAPIWouldRewrite(t *testing.T) {
	f := startFakeServer(t)
	cases := map[string]string{
		`  cidr = "203.0.113.7"`:                                   `Invalid CIDR`,
		`  cidr = "203.0.113.7/24"`:                                `Write\s+"203\.0\.113\.0/24"`,
		`  cidr = "::ffff:203.0.113.0/120"`:                        `Write\s+"203\.0\.113\.0/24"`,
		"  cidr = \"203.0.113.0/24\"\n  description = \" office\"": `Untrimmed value`,
	}
	for body, want := range cases {
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: factories(relay.Dial),
			Steps: []resource.TestStep{{
				Config:      ipAccessListConfig(f, body),
				ExpectError: regexp.MustCompile(want),
			}},
		})
	}
	if n := len(f.callsTo("AddIPAllowlistEntry")); n != 0 {
		t.Errorf("invalid input must fail before the API, got %d adds", n)
	}
}

func TestIPAccessListResource_DriftAndFailures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	denied := status.Error(codes.PermissionDenied, "denied")
	notFound := status.Error(codes.NotFound, "gone")
	stuck := func(on bool) func() {
		return func() {
			f.set(func() {
				f.syncingFor = 0
				if on {
					f.syncingFor = 1 << 20
				}
				f.syncCountdown = map[string]int{}
			})
		}
	}
	first := `  cidr = "203.0.113.0/24"`
	second := `  cidr = "198.51.100.0/24"`
	withTimeout := func(body, op string) string {
		return body + "\n  timeouts {\n    " + op + " = \"1ms\"\n  }"
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				PreConfig:   func() { f.failWith("AddIPAllowlistEntry", denied) },
				Config:      ipAccessListConfig(f, first),
				ExpectError: regexp.MustCompile("Cannot add IP access list entry"),
			},
			{
				PreConfig: func() {
					f.failWith("AddIPAllowlistEntry", nil)
					stuck(true)()
				},
				Config:      ipAccessListConfig(f, withTimeout(first, "create")),
				ExpectError: regexp.MustCompile("did not reach the edge"),
			},
			{
				PreConfig: stuck(false),
				Config:    ipAccessListConfig(f, first),
				Check:     resource.TestCheckResourceAttr("relay_ip_access_list.this", "id", "ip-2"),
			},
			{
				PreConfig:   func() { f.failWith("ListIPAllowlist", unavailable) },
				Config:      ipAccessListConfig(f, first),
				ExpectError: regexp.MustCompile("Cannot read IP access list"),
			},
			{
				PreConfig: func() {
					f.failWith("ListIPAllowlist", nil)
					f.failWith("UpdateIPAllowlistEntry", denied)
				},
				Config:      ipAccessListConfig(f, second),
				ExpectError: regexp.MustCompile("Cannot update IP access list entry"),
			},
			{
				PreConfig: func() {
					f.failWith("UpdateIPAllowlistEntry", nil)
					stuck(true)()
				},
				Config:      ipAccessListConfig(f, withTimeout(second, "update")),
				ExpectError: regexp.MustCompile("did not reach the edge"),
			},
			{
				PreConfig: func() {
					stuck(false)()
					f.set(func() { f.allowlist[netWorkspace] = nil })
				},
				Config:             ipAccessListConfig(f, second),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig:   func() { f.failWith("ListIPAllowlist", notFound) },
				Config:      ipAccessListConfig(f, second),
				ExpectError: regexp.MustCompile("did not reach the edge"),
			},
			{
				PreConfig: func() { f.failWith("ListIPAllowlist", nil) },
				Config:    ipAccessListConfig(f, withTimeout(second, "delete")),
			},
			{
				PreConfig:   func() { f.failWith("RemoveIPAllowlistEntry", denied) },
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot remove IP access list entry"),
			},
			{
				PreConfig: func() {
					f.failWith("RemoveIPAllowlistEntry", nil)
					stuck(true)()
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("did not reach the edge"),
			},
			{
				PreConfig: stuck(false),
				Config:    ipAccessListConfig(f, second),
			},
			{
				PreConfig: func() { f.failWith("RemoveIPAllowlistEntry", notFound) },
				Config:    providerBlock(f),
			},
		},
	})
}

func TestIPAccessListResource_Import(t *testing.T) {
	f := startFakeServer(t)
	seedIPAccessListEntry(f, &serverv1.IPAllowlistEntry{Id: "ip-perm", Cidr: "203.0.113.0/24", Description: "dashboard", CreatedAt: timestamppb.New(fakeEpoch)})
	seedIPAccessListEntry(f, &serverv1.IPAllowlistEntry{Id: "ip-temp", Cidr: "198.51.100.7/32", ExpiresAt: timestamppb.New(fakeEpoch)})
	config := ipAccessListConfig(f, `
  cidr        = "203.0.113.0/24"
  description = "dashboard"
`)
	importStep := func(id, want string) resource.TestStep {
		return resource.TestStep{
			Config:        config,
			ResourceName:  "relay_ip_access_list.this",
			ImportState:   true,
			ImportStateId: id,
			ExpectError:   regexp.MustCompile(want),
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			importStep("no-slash", "Invalid import ID"),
			importStep(netWorkspace+"/203.0.113.7/24", "no entry for 203.0.113.7/24"),
			importStep(netWorkspace+"/198.51.100.7/32", "Temporary entry"),
			{
				PreConfig: func() { f.failWith("ListIPAllowlist", status.Error(codes.PermissionDenied, "denied")) },
				Config:    config, ResourceName: "relay_ip_access_list.this", ImportState: true,
				ImportStateId: netWorkspace + "/203.0.113.0/24",
				ExpectError:   regexp.MustCompile("Cannot read IP access list"),
			},
			{
				PreConfig:          func() { f.failWith("ListIPAllowlist", nil) },
				Config:             config,
				ResourceName:       "relay_ip_access_list.this",
				ImportState:        true,
				ImportStateId:      netWorkspace + "/203.0.113.0/24",
				ImportStatePersist: true,
				ImportStateCheck: func(s []*terraform.InstanceState) error {
					if len(s) != 1 || s[0].ID != "ip-perm" || s[0].Attributes["description"] != "dashboard" {
						return fmt.Errorf("imported %+v", s)
					}
					return nil
				},
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestIPAccessListDataSource_Failures(t *testing.T) {
	f := startFakeServer(t)
	ds := func(body string) string {
		return providerBlock(f) + fmt.Sprintf(`
data "relay_ip_access_list" "this" {
  workspace_id = %q
%s
}
`, netWorkspace, body)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config:      ds("  id = \"ip-1\"\n  cidr = \"203.0.113.0/24\""),
				ExpectError: regexp.MustCompile("Invalid Attribute Combination"),
			},
			{
				Config:      ds(`  cidr = "203.0.113.0/24"`),
				ExpectError: regexp.MustCompile("IP access list entry not found"),
			},
			{
				PreConfig:   func() { f.failWith("ListIPAllowlist", status.Error(codes.PermissionDenied, "denied")) },
				Config:      ds(`  id = "ip-1"`),
				ExpectError: regexp.MustCompile("Cannot read IP access list"),
			},
		},
	})
}

func TestIPAccessListValidatorsSkipUnknownValues(t *testing.T) {
	ctx := context.Background()
	for _, v := range []validator.String{canonicalCIDR{}, trimmed{}} {
		if v.MarkdownDescription(ctx) != v.Description(ctx) {
			t.Errorf("%T descriptions differ", v)
		}
		for _, value := range []types.String{types.StringNull(), types.StringUnknown()} {
			var resp validator.StringResponse
			v.ValidateString(ctx, validator.StringRequest{Path: path.Root("x"), ConfigValue: value}, &resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("%T must skip %s", v, value)
			}
		}
	}
}
