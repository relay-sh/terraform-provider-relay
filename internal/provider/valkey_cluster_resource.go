package provider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
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
	CPU    types.String `tfsdk:"cpu"`
	Memory types.String `tfsdk:"memory"`
}

type valkeyClusterModel struct {
	ID                   types.String                    `tfsdk:"id"`
	WorkspaceID          types.String                    `tfsdk:"workspace_id"`
	Name                 types.String                    `tfsdk:"name"`
	Version              types.String                    `tfsdk:"version"`
	Mode                 types.String                    `tfsdk:"mode"`
	Shards               types.Int64                     `tfsdk:"shards"`
	ReplicasPerShard     types.Int64                     `tfsdk:"replicas_per_shard"`
	Storage              types.Object                    `tfsdk:"storage"`
	Nodes                map[string]valkeyNodeGroupModel `tfsdk:"nodes"`
	Parameters           types.Map                       `tfsdk:"parameters"`
	KeepBackupsOnDestroy types.Bool                      `tfsdk:"keep_backups_on_destroy"`
	DeletionProtection   types.Bool                      `tfsdk:"deletion_protection"`
	Restore              types.Object                    `tfsdk:"restore"`
	DefaultConnections   types.Object                    `tfsdk:"default_connections"`
	Phase                types.String                    `tfsdk:"phase"`
	PhaseMessage         types.String                    `tfsdk:"phase_message"`
	EndpointHost         types.String                    `tfsdk:"endpoint_host"`
	EndpointPort         types.Int64                     `tfsdk:"endpoint_port"`
	PublicEndpoints      types.List                      `tfsdk:"public_endpoints"`
	PrivateEndpoints     types.List                      `tfsdk:"private_endpoints"`
	StorageStatus        types.Object                    `tfsdk:"storage_status"`
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
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}

	resp.Schema = schema.Schema{
		Version: 1,
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
			"storage": storageAttribute("Minimum volume size for persistence, Kubernetes quantity. Omit it to use the tier's storage. " +
				"The platform raises the volume to at least 1.5 times the node's memory and never lowers it, so after a refresh this holds the size it is at; " +
				"a configured value at or below that plans no change, and a larger one grows the volume. " +
				"With `autoscale` enabled the platform may grow the volume past it, reported in `storage_status`."),
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
						},
					},
				}},
			},
			"parameters": schema.MapAttribute{
				ElementType: types.StringType, Optional: true, Computed: true,
				PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Valkey settings, for example `maxmemory-policy`. An empty map resets them to defaults; omitting the attribute leaves them unmanaged. " +
					"Settings the platform manages, such as `maxmemory` or `port`, are rejected. " +
					"The platform stores canonical names (lower case, aliases resolved); the spelling you configure is kept in state when it maps to the stored name.",
			},
			"keep_backups_on_destroy": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Keep the cluster's backups when it is destroyed.",
			},
			"deletion_protection": deletionProtectionAttribute("cluster"),
			"restore": restoreAttribute("`mode`, `version` when set, and the node group label must match the source's.",
				"Recover up to this instant, RFC 3339, for example `2026-10-31T12:00:00Z`. It replays the source's AOF archive, so it needs `mode = \"replication\"`, "+
					"a source that still exists with `pitr_window_days` on its backup schedule, and an instant inside that window. Omit it to restore the latest backup."),
			"default_connections": defaultConnectionsAttribute(),
			"phase":               schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle phase after the last apply."},
			"phase_message":       schema.StringAttribute{Computed: true, MarkdownDescription: "Detail for the phase."},
			"endpoint_host":       schema.StringAttribute{Computed: true, MarkdownDescription: "Public read-write endpoint host. Empty until the platform publishes it."},
			"endpoint_port":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Public read-write endpoint port."},
			"public_endpoints":    schema.ListNestedAttribute{Computed: true, NestedObject: endpoint, MarkdownDescription: "Public endpoints by role."},
			"private_endpoints":   schema.ListNestedAttribute{Computed: true, NestedObject: endpoint, MarkdownDescription: "Private endpoints by role, when private access is enabled."},
			"storage_status": schema.SingleNestedAttribute{
				Computed: true,
				Attributes: map[string]schema.Attribute{
					"size":             schema.StringAttribute{Computed: true, MarkdownDescription: "Volume size the platform has grown the cluster to."},
					"last_expanded_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When the platform last grew the volumes, RFC 3339."},
				},
				MarkdownDescription: "Volume size the platform has grown to via `storage.autoscale`, when above `storage.size`. Null until it grows.",
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
	validateStorage(ctx, req.Config, &resp.Diagnostics)
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
	if req.Plan.Raw.IsNull() {
		return
	}
	if req.State.Raw.IsNull() {
		r.planRestore(ctx, req, resp)
		return
	}
	var plan, prior valkeyClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.Restore = keepEquivalentRestore(plan.Restore, prior.Restore)
	if restoreChanged(plan.Restore, prior.Restore) {
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("restore"))
	}
	if !plan.Version.IsUnknown() && !plan.Version.Equal(prior.Version) {
		resp.Diagnostics.AddAttributeError(path.Root("version"), "Version cannot change here",
			"Upgrading from "+prior.Version.ValueString()+" to "+plan.Version.ValueString()+" goes through Relay's upgrade flow. Run the upgrade, then set version to the new value.")
	}
	if order, ok := compareQuantity(storageSizeOf(plan.Storage), storageSizeOf(prior.Storage)); ok && order <= 0 {
		plan.Storage = withStorageSize(plan.Storage, storageSizeOf(prior.Storage))
	}
	if raised := keepRaisedMaxStorage(plan.Storage, prior.Storage); !raised.IsNull() {
		plan.Storage = withStorageAutoscale(plan.Storage, raised)
	}
	if autoscaleEnabledAnew(plan.Storage, prior.Storage) {
		plan.Storage = unknownComputedStorage(plan.Storage)
	}
	for name, planned := range plan.Nodes {
		current, ok := prior.Nodes[name]
		if !ok {
			planned.Resources.CPU = types.StringUnknown()
			planned.Resources.Memory = types.StringUnknown()
			plan.Nodes[name] = planned
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root("nodes"))
			continue
		}
		if !planned.Tier.IsUnknown() && !planned.Tier.Equal(current.Tier) {
			planned.Resources.CPU = types.StringUnknown()
			planned.Resources.Memory = types.StringUnknown()
			plan.Nodes[name] = planned
		}
	}
	if reflect.DeepEqual(plan.Nodes, prior.Nodes) && sameSettings(plan, prior) {
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &prior)...)
		return
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func sameSettings(a, b valkeyClusterModel) bool {
	return a.Name.Equal(b.Name) && a.WorkspaceID.Equal(b.WorkspaceID) && a.Version.Equal(b.Version) && a.Mode.Equal(b.Mode) &&
		a.Storage.Equal(b.Storage) && a.Shards.Equal(b.Shards) && a.ReplicasPerShard.Equal(b.ReplicasPerShard) && a.Parameters.Equal(b.Parameters) &&
		a.KeepBackupsOnDestroy.Equal(b.KeepBackupsOnDestroy) && a.DeletionProtection.Equal(b.DeletionProtection) && a.Restore.Equal(b.Restore) &&
		a.DefaultConnections.Equal(b.DefaultConnections) && a.Timeouts.Equal(b.Timeouts)
}

