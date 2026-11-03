package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const onlyReadonly = "  default_connections = { readonly = true, readwrite = false, admin = false }\n"

func sentDefaultConnections(f *fakeServer, want *serverv1.DefaultConnectionsIntent) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := f.lastDBCreate.GetDefaultConnections(); !proto.Equal(got, want) {
			return fmt.Errorf("CreateDatabase default_connections = %v, want %v", got, want)
		}
		return nil
	}
}

func setDefaultConnectionsTo(f *fakeServer, want *serverv1.DefaultConnectionsIntent) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := f.lastDefaultConns.GetDefaultConnections(); !proto.Equal(got, want) {
			return fmt.Errorf("SetDefaultConnections = %v, want %v", got, want)
		}
		return nil
	}
}

func defaultConnectionsAre(addr, readonly, readwrite, admin string) resource.TestCheckFunc {
	return resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr(addr, "default_connections.readonly", readonly),
		resource.TestCheckResourceAttr(addr, "default_connections.readwrite", readwrite),
		resource.TestCheckResourceAttr(addr, "default_connections.admin", admin),
	)
}

func defaultConnectionsSteps(f *fakeServer, addr string, config func(extra string) string) []resource.TestStep {
	update := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)}}
	empty := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}
	return []resource.TestStep{
		{
			Config: config(""),
			Check: resource.ComposeAggregateTestCheckFunc(
				sentDefaultConnections(f, &serverv1.DefaultConnectionsIntent{Readonly: true, Readwrite: true, Admin: true}),
				defaultConnectionsAre(addr, "true", "true", "true"),
			),
		},
		{
			Config:           config(onlyReadonly),
			ConfigPlanChecks: update,
			Check: resource.ComposeAggregateTestCheckFunc(
				setDefaultConnectionsTo(f, &serverv1.DefaultConnectionsIntent{Readonly: true}),
				defaultConnectionsAre(addr, "true", "false", "false"),
				callCount(f, "CreateDatabase", 1),
				callCount(f, "UpdateDatabase", 0),
			),
		},
		{
			Config:           config(""),
			ConfigPlanChecks: empty,
		},
		{
			Config:           config("  default_connections = { admin = true }\n"),
			ConfigPlanChecks: update,
			Check: resource.ComposeAggregateTestCheckFunc(
				setDefaultConnectionsTo(f, &serverv1.DefaultConnectionsIntent{Readonly: true, Admin: true}),
				defaultConnectionsAre(addr, "true", "false", "true"),
			),
		},
		{
			PreConfig: func() {
				f.set(func() {
					for _, db := range f.databases {
						db.DefaultConnections = &serverv1.DefaultConnectionsIntent{}
					}
				})
			},
			Config:           config("  default_connections = { admin = true }\n"),
			ConfigPlanChecks: update,
			Check:            setDefaultConnectionsTo(f, &serverv1.DefaultConnectionsIntent{Admin: true}),
		},
		{
			PreConfig: func() {
				f.failWith("SetDefaultConnections", status.Error(codes.FailedPrecondition, "Changing default connections is not available"))
			},
			Config:      config(onlyReadonly),
			ExpectError: regexp.MustCompile("Cannot change default connections"),
		},
		{
			PreConfig: func() { f.failWith("SetDefaultConnections", nil) },
			Config:    config(onlyReadonly),
			Check:     defaultConnectionsAre(addr, "true", "false", "false"),
		},
		{
			ResourceName: addr,
			ImportState:  true,
			ImportStateIdFunc: func(s *terraform.State) (string, error) {
				return dbWorkspace + "/" + s.RootModule().Resources[addr].Primary.ID, nil
			},
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"keep_backups_on_destroy", "timeouts", "nodes", "parameters"},
		},
	}
}

func TestPostgreSQLClusterResource_DefaultConnections(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: defaultConnectionsSteps(f, "relay_postgresql_cluster.this", func(extra string) string {
			return clusterConfig(f, "18", "10Gi", `{}`, extra)
		}),
	})
}

func TestValkeyClusterResource_DefaultConnections(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: defaultConnectionsSteps(f, valkeyAddr, func(extra string) string {
			return replicationConfig(f, "9.1", "8Gi", `{}`, extra)
		}),
	})
}

func TestDefaultConnectionsOnARestore(t *testing.T) {
	cases := map[string]struct {
		config func(f *fakeServer, extra string) string
		addr   string
	}{
		"postgresql": {func(f *fakeServer, extra string) string {
			return postgresRestoreConfig(f, restoreBlock("db-src", ""), extra, 1)
		}, postgresAddr},
		"valkey": {func(f *fakeServer, extra string) string {
			return valkeyConfig(f, restoreBlock("db-vk", "")+extra+valkeyGroup("cache", 2, "8Gi", ""))
		}, valkeyAddr},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for _, extra := range []string{"", onlyReadonly} {
				f := startFakeServer(t)
				seedSources(f, postgresSource("db-src", "18"), valkeySource("db-vk", "9.1", valkeyModeReplication, "cache", true))
				check := resource.ComposeAggregateTestCheckFunc(callCount(f, "SetDefaultConnections", 0), resource.TestCheckNoResourceAttr(c.addr, "default_connections.%"))
				if extra != "" {
					check = resource.ComposeAggregateTestCheckFunc(setDefaultConnectionsTo(f, &serverv1.DefaultConnectionsIntent{Readonly: true}), defaultConnectionsAre(c.addr, "true", "false", "false"))
				}
				resource.UnitTest(t, resource.TestCase{
					ProtoV6ProviderFactories: factories(relay.Dial),
					Steps:                    []resource.TestStep{{Config: c.config(f, extra), Check: check}},
				})
			}
		})
	}
}

