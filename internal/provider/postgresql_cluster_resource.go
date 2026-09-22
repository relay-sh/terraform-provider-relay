package provider

import (
	"context"
	"errors"
	"reflect"
	"sort"
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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectdefault"
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

type postgresqlClusterResource struct {
	client *client.Client
}

type postgresqlClusterModel struct {
	ID                   types.String              `tfsdk:"id"`
	WorkspaceID          types.String              `tfsdk:"workspace_id"`
	Name                 types.String              `tfsdk:"name"`
	Version              types.String              `tfsdk:"version"`
	Nodes                map[string]nodeGroupModel `tfsdk:"nodes"`
	PrimaryNodeGroup     types.String              `tfsdk:"primary_node_group"`
	Parameters           types.Map                 `tfsdk:"parameters"`
	Replication          types.Object              `tfsdk:"replication"`
	KeepBackupsOnDestroy types.Bool                `tfsdk:"keep_backups_on_destroy"`
	Phase                types.String              `tfsdk:"phase"`
	PhaseMessage         types.String              `tfsdk:"phase_message"`
	EndpointHost         types.String              `tfsdk:"endpoint_host"`
	EndpointPort         types.Int64               `tfsdk:"endpoint_port"`
	PublicEndpoints      types.List                `tfsdk:"public_endpoints"`
	PrivateEndpoints     types.List                `tfsdk:"private_endpoints"`
	ReadyNodes           types.Int64               `tfsdk:"ready_nodes"`
	TotalNodes           types.Int64               `tfsdk:"total_nodes"`
	CreatedAt            types.String              `tfsdk:"created_at"`
	UpdatedAt            types.String              `tfsdk:"updated_at"`
	Timeouts             timeouts.Value            `tfsdk:"timeouts"`
}

func newPostgreSQLClusterResource() resource.Resource {
	return &postgresqlClusterResource{}
}

func (r *postgresqlClusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_postgresql_cluster"
}

func (r *postgresqlClusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keepString := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	keepInt := []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed PostgreSQL cluster. Creating, updating and destroying wait for the cluster to settle. " +
			"Changing `name` or `workspace_id` replaces the cluster and its data; use `lifecycle { prevent_destroy = true }`.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "Cluster ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the cluster belongs to."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Cluster name, unique in the workspace."},
			"version": schema.StringAttribute{
				Optional: true, Computed: true, PlanModifiers: keepString,
				MarkdownDescription: "Major PostgreSQL version. Omit it and the platform picks one, which the next read reports. " +
					"It cannot change here: major upgrades go through Relay's upgrade flow, and a plan that changes it fails.",
			},
			"primary_node_group": schema.StringAttribute{
				Computed: true, PlanModifiers: keepString,
				MarkdownDescription: "Label of the group holding the primary. The platform picks an `electable` group and keeps it, so adding a group never moves the primary.",
			},
			"nodes": schema.MapNestedAttribute{
				Required:   true,
				Validators: []validator.Map{mapvalidator.SizeAtLeast(1)},
				MarkdownDescription: "Node groups, keyed by a label you choose. The label is the group's identity, in your configuration and in the platform: changing a value " +
					"inside one group is a diff on that group alone, `ignore_changes` can reach `nodes[\"readers\"].tier`, and renaming a group replaces it. What a group " +
					"does comes from `role`. Every group runs in the cluster's own cloud and region; one cluster does not span regions.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"role": schema.StringAttribute{
						Required:   true,
						Validators: []validator.String{stringvalidator.OneOf(roleElectable, roleStandby, roleReadOnly)},
						MarkdownDescription: "How much of the cluster's consensus this group takes part in. `electable` acknowledges synchronous commits and can be promoted. " +
							"`standby` never acknowledges a commit, which is how a remote group stays out of the commit path — and, since the engine has no per-member " +
							"preference, how you pin the synchronous replica to the groups near the primary; under `replication.mode = \"synchronous\"` only a group that " +
							"was acknowledging commits is promoted, so a standby group is a manual failover, not an automatic one. `read_only` is neither acknowledged nor " +
							"promoted. The platform derives the `nosync` and `nofailover` tags from this.",
					},
					"count": schema.Int64Attribute{
						Optional: true, Computed: true, Default: int64default.StaticInt64(1),
						Validators:          []validator.Int64{int64validator.AtLeast(1)},
						MarkdownDescription: "Nodes in the group. Lowering it deletes the removed nodes' volumes without draining them first.",
					},
					"cloud": schema.StringAttribute{
						Optional: true, Computed: true, PlanModifiers: keepString,
						MarkdownDescription: "Cloud the group runs in, for example `aws`. With `region` it selects the provisioner. Changing cloud or region moves the group (add dest, switchover, drain source).",
					},
					"region": schema.StringAttribute{
						Optional: true, Computed: true, PlanModifiers: keepString,
						MarkdownDescription: "Region the group runs in, for example `us-east-1`. Changing it moves the group to that region.",
					},
					"tier": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Machine catalog name, for example `R20`. CPU and memory are derived from it; an unknown name is rejected. Changing it restarts the group's pods.",
					},
					"resources": schema.SingleNestedAttribute{
						Required: true,
						Attributes: map[string]schema.Attribute{
							"cpu":     schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "CPU request derived from `tier`."},
							"memory":  schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "Memory request derived from `tier`."},
							"storage": schema.StringAttribute{Required: true, MarkdownDescription: "Volume size, Kubernetes quantity. It can only grow; a plan that shrinks it fails."},
							"iops": schema.Int64Attribute{
								Optional: true, Computed: true, PlanModifiers: keepInt,
								MarkdownDescription: "Provisioned IOPS. Can change in place. Required for `io1`/`io2`. AWS caps: gp3 3000–80000 (500/GiB above 3000), io1 100–64000 (50/GiB), io2 100–256000 (1000/GiB).",
							},
							"throughput": schema.Int64Attribute{
								Optional: true, Computed: true, PlanModifiers: keepInt,
								MarkdownDescription: "Provisioned throughput in MiB/s. Can change in place.",
							},
							"disk_type": schema.StringAttribute{
								Optional: true, Computed: true, PlanModifiers: keepString,
								MarkdownDescription: "Cloud volume type. AWS: `gp3` (default), `io1`, `io2` (min 4Gi). GCP: `pd-ssd`, `pd-balanced`, `hyperdisk-balanced`. Azure: `PremiumV2_LRS`, `UltraSSD_LRS`. Can change in place. `io1`/`io2` require `iops` within the per-GiB cap.",
							},
						},
					},
				}},
			},
			"parameters": schema.MapAttribute{
				ElementType: types.StringType, Optional: true, Computed: true,
				PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "PostgreSQL settings (GUCs). An empty map resets them to defaults; omitting the attribute leaves them unmanaged. " +
					"Some need a restart, which Relay orchestrates with a switchover.",
			},
			"replication": schema.SingleNestedAttribute{
				Optional: true, Computed: true,
				Default: objectdefault.StaticValue(types.ObjectValueMust(replicationAttrTypes, map[string]attr.Value{
					"mode": types.StringValue("async"), "synchronous_replicas": types.Int64Null(),
				})),
				MarkdownDescription: "Commit replication for the cluster. Patroni configures synchronous mode per cluster, not per node.",
				Attributes: map[string]schema.Attribute{
					"mode": schema.StringAttribute{
						Optional: true, Computed: true, Default: stringdefault.StaticString("async"),
						Validators:          []validator.String{stringvalidator.OneOf("async", "synchronous")},
						MarkdownDescription: "`async` or `synchronous`.",
					},
					"synchronous_replicas": schema.Int64Attribute{
						Optional:            true,
						Validators:          []validator.Int64{int64validator.AtLeast(1)},
						MarkdownDescription: "Replicas that must acknowledge a commit. Required when `mode` is `synchronous`, and the groups that can acknowledge must hold at least this many nodes.",
					},
				},
			},
			"keep_backups_on_destroy": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Keep the cluster's backups when it is destroyed.",
			},
			"phase":             schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle phase after the last apply."},
			"phase_message":     schema.StringAttribute{Computed: true, MarkdownDescription: "Detail for the phase."},
			"endpoint_host":     schema.StringAttribute{Computed: true, MarkdownDescription: "Primary endpoint host."},
			"endpoint_port":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Primary endpoint port."},
			"public_endpoints":  schema.ListNestedAttribute{Computed: true, NestedObject: endpoint, MarkdownDescription: "Public endpoints by role."},
			"private_endpoints": schema.ListNestedAttribute{Computed: true, NestedObject: endpoint, MarkdownDescription: "Private endpoints by role, when private access is enabled."},
			"ready_nodes":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Nodes ready."},
			"total_nodes":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Nodes expected."},
			"created_at":        schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time, RFC 3339."},
			"updated_at":        schema.StringAttribute{Computed: true, MarkdownDescription: "Last change time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

func (r *postgresqlClusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *postgresqlClusterResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var replication types.Object
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("replication"), &replication)...)
	if !replication.IsNull() && !replication.IsUnknown() {
		attrs := replication.Attributes()
		if attrs["mode"].(types.String).ValueString() == "synchronous" && attrs["synchronous_replicas"].(types.Int64).IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("replication").AtName("synchronous_replicas"),
				"Missing synchronous_replicas", "Synchronous replication needs the number of replicas that must acknowledge a commit.")
		}
	}

	var nodes types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("nodes"), &nodes)...)
	if nodes.IsNull() || nodes.IsUnknown() {
		return
	}
	electable := false
	placements := map[string][]string{}
	for label, element := range nodes.Elements() {
		group, _ := element.(types.Object)
		attrs := group.Attributes()
		if role, ok := attrs["role"].(types.String); ok && role.ValueString() == roleElectable {
			electable = true
		}
		cloud, _ := attrs["cloud"].(types.String)
		region, _ := attrs["region"].(types.String)
		if cloud.IsNull() || cloud.IsUnknown() || region.IsNull() || region.IsUnknown() {
			continue
		}
		place := cloud.ValueString() + "/" + region.ValueString()
		placements[place] = append(placements[place], label)
	}
	if !electable {
		resp.Diagnostics.AddAttributeError(path.Root("nodes"), "No electable group",
			"A cluster needs at least one group with role \"electable\" to hold the primary.")
	}
	if len(placements) > 1 {
		resp.Diagnostics.AddAttributeError(path.Root("nodes"), "One cluster, one region",
			"The groups ask for "+describePlacements(placements)+". Every group runs where the cluster runs; spreading one cluster across regions is not available yet.")
	}
}

