package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type valkeyClusterModelV0 struct {
	ID                   types.String                      `tfsdk:"id"`
	WorkspaceID          types.String                      `tfsdk:"workspace_id"`
	Name                 types.String                      `tfsdk:"name"`
	Version              types.String                      `tfsdk:"version"`
	Mode                 types.String                      `tfsdk:"mode"`
	Shards               types.Int64                       `tfsdk:"shards"`
	ReplicasPerShard     types.Int64                       `tfsdk:"replicas_per_shard"`
	Nodes                map[string]valkeyNodeGroupModelV0 `tfsdk:"nodes"`
	Parameters           types.Map                         `tfsdk:"parameters"`
	KeepBackupsOnDestroy types.Bool                        `tfsdk:"keep_backups_on_destroy"`
	DeletionProtection   types.Bool                        `tfsdk:"deletion_protection"`
	Restore              types.Object                      `tfsdk:"restore"`
	DefaultConnections   types.Object                      `tfsdk:"default_connections"`
	Phase                types.String                      `tfsdk:"phase"`
	PhaseMessage         types.String                      `tfsdk:"phase_message"`
	EndpointHost         types.String                      `tfsdk:"endpoint_host"`
	EndpointPort         types.Int64                       `tfsdk:"endpoint_port"`
	PublicEndpoints      types.List                        `tfsdk:"public_endpoints"`
	PrivateEndpoints     types.List                        `tfsdk:"private_endpoints"`
	StorageStatus        types.List                        `tfsdk:"storage_status"`
	ReadyNodes           types.Int64                       `tfsdk:"ready_nodes"`
	TotalNodes           types.Int64                       `tfsdk:"total_nodes"`
	CreatedAt            types.String                      `tfsdk:"created_at"`
	UpdatedAt            types.String                      `tfsdk:"updated_at"`
	Timeouts             timeouts.Value                    `tfsdk:"timeouts"`
}

type valkeyNodeGroupModelV0 struct {
	Count     types.Int64            `tfsdk:"count"`
	Cloud     types.String           `tfsdk:"cloud"`
	Region    types.String           `tfsdk:"region"`
	Tier      types.String           `tfsdk:"tier"`
	Resources valkeyResourcesModelV0 `tfsdk:"resources"`
}

type valkeyResourcesModelV0 struct {
	CPU              types.String `tfsdk:"cpu"`
	Memory           types.String `tfsdk:"memory"`
	Storage          types.String `tfsdk:"storage"`
	StorageAutoscale types.Object `tfsdk:"storage_autoscale"`
}

func (r *valkeyClusterResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	prior := valkeyClusterSchemaV0(ctx)
	return map[int64]resource.StateUpgrader{0: {PriorSchema: &prior, StateUpgrader: upgradeValkeyClusterV0}}
}

func upgradeValkeyClusterV0(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
	var old valkeyClusterModelV0
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var sizes, ceilings []types.String
	primary := types.ObjectNull(storageAutoscaleAttrTypes)
	nodes := make(map[string]valkeyNodeGroupModel, len(old.Nodes))
	for name, g := range old.Nodes {
		sizes = append(sizes, g.Resources.Storage)
		ceilings = append(ceilings, maxStorageOf(g.Resources.StorageAutoscale))
		primary = g.Resources.StorageAutoscale
		nodes[name] = valkeyNodeGroupModel{
			Count: g.Count, Cloud: g.Cloud, Region: g.Region, Tier: g.Tier,
			Resources: valkeyResourcesModel{CPU: g.Resources.CPU, Memory: g.Resources.Memory},
		}
	}
	upgraded := valkeyClusterModel{
		ID: old.ID, WorkspaceID: old.WorkspaceID, Name: old.Name, Version: old.Version, Mode: old.Mode,
		Shards: old.Shards, ReplicasPerShard: old.ReplicasPerShard,
		Storage: upgradedStorage(sizes, primary, ceilings), Nodes: nodes, Parameters: old.Parameters,
		KeepBackupsOnDestroy: old.KeepBackupsOnDestroy, DeletionProtection: old.DeletionProtection,
		Restore: old.Restore, DefaultConnections: old.DefaultConnections, Phase: old.Phase, PhaseMessage: old.PhaseMessage,
		EndpointHost: old.EndpointHost, EndpointPort: old.EndpointPort, PublicEndpoints: old.PublicEndpoints,
		PrivateEndpoints: old.PrivateEndpoints, StorageStatus: upgradedStorageStatus(old.StorageStatus),
		ReadyNodes: old.ReadyNodes, TotalNodes: old.TotalNodes, CreatedAt: old.CreatedAt, UpdatedAt: old.UpdatedAt,
		Timeouts: old.Timeouts,
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &upgraded)...)
}

func valkeyClusterSchemaV0(ctx context.Context) schema.Schema {
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}
	return schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id":                 schema.StringAttribute{Computed: true},
			"workspace_id":       schema.StringAttribute{Required: true},
			"name":               schema.StringAttribute{Required: true},
			"version":            schema.StringAttribute{Optional: true, Computed: true},
			"mode":               schema.StringAttribute{Optional: true, Computed: true},
			"shards":             schema.Int64Attribute{Optional: true},
			"replicas_per_shard": schema.Int64Attribute{Optional: true},
			"nodes": schema.MapNestedAttribute{
				Required: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"count":  schema.Int64Attribute{Optional: true, Computed: true},
					"cloud":  schema.StringAttribute{Required: true},
					"region": schema.StringAttribute{Required: true},
					"tier":   schema.StringAttribute{Required: true},
					"resources": schema.SingleNestedAttribute{
						Required: true,
						Attributes: map[string]schema.Attribute{
							"cpu":     schema.StringAttribute{Computed: true},
							"memory":  schema.StringAttribute{Computed: true},
							"storage": schema.StringAttribute{Optional: true, Computed: true},
							"storage_autoscale": schema.SingleNestedAttribute{
								Optional: true,
								Attributes: map[string]schema.Attribute{
									"enabled":           schema.BoolAttribute{Required: true},
									"threshold_percent": schema.Int64Attribute{Optional: true, Computed: true},
									"increase_percent":  schema.Int64Attribute{Optional: true, Computed: true},
									"max_storage":       schema.StringAttribute{Optional: true},
								},
							},
						},
					},
				}},
			},
			"parameters":              schema.MapAttribute{ElementType: types.StringType, Optional: true, Computed: true},
			"keep_backups_on_destroy": schema.BoolAttribute{Optional: true, Computed: true},
			"deletion_protection":     schema.BoolAttribute{Optional: true, Computed: true},
			"restore":                 restoreAttribute("", ""),
			"default_connections":     defaultConnectionsAttribute(),
			"phase":                   schema.StringAttribute{Computed: true},
			"phase_message":           schema.StringAttribute{Computed: true},
			"endpoint_host":           schema.StringAttribute{Computed: true},
			"endpoint_port":           schema.Int64Attribute{Computed: true},
			"public_endpoints":        schema.ListNestedAttribute{Computed: true, NestedObject: endpoint},
			"private_endpoints":       schema.ListNestedAttribute{Computed: true, NestedObject: endpoint},
			"storage_status": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"node_group": schema.StringAttribute{Computed: true},
					"size":       schema.StringAttribute{Computed: true},
				}},
			},
			"ready_nodes": schema.Int64Attribute{Computed: true},
			"total_nodes": schema.Int64Attribute{Computed: true},
			"created_at":  schema.StringAttribute{Computed: true},
			"updated_at":  schema.StringAttribute{Computed: true},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}
