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

func defaultConnectionsSteps(f *fakeServer, addr string, config func(extra string) string) []resource.TestStep {
	return []resource.TestStep{
		{
			Config: config(""),
			Check: resource.ComposeAggregateTestCheckFunc(
				sentDefaultConnections(f, &serverv1.DefaultConnectionsIntent{Readonly: true, Readwrite: true, Admin: true}),
				resource.TestCheckNoResourceAttr(addr, "default_connections.%"),
			),
		},
		{
			Config:           config(onlyReadonly),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(addr, "default_connections.readonly", "true"),
				resource.TestCheckResourceAttr(addr, "default_connections.admin", "false"),
				callCount(f, "CreateDatabase", 1),
				callCount(f, "UpdateDatabase", 0),
			),
		},
		{
			Config:  config(onlyReadonly),
			Destroy: true,
		},
		{
			Config: config(onlyReadonly),
			Check: resource.ComposeAggregateTestCheckFunc(
				sentDefaultConnections(f, &serverv1.DefaultConnectionsIntent{Readonly: true}),
				resource.TestCheckResourceAttr(addr, "default_connections.readwrite", "false"),
			),
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

func TestDefaultConnectionsRejectedWithRestore(t *testing.T) {
	f := startFakeServer(t)
	seedSources(f, postgresSource("db-src", "18"), valkeySource("db-vk", "9.1", valkeyModeReplication, "cache", true))
	for name, config := range map[string]string{
		"postgresql": postgresRestoreConfig(f, restoreBlock("db-src", ""), onlyReadonly, 1),
		"valkey":     valkeyConfig(f, restoreBlock("db-vk", "")+onlyReadonly+valkeyGroup("cache", 2, "8Gi", "")),
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: config, ExpectError: regexp.MustCompile("Not available with restore")}},
			})
		})
	}
}

func TestDefaultConnectionsPlanWarnsOnAnExistingCluster(t *testing.T) {
	objType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"default_connections": tftypes.Object{AttributeTypes: map[string]tftypes.Type{"readonly": tftypes.Bool, "readwrite": tftypes.Bool, "admin": tftypes.Bool}},
		"restore":             tftypes.Object{AttributeTypes: map[string]tftypes.Type{"source_database_id": tftypes.String}},
	}}
	existing := tftypes.NewValue(objType, map[string]tftypes.Value{
		"default_connections": tftypes.NewValue(objType.AttributeTypes["default_connections"], nil),
		"restore":             tftypes.NewValue(objType.AttributeTypes["restore"], nil),
	})
	set := types.ObjectValueMust(map[string]attr.Type{"readonly": types.BoolType, "readwrite": types.BoolType, "admin": types.BoolType},
		map[string]attr.Value{"readonly": types.BoolValue(true), "readwrite": types.BoolValue(true), "admin": types.BoolValue(true)})
	null := types.ObjectNull(set.AttributeTypes(context.Background()))
	cases := map[string]struct {
		plan, state tftypes.Value
		config      types.Object
		warnings    int
	}{
		"set on an existing cluster":   {existing, existing, set, 1},
		"unchanged on an existing one": {existing, existing, null, 0},
		"destroy":                      {tftypes.NewValue(objType, nil), existing, null, 0},
	}
	for name, c := range cases {
		req := planmodifier.ObjectRequest{
			Path: path.Root("default_connections"), ConfigValue: c.config, StateValue: null, PlanValue: c.config,
			Plan:  tfsdk.Plan{Raw: c.plan},
			State: tfsdk.State{Raw: c.state},
		}
		var resp planmodifier.ObjectResponse
		defaultConnectionsPlan{}.PlanModifyObject(context.Background(), req, &resp)
		if got := resp.Diagnostics.WarningsCount(); got != c.warnings || resp.Diagnostics.HasError() {
			t.Errorf("%s: %d warnings, errors %v, want %d warnings", name, got, resp.Diagnostics.Errors(), c.warnings)
		}
	}
	if d := (defaultConnectionsPlan{}).MarkdownDescription(context.Background()); d == "" {
		t.Error("the plan modifier needs a description")
	}
}