func (r *valkeyClusterResource) planRestore(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	var restore types.Object
	var workspaceID, version, mode types.String
	var nodes types.Map
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("restore"), &restore)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("workspace_id"), &workspaceID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("version"), &version)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("mode"), &mode)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("nodes"), &nodes)...)
	if known(restoreTargetTime(restore)) && mode.ValueString() == valkeyModeCluster {
		resp.Diagnostics.AddAttributeError(path.Root("restore").AtName("target_time"), "No point-in-time restore in Valkey cluster mode",
			"A point-in-time restore replays the AOF archive, which only mode \"replication\" keeps. Remove target_time to restore the latest backup.")
	}
	src := checkRestorePlan(ctx, r.client, workspaceID, version, restore, engineValkey, &resp.Diagnostics)
	if src == nil {
		return
	}
	if srcMode := valkeyModeOf(src); known(mode) && mode.ValueString() != srcMode {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Mode differs from the source",
			"The source runs in "+srcMode+" mode, and a backup restores only into the same mode. Set mode = \""+srcMode+"\".")
	}
	checkRestoreLabel(nodes, src.GetPrimaryNodeGroup(), &resp.Diagnostics)
}

func valkeyModeOf(spec *serverv1.DatabaseSpec) string {
	if mode := spec.GetValkey().GetMode(); mode != "" {
		return mode
	}
	return valkeyModeReplication
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
	var created *serverv1.Database
	if plan.Restore.IsNull() {
		out, err := r.client.API.CreateDatabase(scoped, &serverv1.CreateDatabaseRequest{
			Name: plan.Name.ValueString(), Spec: valkeySpecToProto(plan), DefaultConnections: defaultConnectionsToProto(plan.DefaultConnections),
			DeletionProtection: plan.DeletionProtection.ValueBool(),
		})
		if err != nil {
			resp.Diagnostics.AddError("Cannot create cluster", err.Error())
			return
		}
		created = out.GetDatabase()
	} else {
		out, err := r.client.API.RestoreDatabase(scoped, restoreRequest(plan.Name.ValueString(), plan.Restore))
		if err != nil {
			resp.Diagnostics.AddError("Cannot restore cluster", err.Error())
			return
		}
		created = out.GetDatabase()
	}
	id := created.GetId()
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), plan.WorkspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), plan.Name)...)

	db, err := r.waitRunning(scoped, id, timeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not become ready", err.Error())
		return
	}
	if !plan.Restore.IsNull() {
		if db = r.convergeRestore(scoped, req.Plan, plan, db, timeout, resp); db == nil {
			return
		}
	}
	if !plan.Restore.IsNull() && known(plan.DefaultConnections) {
		if db, err = setDefaultConnections(scoped, r.client, id, plan.DefaultConnections); err != nil {
			resp.Diagnostics.AddError("Cannot set default connections on the restored cluster", err.Error())
			return
		}
	}
	protected, ok := setClusterDeletionProtection(scoped, r.client, id, plan.DeletionProtection, types.BoolValue(db.GetDeletionProtection()), &resp.Diagnostics)
	if !ok {
		return
	}
	if protected != nil {
		db = protected
	}
	state := valkeyClusterFromProto(db, plan)
	state.Storage = keepPlannedStorage(state.Storage, plan.Storage)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *valkeyClusterResource) convergeRestore(ctx context.Context, config tfsdk.Plan, plan valkeyClusterModel, db *serverv1.Database, timeout time.Duration, resp *resource.CreateResponse) *serverv1.Database {
	restored := valkeyClusterFromProto(db, plan)
	if order, ok := compareQuantity(storageSizeOf(plan.Storage), storageSizeOf(restored.Storage)); ok && order <= 0 {
		restored.Storage = withStorageSize(restored.Storage, storageSizeOf(plan.Storage))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &restored)...)
	if fixed := driftedRoots(config, resp.State, []string{"version", "mode"}); len(fixed) > 0 {
		resp.Diagnostics.AddError("Restored cluster does not match the configuration",
			"The source's "+strings.Join(fixed, " and ")+" differ from the configuration and cannot change in place. Set them to the values in state, then apply again.")
		return nil
	}
	var paths []string
	for _, root := range driftedRoots(config, resp.State, []string{"nodes", "storage", "shards", "replicas_per_shard", "parameters"}) {
		p := map[string]string{"nodes": "nodes", "storage": "storage", "shards": "valkey", "replicas_per_shard": "valkey", "parameters": "valkey.parameters"}[root]
		if !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	if slices.Contains(paths, "valkey") {
		paths = slices.DeleteFunc(paths, func(p string) bool { return p == "valkey.parameters" })
	}
	if len(paths) == 0 {
		return db
	}
	desired := plan
	desired.Version = restored.Version
	if desired.Parameters.IsUnknown() {
		desired.Parameters = restored.Parameters
	}
	if _, err := r.client.API.UpdateDatabase(ctx, &serverv1.UpdateDatabaseRequest{
		Id:         db.GetId(),
		Spec:       valkeySpecToProto(desired),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	}); err != nil {
		resp.Diagnostics.AddError("Cannot apply the configuration to the restored cluster", err.Error())
		return nil
	}
	db, err := r.waitRunning(ctx, db.GetId(), timeout)
	if err != nil {
		resp.Diagnostics.AddError("Restored cluster did not settle after update", err.Error())
		return nil
	}
	return db
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
	if _, ok := setClusterDeletionProtection(scoped, r.client, plan.ID.ValueString(), plan.DeletionProtection, prior.DeletionProtection, &resp.Diagnostics); !ok {
		return
	}
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
	if known(plan.DefaultConnections) && !plan.DefaultConnections.Equal(prior.DefaultConnections) {
		if _, err := setDefaultConnections(scoped, r.client, plan.ID.ValueString(), plan.DefaultConnections); err != nil {
			resp.Diagnostics.AddError("Cannot change default connections", err.Error())
			return
		}
	}
	db, err := r.waitRunning(scoped, plan.ID.ValueString(), timeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not settle after update", err.Error())
		return
	}
	state := valkeyClusterFromProto(db, plan)
	state.Storage = keepPlannedStorage(state.Storage, plan.Storage)
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
	if refuseProtectedDelete(state.DeletionProtection, &resp.Diagnostics) {
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
			return !changing(db.GetStatus()), nil
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
		Storage:        storageToProto(m.Storage),
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
			Name:      name,
			Role:      roleElectable,
			Count:     int32(n.Count.ValueInt64()),
			Cloud:     n.Cloud.ValueString(),
			Region:    n.Region.ValueString(),
			Tier:      n.Tier.ValueString(),
			Resources: &serverv1.ResourceRequirements{},
		})
	}
	return spec
}