func describePlacements(placements map[string][]string) string {
	out := make([]string, 0, len(placements))
	for place, labels := range placements {
		sort.Strings(labels)
		out = append(out, place+" ("+strings.Join(labels, ", ")+")")
	}
	sort.Strings(out)
	return strings.Join(out, " and ")
}

func (r *postgresqlClusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, prior postgresqlClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.Version.IsUnknown() && !plan.Version.Equal(prior.Version) {
		resp.Diagnostics.AddAttributeError(path.Root("version"), "Version cannot change here",
			"Upgrading from "+prior.Version.ValueString()+" to "+plan.Version.ValueString()+" goes through Relay's upgrade flow. Run the upgrade, then set version to the new value.")
	}
	added := false
	for name, planned := range plan.Nodes {
		current, ok := prior.Nodes[name]
		if !ok {
			// Terraform proposes a new map element from config alone, leaving its computed attributes null instead of unknown.
			plan.Nodes[name] = unknownComputed(planned)
			added = true
			continue
		}
		group := path.Root("nodes").AtMapKey(name)
		if shrinks(planned.Resources.Storage, current.Resources.Storage) {
			resp.Diagnostics.AddAttributeError(group.AtName("resources").AtName("storage"), "Storage cannot shrink",
				"Volumes only grow. Group "+name+" is at "+current.Resources.Storage.ValueString()+" and the plan asks for "+planned.Resources.Storage.ValueString()+".")
		}
		if !planned.Count.IsUnknown() && planned.Count.ValueInt64() < current.Count.ValueInt64() {
			resp.Diagnostics.AddAttributeWarning(group.AtName("count"), "Removed nodes lose their volumes",
				"Group "+name+" goes from "+current.Count.String()+" to "+planned.Count.String()+" nodes. Relay deletes the removed nodes and their volumes without draining them first.")
		}
	}
	if added {
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
	}
}

