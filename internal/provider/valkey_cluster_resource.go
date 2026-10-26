package provider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const (
	engineValkey          = "valkey"
	valkeyModeReplication = "replication"
	valkeyModeCluster     = "cluster"
)

type valkeyClusterResource struct {
	client *client.Client
}

type valkeyNodeGroupModel struct {
	Count     types.Int64          `tfsdk:"count"`
	Cloud     types.String         `tfsdk:"cloud"`
	Region    types.String         `tfsdk:"region"`
	Tier      types.String         `tfsdk:"tier"`
	Resources valkeyResourcesModel `tfsdk:"resources"`
}

type valkeyResourcesModel struct {
	CPU              types.String `tfsdk:"cpu"`
	Memory           types.String `tfsdk:"memory"`
	Storage          types.String `tfsdk:"storage"`
	StorageAutoscale types.Object `tfsdk:"storage_autoscale"`
}

type valkeyClusterModel struct {
	ID                   types.String                    `tfsdk:"id"`
	WorkspaceID          types.String                    `tfsdk:"workspace_id"`
	Name                 types.String                    `tfsdk:"name"`
	Version              types.String                    `tfsdk:"version"`
	Mode                 types.String                    `tfsdk:"mode"`
	Shards               types.Int64                     `tfsdk:"shards"`
	ReplicasPerShard     types.Int64                     `tfsdk:"replicas_per_shard"`
	Nodes                map[string]valkeyNodeGroupModel `tfsdk:"nodes"`
	Parameters           types.Map                       `tfsdk:"parameters"`
	KeepBackupsOnDestroy types.Bool                      `tfsdk:"keep_backups_on_destroy"`
	Phase                types.String                    `tfsdk:"phase"`
	PhaseMessage         types.String                    `tfsdk:"phase_message"`
	EndpointHost         types.String                    `tfsdk:"endpoint_host"`
	EndpointPort         types.Int64                     `tfsdk:"endpoint_port"`
	PublicEndpoints      types.List                      `tfsdk:"public_endpoints"`
	PrivateEndpoints     types.List                      `tfsdk:"private_endpoints"`
	StorageStatus        types.List                      `tfsdk:"storage_status"`
	ReadyNodes           types.Int64                     `tfsdk:"ready_nodes"`
	TotalNodes           types.Int64                     `tfsdk:"total_nodes"`
	CreatedAt            types.String                    `tfsdk:"created_at"`
	UpdatedAt            types.String                    `tfsdk:"updated_at"`
	Timeouts             timeouts.Value                  `tfsdk:"timeouts"`
}

var valkeyGroupName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func newValkeyClusterResource() resource.Resource {
	return &valkeyClusterResource{}
}

func (r *valkeyClusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_valkey_cluster"
}

