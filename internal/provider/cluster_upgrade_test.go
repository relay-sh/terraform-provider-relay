package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

type upgradableResource interface {
	resource.Resource
	resource.ResourceWithUpgradeState
}

func upgradeV0(t *testing.T, r upgradableResource, attrs map[string]any) (tfsdk.State, resource.UpgradeStateResponse) {
	t.Helper()
	ctx := context.Background()
	var current resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &current)
	if current.Schema.Version != 1 {
		t.Fatalf("schema version = %d, want 1", current.Schema.Version)
	}
	upgrader, ok := r.UpgradeState(ctx)[0]
	if !ok || upgrader.PriorSchema == nil {
		t.Fatal("version 0 needs an upgrader with its prior schema")
	}
	prior := tfsdk.State{Schema: *upgrader.PriorSchema, Raw: tftypes.NewValue(upgrader.PriorSchema.Type().TerraformType(ctx), nil)}
	for name, value := range attrs {
		if diags := prior.SetAttribute(ctx, path.Root(name), value); diags.HasError() {
			t.Fatalf("setting %s: %v", name, diags)
		}
	}
	resp := resource.UpgradeStateResponse{State: tfsdk.State{Schema: current.Schema, Raw: tftypes.NewValue(current.Schema.Type().TerraformType(ctx), nil)}}
	upgrader.StateUpgrader(ctx, resource.UpgradeStateRequest{State: &prior}, &resp)
	return resp.State, resp
}

func legacyStatus(sizes ...string) types.List {
	elem := types.ObjectType{AttrTypes: map[string]attr.Type{"node_group": types.StringType, "size": types.StringType}}
	var values []attr.Value
	for i, size := range sizes {
		values = append(values, types.ObjectValueMust(elem.AttrTypes, map[string]attr.Value{
			"node_group": types.StringValue(string(rune('a' + i))), "size": types.StringValue(size),
		}))
	}
	return types.ListValueMust(elem, values)
}

func legacyPostgreSQLGroup(role, size string, autoscale types.Object) nodeGroupModelV0 {
	return nodeGroupModelV0{
		Role: types.StringValue(role), Count: types.Int64Value(2), Cloud: types.StringValue("aws"),
		Region: types.StringValue("us-east-1"), Tier: types.StringValue("R10"),
		Resources: resourcesModelV0{
			CPU: types.StringValue("2"), Memory: types.StringValue("4Gi"), Storage: types.StringValue(size),
			IOPS: types.Int64Value(3000), Throughput: types.Int64Null(), DiskType: types.StringValue("gp3"), StorageAutoscale: autoscale,
		},
	}
}

func TestPostgreSQLClusterResource_UpgradeFromV0(t *testing.T) {
	ctx := context.Background()
	none := types.ObjectNull(storageAutoscaleAttrTypes)
	cases := map[string]struct {
		nodes     map[string]nodeGroupModelV0
		primary   types.String
		status    types.List
		wantSize  string
		wantAuto  types.Object
		wantState types.Object
	}{
		"largest size, primary autoscale, largest ceiling": {
			nodes: map[string]nodeGroupModelV0{
				"writers": legacyPostgreSQLGroup(roleElectable, "30Gi", autoscaleObject(true, 85, 25, types.StringValue("60Gi"))),
				"readers": legacyPostgreSQLGroup(roleReadOnly, "10Gi", autoscaleObject(true, 70, 10, types.StringValue("100Gi"))),
				"plain":   legacyPostgreSQLGroup(roleReadOnly, "20Gi", none),
			},
			primary:   types.StringValue("writers"),
			status:    legacyStatus("15Gi", "25Gi"),
			wantSize:  "30Gi",
			wantAuto:  autoscaleObject(true, 85, 25, types.StringValue("100Gi")),
			wantState: types.ObjectValueMust(storageStatusAttrTypes, map[string]attr.Value{"size": types.StringValue("25Gi"), "last_expanded_at": types.StringNull()}),
		},
		"primary without autoscale": {
			nodes: map[string]nodeGroupModelV0{
				"writers": legacyPostgreSQLGroup(roleElectable, "10Gi", none),
				"readers": legacyPostgreSQLGroup(roleReadOnly, "20Gi", autoscaleObject(true, 70, 10, types.StringValue("100Gi"))),
			},
			primary:   types.StringValue("writers"),
			status:    legacyStatus(),
			wantSize:  "20Gi",
			wantAuto:  none,
			wantState: types.ObjectNull(storageStatusAttrTypes),
		},
		"primary missing": {
			nodes:     map[string]nodeGroupModelV0{"writers": legacyPostgreSQLGroup(roleElectable, "10Gi", autoscaleObject(true, 70, 10, types.StringValue("100Gi")))},
			primary:   types.StringNull(),
			status:    legacyStatus(),
			wantSize:  "10Gi",
			wantAuto:  none,
			wantState: types.ObjectNull(storageStatusAttrTypes),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			state, resp := upgradeV0(t, &postgresqlClusterResource{}, map[string]any{
				"id": "db-1", "workspace_id": "ws", "name": "billing", "nodes": c.nodes,
				"primary_node_group": c.primary, "storage_status": c.status, "phase": "running",
			})
			if resp.Diagnostics.HasError() {
				t.Fatalf("upgrade failed: %v", resp.Diagnostics)
			}
			var got postgresqlClusterModel
			if diags := state.Get(ctx, &got); diags.HasError() {
				t.Fatalf("reading the upgraded state: %v", diags)
			}
			if size := storageSizeOf(got.Storage).ValueString(); size != c.wantSize {
				t.Errorf("storage.size = %q, want %q", size, c.wantSize)
			}
			if autoscale := storageAutoscaleOf(got.Storage); !autoscale.Equal(c.wantAuto) {
				t.Errorf("storage.autoscale = %v, want %v", autoscale, c.wantAuto)
			}
			if !got.StorageStatus.Equal(c.wantState) {
				t.Errorf("storage_status = %v, want %v", got.StorageStatus, c.wantState)
			}
			if got.ID.ValueString() != "db-1" || got.Phase.ValueString() != "running" || got.PrimaryNodeGroup != c.primary {
				t.Errorf("untouched attributes must carry over, got %q %q %v", got.ID.ValueString(), got.Phase.ValueString(), got.PrimaryNodeGroup)
			}
			for label, old := range c.nodes {
				group := got.Nodes[label]
				if group.Role != old.Role || group.Tier != old.Tier || group.Resources.IOPS != old.Resources.IOPS ||
					group.Resources.DiskType != old.Resources.DiskType || group.Resources.CPU != old.Resources.CPU {
					t.Errorf("group %s lost its per-group settings: %+v", label, group)
				}
			}
		})
	}
}