func unknownComputed(g nodeGroupModel) nodeGroupModel {
	g.Cloud = unknownIfNull(g.Cloud)
	g.Region = unknownIfNull(g.Region)
	g.Resources.CPU = types.StringUnknown()
	g.Resources.Memory = types.StringUnknown()
	g.Resources.DiskType = unknownIfNull(g.Resources.DiskType)
	if g.Resources.IOPS.IsNull() {
		g.Resources.IOPS = types.Int64Unknown()
	}
	if g.Resources.Throughput.IsNull() {
		g.Resources.Throughput = types.Int64Unknown()
	}
	return g
}

func unknownIfNull(v types.String) types.String {
	if v.IsNull() {
		return types.StringUnknown()
	}
	return v
}

func (r *postgresqlClusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan postgresqlClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 30*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	catalog, err := placements(scoped, r.client)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list the platform's provisioners", err.Error())
		return
	}
	spec, err := clusterSpecToProto(plan, catalog)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("nodes"), "Cannot place the cluster", err.Error())
		return
	}
	out, err := r.client.API.CreateDatabase(scoped, &serverv1.CreateDatabaseRequest{Name: plan.Name.ValueString(), Spec: spec})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create cluster", err.Error())
		return
	}
	id := out.GetDatabase().GetId()
	// The cluster exists from here on, so its identity goes to state before the wait:
	// a wait that fails leaves a tainted resource, and a destroy has to be able to
	// address it — including the workspace the delete is scoped to.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), plan.WorkspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), plan.Name)...)

	db, err := r.waitRunning(scoped, id, timeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not become ready", err.Error())
		return
	}
	state := clusterFromProto(db, plan, catalog)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *postgresqlClusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state postgresqlClusterModel
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
	catalog, err := placements(scoped, r.client)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list the platform's provisioners", err.Error())
		return
	}
	state = clusterFromProto(out.GetDatabase(), state, catalog)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *postgresqlClusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior postgresqlClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	timeout, diags := plan.Timeouts.Update(ctx, 30*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	catalog, err := placements(scoped, r.client)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list the platform's provisioners", err.Error())
		return
	}
	if paths := clusterUpdateMask(plan, prior); len(paths) > 0 {
		spec, specErr := clusterSpecToProto(plan, catalog)
		if specErr != nil {
			resp.Diagnostics.AddAttributeError(path.Root("nodes"), "Cannot place the cluster", specErr.Error())
			return
		}
		if _, err = r.client.API.UpdateDatabase(scoped, &serverv1.UpdateDatabaseRequest{
			Id:         plan.ID.ValueString(),
			Spec:       spec,
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
	state := clusterFromProto(db, plan, catalog)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *postgresqlClusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state postgresqlClusterModel
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

func (r *postgresqlClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	workspaceID, id, ok := strings.Cut(req.ID, "/")
	if !ok || workspaceID == "" || id == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cluster_id>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), workspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("keep_backups_on_destroy"), false)...)
}