func (r *valkeyClusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keepString := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	keepInt := []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed Valkey cluster. Creating, updating and destroying wait for the cluster to settle. " +
			"Changing `name`, `workspace_id` or `mode` replaces the cluster and its data; use `lifecycle { prevent_destroy = true }`.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "Cluster ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the cluster belongs to."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Cluster name, unique in the workspace."},
			"version": schema.StringAttribute{
				Optional: true, Computed: true, PlanModifiers: keepString,
				MarkdownDescription: "Valkey version, for example `9.1`. Omit it and the platform picks one, which the next read reports. " +
					"It cannot change here: upgrades go through Relay's upgrade flow, and a plan that changes it fails.",
			},
			"mode": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(valkeyModeReplication),
				PlanModifiers: replace,
				Validators:    []validator.String{stringvalidator.OneOf(valkeyModeReplication, valkeyModeCluster)},
				MarkdownDescription: "`replication` runs one primary and its replicas. `cluster` shards the keyspace across `shards` primaries, " +
					"each with `replicas_per_shard` replicas. Changing it replaces the cluster.",
			},
			"shards": schema.Int64Attribute{
				Optional:   true,
				Validators: []validator.Int64{int64validator.Between(3, 100)},
				MarkdownDescription: "Primaries the keyspace is split across, 3-100. Required in `cluster` mode and not allowed in `replication` mode. " +
					"Changing it reshards in place, together with the node group's `count`.",
			},
			"replicas_per_shard": schema.Int64Attribute{
				Optional:            true,
				Validators:          []validator.Int64{int64validator.Between(0, 4)},
				MarkdownDescription: "Replicas behind each shard's primary, 0-4. Only in `cluster` mode; omitted means 0.",
			},
			"nodes": schema.MapNestedAttribute{
				Required: true,
				Validators: []validator.Map{
					mapvalidator.SizeBetween(1, 1),
					mapvalidator.KeysAre(stringvalidator.LengthAtMost(30), stringvalidator.RegexMatches(valkeyGroupName, "must be lowercase letters, digits and hyphens")),
				},
				MarkdownDescription: "The cluster's single node group, keyed by a label you choose. Renaming it replaces the cluster. " +
					"In `cluster` mode `count` must equal `shards * (1 + replicas_per_shard)`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"count": schema.Int64Attribute{
						Optional: true, Computed: true, Default: int64default.StaticInt64(1),
						Validators:          []validator.Int64{int64validator.Between(1, 500)},
						MarkdownDescription: "Nodes in the group. In `replication` mode, one primary and `count - 1` replicas.",
					},
					"cloud": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Cloud the group runs in: `aws`, `gcp` or `azure`.",
					},
					"region": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Region the group runs in, for example `us-east-1`.",
					},
					"tier": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Relay Cache machine catalog name, for example `RC10`. CPU and memory are derived from it; an unknown name is rejected.",
					},
					"resources": schema.SingleNestedAttribute{
						Required: true,
						Attributes: map[string]schema.Attribute{
							"cpu":    schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "CPU request derived from `tier`."},
							"memory": schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "Memory request derived from `tier`."},
							"storage": schema.StringAttribute{
								Required: true,
								MarkdownDescription: "Volume size for persistence, Kubernetes quantity. It can only grow here; a plan that shrinks it fails. " +
									"With `storage_autoscale` enabled the platform may grow the volume past it, reported in `storage_status`.",
							},
							"storage_autoscale": schema.SingleNestedAttribute{
								Optional: true,
								MarkdownDescription: "Grows the group's volumes past `storage`, up to `max_storage`, when disk usage crosses `threshold_percent`. " +
									"Omitting this block, or removing it from a config that had it, disables autoscaling.",
								Attributes: map[string]schema.Attribute{
									"enabled": schema.BoolAttribute{Required: true, MarkdownDescription: "Turn storage autoscaling on for this group."},
									"threshold_percent": schema.Int64Attribute{
										Optional: true, Computed: true, PlanModifiers: keepInt,
										Validators:          []validator.Int64{int64validator.Any(int64validator.OneOf(0), int64validator.Between(50, 95))},
										MarkdownDescription: "Disk-usage percent that triggers a grow. `0` or omitted uses the platform default of 80; otherwise 50-95.",
									},
									"increase_percent": schema.Int64Attribute{
										Optional: true, Computed: true, PlanModifiers: keepInt,
										Validators:          []validator.Int64{int64validator.Any(int64validator.OneOf(0), int64validator.Between(10, 100))},
										MarkdownDescription: "Percent to grow the volume by on each trigger. `0` or omitted uses the platform default of 20; otherwise 10-100.",
									},
									"max_storage": schema.StringAttribute{
										Optional:            true,
										MarkdownDescription: "Ceiling the platform will not grow past, Kubernetes quantity. Required when `enabled` is true, and must be at least `storage`.",
									},
								},
							},
						},
					},
				}},
			},
			"parameters": schema.MapAttribute{
				ElementType: types.StringType, Optional: true, Computed: true,
				PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Valkey settings, for example `maxmemory-policy`. An empty map resets them to defaults; omitting the attribute leaves them unmanaged. " +
					"Settings the platform manages, such as `maxmemory` or `port`, are rejected.",
			},
			"keep_backups_on_destroy": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Keep the cluster's backups when it is destroyed.",
			},
			"phase":             schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle phase after the last apply."},
			"phase_message":     schema.StringAttribute{Computed: true, MarkdownDescription: "Detail for the phase."},
			"endpoint_host":     schema.StringAttribute{Computed: true, MarkdownDescription: "Public read-write endpoint host. Empty until the platform publishes it."},
			"endpoint_port":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Public read-write endpoint port."},
			"public_endpoints":  schema.ListNestedAttribute{Computed: true, NestedObject: endpoint, MarkdownDescription: "Public endpoints by role."},
			"private_endpoints": schema.ListNestedAttribute{Computed: true, NestedObject: endpoint, MarkdownDescription: "Private endpoints by role, when private access is enabled."},
			"storage_status": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"node_group": schema.StringAttribute{Computed: true},
					"size":       schema.StringAttribute{Computed: true},
				}},
				MarkdownDescription: "Volume size the platform has grown to via `storage_autoscale`, when above the group's `storage` floor.",
			},
			"ready_nodes": schema.Int64Attribute{Computed: true, MarkdownDescription: "Nodes ready."},
			"total_nodes": schema.Int64Attribute{Computed: true, MarkdownDescription: "Nodes expected."},
			"created_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time, RFC 3339."},
			"updated_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Last change time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