func valkeyUpdateMask(plan, prior valkeyClusterModel) []string {
	var paths []string
	if !reflect.DeepEqual(plan.Nodes, prior.Nodes) {
		paths = append(paths, "nodes")
	}
	if !plan.Storage.Equal(prior.Storage) {
		paths = append(paths, "storage")
	}
	switch {
	case !plan.Shards.Equal(prior.Shards) || !plan.ReplicasPerShard.Equal(prior.ReplicasPerShard):
		paths = append(paths, "valkey")
	case !plan.Parameters.Equal(prior.Parameters):
		paths = append(paths, "valkey.parameters")
	}
	return paths
}

func valkeyNodesFromProto(nodes []*serverv1.NodeSpec) map[string]valkeyNodeGroupModel {
	out := make(map[string]valkeyNodeGroupModel, len(nodes))
	for _, n := range nodes {
		res := n.GetResources()
		out[n.GetName()] = valkeyNodeGroupModel{
			Count:  types.Int64Value(int64(n.GetCount())),
			Cloud:  types.StringValue(n.GetCloud()),
			Region: types.StringValue(n.GetRegion()),
			Tier:   types.StringValue(n.GetTier()),
			Resources: valkeyResourcesModel{
				CPU:    types.StringValue(res.GetCpu()),
				Memory: types.StringValue(res.GetMemory()),
			},
		}
	}
	return out
}