func TestDefaultConnectionsOnARestoreFails(t *testing.T) {
	configs := map[string]func(f *fakeServer) string{
		"postgresql": func(f *fakeServer) string {
			return postgresRestoreConfig(f, restoreBlock("db-src", ""), onlyReadonly, 1)
		},
		"valkey": func(f *fakeServer) string {
			return valkeyConfig(f, restoreBlock("db-vk", "")+onlyReadonly+valkeyGroup("cache", 2, "8Gi", ""))
		},
	}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			seedSources(f, postgresSource("db-src", "18"), valkeySource("db-vk", "9.1", valkeyModeReplication, "cache", true))
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					PreConfig:   func() { f.failWith("SetDefaultConnections", status.Error(codes.FailedPrecondition, "not available")) },
					Config:      config(f),
					ExpectError: regexp.MustCompile("Cannot set default connections on the restored cluster"),
				}},
			})
		})
	}
}

func TestDefaultConnectionsPlan(t *testing.T) {
	ctx := context.Background()
	objType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"default_connections": tftypes.Object{AttributeTypes: map[string]tftypes.Type{"readonly": tftypes.Bool, "readwrite": tftypes.Bool, "admin": tftypes.Bool}},
		"restore":             tftypes.Object{AttributeTypes: map[string]tftypes.Type{"source_database_id": tftypes.String}},
	}}
	resourceValue := tftypes.NewValue(objType, map[string]tftypes.Value{
		"default_connections": tftypes.NewValue(objType.AttributeTypes["default_connections"], nil),
		"restore":             tftypes.NewValue(objType.AttributeTypes["restore"], nil),
	})
	none := tftypes.NewValue(objType, nil)
	partial := func(admin types.Bool) types.Object {
		return types.ObjectValueMust(defaultConnectionsAttrTypes, map[string]attr.Value{"readonly": types.BoolNull(), "readwrite": types.BoolValue(false), "admin": admin})
	}
	cases := map[string]struct {
		plan, state   tftypes.Value
		config, prior types.Object
		want          types.Object
		warnings      int
	}{
		"partial on create":       {resourceValue, none, partial(types.BoolValue(true)), types.ObjectNull(defaultConnectionsAttrTypes), defaultConnectionsValue(true, false, true), 0},
		"partial keeps the state": {resourceValue, resourceValue, partial(types.BoolNull()), defaultConnectionsValue(false, true, true), defaultConnectionsValue(false, false, true), 1},
		"unknown role":            {resourceValue, resourceValue, partial(types.BoolUnknown()), defaultConnectionsValue(true, false, true), types.ObjectValueMust(defaultConnectionsAttrTypes, map[string]attr.Value{"readonly": types.BoolValue(true), "readwrite": types.BoolValue(false), "admin": types.BoolUnknown()}), 0},
		"unknown config":          {resourceValue, resourceValue, types.ObjectUnknown(defaultConnectionsAttrTypes), defaultConnectionsValue(true, true, true), types.ObjectUnknown(defaultConnectionsAttrTypes), 0},
		"destroy":                 {none, resourceValue, types.ObjectNull(defaultConnectionsAttrTypes), defaultConnectionsValue(true, true, true), types.ObjectNull(defaultConnectionsAttrTypes), 0},
	}
	for name, c := range cases {
		req := planmodifier.ObjectRequest{
			Path: path.Root("default_connections"), ConfigValue: c.config, StateValue: c.prior, PlanValue: c.config,
			Plan: tfsdk.Plan{Raw: c.plan}, State: tfsdk.State{Raw: c.state},
		}
		resp := planmodifier.ObjectResponse{PlanValue: c.config}
		defaultConnectionsPlan{}.PlanModifyObject(ctx, req, &resp)
		if !resp.PlanValue.Equal(c.want) || resp.Diagnostics.WarningsCount() != c.warnings || resp.Diagnostics.HasError() {
			t.Errorf("%s: plan %v, %d warnings, errors %v; want %v, %d warnings", name, resp.PlanValue, resp.Diagnostics.WarningsCount(), resp.Diagnostics.Errors(), c.want, c.warnings)
		}
	}
	if (defaultConnectionsPlan{}).MarkdownDescription(ctx) == "" {
		t.Error("the plan modifier needs a description")
	}
	if got := defaultConnectionsFromProto(nil, types.ObjectUnknown(defaultConnectionsAttrTypes)); !got.IsNull() {
		t.Errorf("an unknown prior with nothing from the API = %v, want null", got)
	}
	if defaultConnectionsToProto(types.ObjectUnknown(defaultConnectionsAttrTypes)) != nil {
		t.Error("an unknown value must not be sent")
	}
}