func (r *valkeyClusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *valkeyClusterResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var mode types.String
	var shards, replicas types.Int64
	var nodes types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("mode"), &mode)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("shards"), &shards)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("replicas_per_shard"), &replicas)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("nodes"), &nodes)...)
	if resp.Diagnostics.HasError() || mode.IsUnknown() {
		return
	}

	if mode.ValueString() != valkeyModeCluster {
		for _, p := range []struct {
			name  string
			value types.Int64
		}{{"shards", shards}, {"replicas_per_shard", replicas}} {
			if !p.value.IsNull() {
				resp.Diagnostics.AddAttributeError(path.Root(p.name), "Only in cluster mode",
					p.name+" shards the keyspace, which only mode \"cluster\" does. Remove it or set mode = \"cluster\".")
			}
		}
	} else if shards.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("shards"), "Missing shards",
			"Cluster mode splits the keyspace across shards primaries; set shards to 3 or more.")
	}

	if nodes.IsNull() || nodes.IsUnknown() {
		return
	}
	for label, element := range nodes.Elements() {
		group, _ := element.(types.Object)
		attrs := group.Attributes()
		validateStorageAutoscale(resp, label, attrs["resources"])
		count, _ := attrs["count"].(types.Int64)
		if mode.ValueString() != valkeyModeCluster || shards.IsNull() || shards.IsUnknown() || replicas.IsUnknown() || count.IsUnknown() {
			continue
		}
		want := shards.ValueInt64() * (1 + replicas.ValueInt64())
		have := int64(1)
		if !count.IsNull() {
			have = count.ValueInt64()
		}
		if have != want {
			resp.Diagnostics.AddAttributeError(path.Root("nodes").AtMapKey(label).AtName("count"), "Count does not match the shards",
				"Cluster mode runs shards * (1 + replicas_per_shard) = "+strconv.FormatInt(want, 10)+" nodes, but group "+label+" asks for "+strconv.FormatInt(have, 10)+".")
		}
	}
}

func (r *valkeyClusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, prior valkeyClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.Version.IsUnknown() && !plan.Version.Equal(prior.Version) {
		resp.Diagnostics.AddAttributeError(path.Root("version"), "Version cannot change here",
			"Upgrading from "+prior.Version.ValueString()+" to "+plan.Version.ValueString()+" goes through Relay's upgrade flow. Run the upgrade, then set version to the new value.")
	}
	changed := false
	for name, planned := range plan.Nodes {
		current, ok := prior.Nodes[name]
		if !ok {
			planned.Resources.CPU = types.StringUnknown()
			planned.Resources.Memory = types.StringUnknown()
			planned.Resources.StorageAutoscale = unknownComputedStorageAutoscale(planned.Resources.StorageAutoscale)
			plan.Nodes[name] = planned
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root("nodes"))
			changed = true
			continue
		}
		group := path.Root("nodes").AtMapKey(name)
		if shrinks(planned.Resources.Storage, current.Resources.Storage) {
			resp.Diagnostics.AddAttributeError(group.AtName("resources").AtName("storage"), "Storage cannot shrink",
				"Volumes only grow. Group "+name+" is at "+current.Resources.Storage.ValueString()+" and the plan asks for "+planned.Resources.Storage.ValueString()+".")
		}
		if current.Resources.StorageAutoscale.IsNull() && !planned.Resources.StorageAutoscale.IsNull() && !planned.Resources.StorageAutoscale.IsUnknown() {
			planned.Resources.StorageAutoscale = unknownComputedStorageAutoscale(planned.Resources.StorageAutoscale)
			plan.Nodes[name] = planned
			changed = true
		}
	}
	if changed {
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
	}
}