func keepConfiguredParameterNames(server map[string]string, prior types.Map) types.Map {
	if prior.IsNull() || prior.IsUnknown() {
		return stringMapFromGo(server)
	}
	configured := make(map[string]string, len(prior.Elements()))
	for k, v := range prior.Elements() {
		configured[k] = v.(types.String).ValueString()
	}
	keys := make([]string, 0, len(server))
	for k := range server {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	names := make(map[string]string, len(server))
	claimed := map[string]bool{}
	for _, k := range keys {
		if _, ok := configured[k]; ok {
			names[k] = k
		}
	}
	sameName := func(k, p string) bool { return strings.ToLower(p) == k }
	sameValue := func(k, p string) bool { return configured[p] == server[k] }
	for _, match := range []func(k, p string) bool{sameName, sameValue} {
		for _, k := range keys {
			if _, ok := names[k]; ok {
				continue
			}
			var candidates []string
			for p := range configured {
				if _, inServer := server[p]; !inServer && !claimed[p] && match(k, p) {
					candidates = append(candidates, p)
				}
			}
			if len(candidates) == 1 {
				names[k] = candidates[0]
				claimed[candidates[0]] = true
			}
		}
	}
	elems := make(map[string]attr.Value, len(server))
	for _, k := range keys {
		name, ok := names[k]
		if !ok {
			name = k
		}
		elems[name] = types.StringValue(server[k])
	}
	return types.MapValueMust(types.StringType, elems)
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
		Storage:              storageFromProto(spec.GetStorage(), prior.Storage),
		Nodes:                valkeyNodesFromProto(spec.GetNodes()),
		Parameters:           keepConfiguredParameterNames(v.GetParameters(), prior.Parameters),
		KeepBackupsOnDestroy: prior.KeepBackupsOnDestroy,
		DeletionProtection:   types.BoolValue(db.GetDeletionProtection()),
		Restore:              prior.Restore,
		DefaultConnections:   defaultConnectionsFromProto(db.GetDefaultConnections(), prior.DefaultConnections),
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