func TestPostgreSQLClusterResource_UpgradeFromV0RejectsAForeignState(t *testing.T) {
	ctx := context.Background()
	r := &postgresqlClusterResource{}
	var current resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &current)
	foreign := tfsdk.State{Schema: current.Schema, Raw: tftypes.NewValue(current.Schema.Type().TerraformType(ctx), nil)}
	resp := resource.UpgradeStateResponse{State: foreign}
	r.UpgradeState(ctx)[0].StateUpgrader(ctx, resource.UpgradeStateRequest{State: &foreign}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Error("a state the prior schema cannot read must fail the upgrade")
	}
}

func TestValkeyClusterResource_UpgradeFromV0(t *testing.T) {
	ctx := context.Background()
	group := func(size string, autoscale types.Object) map[string]valkeyNodeGroupModelV0 {
		return map[string]valkeyNodeGroupModelV0{"cache": {
			Count: types.Int64Value(3), Cloud: types.StringValue("aws"), Region: types.StringValue("us-east-1"), Tier: types.StringValue("RC10"),
			Resources: valkeyResourcesModelV0{CPU: types.StringValue("1"), Memory: types.StringValue("2Gi"), Storage: types.StringValue(size), StorageAutoscale: autoscale},
		}}
	}
	cases := map[string]struct {
		nodes    map[string]valkeyNodeGroupModelV0
		status   types.List
		wantAuto types.Object
	}{
		"autoscale on":  {group("6Gi", autoscaleObject(true, 85, 25, types.StringValue("40Gi"))), legacyStatus("8Gi"), autoscaleObject(true, 85, 25, types.StringValue("40Gi"))},
		"autoscale off": {group("6Gi", types.ObjectNull(storageAutoscaleAttrTypes)), legacyStatus(), types.ObjectNull(storageAutoscaleAttrTypes)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			state, resp := upgradeV0(t, &valkeyClusterResource{}, map[string]any{
				"id": "db-1", "workspace_id": "ws", "name": "sessions", "mode": "replication", "nodes": c.nodes, "storage_status": c.status,
			})
			if resp.Diagnostics.HasError() {
				t.Fatalf("upgrade failed: %v", resp.Diagnostics)
			}
			var got valkeyClusterModel
			if diags := state.Get(ctx, &got); diags.HasError() {
				t.Fatalf("reading the upgraded state: %v", diags)
			}
			if size := storageSizeOf(got.Storage).ValueString(); size != "6Gi" {
				t.Errorf("storage.size = %q, want 6Gi", size)
			}
			if autoscale := storageAutoscaleOf(got.Storage); !autoscale.Equal(c.wantAuto) {
				t.Errorf("storage.autoscale = %v, want %v", autoscale, c.wantAuto)
			}
			if wantStatus := upgradedStorageStatus(c.status); !got.StorageStatus.Equal(wantStatus) {
				t.Errorf("storage_status = %v, want %v", got.StorageStatus, wantStatus)
			}
			if g := got.Nodes["cache"]; g.Tier.ValueString() != "RC10" || g.Count.ValueInt64() != 3 || g.Resources.Memory.ValueString() != "2Gi" {
				t.Errorf("the group lost its settings: %+v", g)
			}
			if got.Mode.ValueString() != "replication" || got.ID.ValueString() != "db-1" {
				t.Errorf("untouched attributes must carry over, got %q %q", got.Mode.ValueString(), got.ID.ValueString())
			}
		})
	}
}

func TestValkeyClusterResource_UpgradeFromV0RejectsAForeignState(t *testing.T) {
	ctx := context.Background()
	r := &valkeyClusterResource{}
	var current resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &current)
	foreign := tfsdk.State{Schema: current.Schema, Raw: tftypes.NewValue(current.Schema.Type().TerraformType(ctx), nil)}
	resp := resource.UpgradeStateResponse{State: foreign}
	r.UpgradeState(ctx)[0].StateUpgrader(ctx, resource.UpgradeStateRequest{State: &foreign}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Error("a state the prior schema cannot read must fail the upgrade")
	}
}