func (r *valkeyClusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan valkeyClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 30*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	out, err := r.client.API.CreateDatabase(scoped, &serverv1.CreateDatabaseRequest{Name: plan.Name.ValueString(), Spec: valkeySpecToProto(plan)})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create cluster", err.Error())
		return
	}
	id := out.GetDatabase().GetId()
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), plan.WorkspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), plan.Name)...)

	db, err := r.waitRunning(scoped, id, timeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not become ready", err.Error())
		return
	}
	state := valkeyClusterFromProto(db, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *valkeyClusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state valkeyClusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := r.client.Workspace(ctx, state.WorkspaceID.ValueString())
	out, err := r.client.API.GetDatabase(scoped, &serverv1.GetDatabaseRequest{Id: state.ID.ValueString()})
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read cluster", err.Error())
		return
	}
	if phase := out.GetDatabase().GetStatus().GetPhase(); phase == "deleting" || phase == phaseDeleted {
		resp.State.RemoveResource(ctx)
		return
	}
	if actual := out.GetDatabase().GetSpec().GetEngine(); !strings.EqualFold(actual, engineValkey) {
		resp.Diagnostics.AddError("Wrong cluster engine", fmt.Sprintf("Cluster %q is a %s cluster; manage it with relay_postgresql_cluster.", state.ID.ValueString(), actual))
		return
	}
	state = valkeyClusterFromProto(out.GetDatabase(), state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *valkeyClusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior valkeyClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	timeout, diags := plan.Timeouts.Update(ctx, 30*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	if paths := valkeyUpdateMask(plan, prior); len(paths) > 0 {
		if _, err := r.client.API.UpdateDatabase(scoped, &serverv1.UpdateDatabaseRequest{
			Id:         plan.ID.ValueString(),
			Spec:       valkeySpecToProto(plan),
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		}); err != nil {
			resp.Diagnostics.AddError("Cannot update cluster", err.Error())
			return
		}
	}
	db, err := r.waitRunning(scoped, plan.ID.ValueString(), timeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not settle after update", err.Error())
		return
	}
	state := valkeyClusterFromProto(db, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *valkeyClusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state valkeyClusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	timeout, diags := state.Timeouts.Delete(ctx, 20*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := r.client.Workspace(ctx, state.WorkspaceID.ValueString())
	id := state.ID.ValueString()
	_, err := r.client.API.DeleteDatabase(scoped, &serverv1.DeleteDatabaseRequest{Id: id, KeepBackups: state.KeepBackupsOnDestroy.ValueBool()})
	if relay.IsNotFound(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete cluster", err.Error())
		return
	}
	err = waitFor(scoped, timeout, func(ctx context.Context) (bool, error) {
		out, err := r.client.API.GetDatabase(ctx, &serverv1.GetDatabaseRequest{Id: id})
		if relay.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		st := out.GetDatabase().GetStatus()
		if st.GetPhase() == phaseFailed {
			return false, errors.New("deletion failed: " + st.GetMessage())
		}
		return st.GetPhase() == phaseDeleted, nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Cluster was not deleted", err.Error())
	}
}

func (r *valkeyClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	workspaceID, id, ok := strings.Cut(req.ID, "/")
	if !ok || workspaceID == "" || id == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cluster_id>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), workspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("keep_backups_on_destroy"), false)...)
}

func (r *valkeyClusterResource) waitRunning(ctx context.Context, id string, timeout time.Duration) (*serverv1.Database, error) {
	var db *serverv1.Database
	err := waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		out, err := r.client.API.GetDatabase(ctx, &serverv1.GetDatabaseRequest{Id: id})
		if err != nil {
			return false, err
		}
		db = out.GetDatabase()
		switch db.GetStatus().GetPhase() {
		case phaseRunning:
			return true, nil
		case phaseFailed:
			return false, errors.New("provisioning failed: " + db.GetStatus().GetMessage())
		}
		return false, nil
	})
	return db, err
}

func valkeySpecToProto(m valkeyClusterModel) *serverv1.DatabaseSpec {
	spec := &serverv1.DatabaseSpec{
		Engine:         engineValkey,
		Version:        m.Version.ValueString(),
		BackupsEnabled: true,
		Valkey: &serverv1.ValkeyConfig{
			Mode:             m.Mode.ValueString(),
			Shards:           int32(m.Shards.ValueInt64()),
			ReplicasPerShard: int32(m.ReplicasPerShard.ValueInt64()),
			Parameters:       stringMapToGo(m.Parameters),
		},
	}
	for name, n := range m.Nodes {
		spec.PrimaryNodeGroup = name
		spec.Nodes = append(spec.Nodes, &serverv1.NodeSpec{
			Name:   name,
			Role:   roleElectable,
			Count:  int32(n.Count.ValueInt64()),
			Cloud:  n.Cloud.ValueString(),
			Region: n.Region.ValueString(),
			Tier:   n.Tier.ValueString(),
			Resources: &serverv1.ResourceRequirements{
				Storage:          n.Resources.Storage.ValueString(),
				StorageAutoscale: storageAutoscaleToProto(n.Resources.StorageAutoscale),
			},
		})
	}
	return spec
}