func (r *postgresqlClusterResource) waitRunning(ctx context.Context, id string, timeout time.Duration) (*serverv1.Database, error) {
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

func clusterSpecToProto(m postgresqlClusterModel, catalog []*serverv1.DatabaseProvider) (*serverv1.DatabaseSpec, error) {
	nodes, err := nodesToProto(m.Nodes, catalog)
	if err != nil {
		return nil, err
	}
	spec := &serverv1.DatabaseSpec{
		Engine:           enginePostgreSQL,
		Version:          m.Version.ValueString(),
		Nodes:            nodes,
		PrimaryNodeGroup: primaryNodeGroup(m.Nodes, m.PrimaryNodeGroup),
		// Turning backups off is accepted and then ignored, so on is the only value that reads back unchanged.
		BackupsEnabled: true,
		Replication:    replicationToProto(m.Replication),
	}
	if !m.Parameters.IsNull() && !m.Parameters.IsUnknown() {
		spec.Postgresql = &serverv1.PostgreSQLConfig{Parameters: stringMapToGo(m.Parameters)}
	}
	return spec, nil
}

func clusterUpdateMask(plan, prior postgresqlClusterModel) []string {
	var paths []string
	add := func(changed bool, p string) {
		if changed {
			paths = append(paths, p)
		}
	}
	add(!reflect.DeepEqual(plan.Nodes, prior.Nodes), "nodes")
	add(!plan.Replication.Equal(prior.Replication), "replication")
	add(!plan.Parameters.Equal(prior.Parameters), "postgresql")
	return paths
}

func clusterFromProto(db *serverv1.Database, prior postgresqlClusterModel, catalog []*serverv1.DatabaseProvider) postgresqlClusterModel {
	spec, st := db.GetSpec(), db.GetStatus()
	return postgresqlClusterModel{
		ID:                   types.StringValue(db.GetId()),
		WorkspaceID:          prior.WorkspaceID,
		Name:                 types.StringValue(db.GetName()),
		Version:              types.StringValue(spec.GetVersion()),
		Nodes:                nodesFromProto(spec.GetNodes()),
		PrimaryNodeGroup:     types.StringValue(spec.GetPrimaryNodeGroup()),
		Parameters:           stringMapFromGo(spec.GetPostgresql().GetParameters()),
		Replication:          replicationFromProto(spec.GetReplication()),
		KeepBackupsOnDestroy: prior.KeepBackupsOnDestroy,
		Phase:                types.StringValue(st.GetPhase()),
		PhaseMessage:         types.StringValue(st.GetMessage()),
		EndpointHost:         types.StringValue(st.GetEndpoint().GetHost()),
		EndpointPort:         types.Int64Value(int64(st.GetEndpoint().GetPort())),
		PublicEndpoints:      endpointsFromProto(st.GetPublicEndpoints()),
		PrivateEndpoints:     endpointsFromProto(st.GetPrivateEndpoints()),
		ReadyNodes:           types.Int64Value(int64(st.GetReadyNodes())),
		TotalNodes:           types.Int64Value(int64(st.GetTotalNodes())),
		CreatedAt:            timestampValue(st.GetCreatedAt()),
		UpdatedAt:            timestampValue(st.GetUpdatedAt()),
		Timeouts:             prior.Timeouts,
	}
}