func valkeyUpdateMask(plan, prior valkeyClusterModel) []string {
	var paths []string
	if !reflect.DeepEqual(plan.Nodes, prior.Nodes) {
		paths = append(paths, "nodes")
	}
	switch {
	case !plan.Shards.Equal(prior.Shards) || !plan.ReplicasPerShard.Equal(prior.ReplicasPerShard):
		paths = append(paths, "valkey")
	case !plan.Parameters.Equal(prior.Parameters):
		paths = append(paths, "valkey.parameters")
	}
	return paths
}

func valkeyNodesFromProto(nodes []*serverv1.NodeSpec, prior map[string]valkeyNodeGroupModel) map[string]valkeyNodeGroupModel {
	out := make(map[string]valkeyNodeGroupModel, len(nodes))
	for _, n := range nodes {
		res := n.GetResources()
		priorAutoscale := types.ObjectNull(storageAutoscaleAttrTypes)
		if p, ok := prior[n.GetName()]; ok {
			priorAutoscale = p.Resources.StorageAutoscale
		}
		out[n.GetName()] = valkeyNodeGroupModel{
			Count:  types.Int64Value(int64(n.GetCount())),
			Cloud:  types.StringValue(n.GetCloud()),
			Region: types.StringValue(n.GetRegion()),
			Tier:   types.StringValue(n.GetTier()),
			Resources: valkeyResourcesModel{
				CPU:              types.StringValue(res.GetCpu()),
				Memory:           types.StringValue(res.GetMemory()),
				Storage:          types.StringValue(res.GetStorage()),
				StorageAutoscale: storageAutoscaleFromProto(res.GetStorageAutoscale(), priorAutoscale),
			},
		}
	}
	return out
}

func valkeyClusterFromProto(db *serverv1.Database, prior valkeyClusterModel) valkeyClusterModel {
	spec, st := db.GetSpec(), db.GetStatus()
	v := spec.GetValkey()
	mode := v.GetMode()
	if mode == "" {
		mode = valkeyModeReplication
	}
	shards, replicas := types.Int64Null(), types.Int64Null()
	if mode == valkeyModeCluster {
		shards = types.Int64Value(int64(v.GetShards()))
		if v.GetReplicasPerShard() != 0 || !prior.ReplicasPerShard.IsNull() {
			replicas = types.Int64Value(int64(v.GetReplicasPerShard()))
		}
	}
	return valkeyClusterModel{
		ID:                   types.StringValue(db.GetId()),
		WorkspaceID:          prior.WorkspaceID,
		Name:                 types.StringValue(db.GetName()),
		Version:              types.StringValue(spec.GetVersion()),
		Mode:                 types.StringValue(mode),
		Shards:               shards,
		ReplicasPerShard:     replicas,
		Nodes:                valkeyNodesFromProto(spec.GetNodes(), prior.Nodes),
		Parameters:           stringMapFromGo(v.GetParameters()),
		KeepBackupsOnDestroy: prior.KeepBackupsOnDestroy,
		Phase:                types.StringValue(st.GetPhase()),
		PhaseMessage:         types.StringValue(st.GetMessage()),
		EndpointHost:         types.StringValue(st.GetEndpoint().GetHost()),
		EndpointPort:         types.Int64Value(int64(st.GetEndpoint().GetPort())),
		PublicEndpoints:      endpointsFromProto(st.GetPublicEndpoints()),
		PrivateEndpoints:     endpointsFromProto(st.GetPrivateEndpoints()),
		StorageStatus:        storageStatusFromProto(st.GetStorage()),
		ReadyNodes:           types.Int64Value(int64(st.GetReadyNodes())),
		TotalNodes:           types.Int64Value(int64(st.GetTotalNodes())),
		CreatedAt:            timestampValue(st.GetCreatedAt()),
		UpdatedAt:            timestampValue(st.GetUpdatedAt()),
		Timeouts:             prior.Timeouts,
	}
}
