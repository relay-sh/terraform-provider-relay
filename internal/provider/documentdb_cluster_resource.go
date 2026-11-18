package provider

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const (
	engineDocumentDB   = "documentdb"
	documentDBRoleData = "data"
)

var (
	documentDBVersions         = []string{"17", "18"}
	documentDBTier             = regexp.MustCompile(`^R[0-9]+$`)
	documentDBStorageAttrTypes = map[string]attr.Type{"size": types.StringType}
)

type documentDBClusterResource struct {
	client *client.Client
}

type documentDBNodeGroupModel struct {
	Count     types.Int64          `tfsdk:"count"`
	Cloud     types.String         `tfsdk:"cloud"`
	Region    types.String         `tfsdk:"region"`
	Tier      types.String         `tfsdk:"tier"`
	Resources valkeyResourcesModel `tfsdk:"resources"`
}

type documentDBClusterModel struct {
	ID                 types.String                        `tfsdk:"id"`
	WorkspaceID        types.String                        `tfsdk:"workspace_id"`
	Name               types.String                        `tfsdk:"name"`
	Version            types.String                        `tfsdk:"version"`
	Storage            types.Object                        `tfsdk:"storage"`
	Nodes              map[string]documentDBNodeGroupModel `tfsdk:"nodes"`
	DeletionProtection types.Bool                          `tfsdk:"deletion_protection"`
	DefaultConnections types.Object                        `tfsdk:"default_connections"`
	Phase              types.String                        `tfsdk:"phase"`
	PhaseMessage       types.String                        `tfsdk:"phase_message"`
	EndpointHost       types.String                        `tfsdk:"endpoint_host"`
	EndpointPort       types.Int64                         `tfsdk:"endpoint_port"`
	PublicEndpoints    types.List                          `tfsdk:"public_endpoints"`
	ReadyNodes         types.Int64                         `tfsdk:"ready_nodes"`
	TotalNodes         types.Int64                         `tfsdk:"total_nodes"`
	CreatedAt          types.String                        `tfsdk:"created_at"`
	UpdatedAt          types.String                        `tfsdk:"updated_at"`
	Timeouts           timeouts.Value                      `tfsdk:"timeouts"`
}

func newDocumentDBClusterResource() resource.Resource {
	return &documentDBClusterResource{}
}

func (r *documentDBClusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_documentdb_cluster"
}

func (r *documentDBClusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keepString := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed DocumentDB cluster, MongoDB-compatible on port 27017 with TLS. Creating, updating and destroying wait for the cluster to settle. " +
			"Changing `name`, `workspace_id` or `version` replaces the cluster and its data; use `lifecycle { prevent_destroy = true }`. " +
			"DocumentDB has no backups, restore, parameters, storage autoscaling, compute autoscaling or private endpoints yet, and is not available on the Free plan.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "Cluster ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the cluster belongs to."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Cluster name, unique in the workspace."},
			"version": schema.StringAttribute{
				Optional: true, Computed: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.OneOf(documentDBVersions...)},
				MarkdownDescription: "DocumentDB version, `17` or `18`. Omit it and the platform picks one, which the next read reports. " +
					"There is no in-place upgrade: changing it replaces the cluster and its data.",
			},
			"storage": schema.SingleNestedAttribute{
				Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Volume size of every node: each node holds the whole data set. Omit the block and the platform picks the size included with the tier, which the next read reports.",
				Attributes: map[string]schema.Attribute{
					"size": schema.StringAttribute{
						Optional: true, Computed: true,
						PlanModifiers:       keepString,
						MarkdownDescription: "Volume size, Kubernetes quantity, for example `20Gi`. It can only grow; a plan that shrinks it fails.",
					},
				},
			},
			"nodes": schema.MapNestedAttribute{
				Required: true,
				Validators: []validator.Map{
					mapvalidator.SizeBetween(1, 1),
					mapvalidator.KeysAre(stringvalidator.LengthAtMost(30), stringvalidator.RegexMatches(valkeyGroupName, "must be lowercase letters, digits and hyphens")),
				},
				MarkdownDescription: "The cluster's single node group, keyed by a label you choose: one primary and `count - 1` replicas. Renaming it replaces the cluster.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"count": schema.Int64Attribute{
						Optional: true, Computed: true, Default: int64default.StaticInt64(1),
						Validators:          []validator.Int64{int64validator.AtLeast(1)},
						MarkdownDescription: "Nodes in the group: one primary and `count - 1` replicas serving the read-only endpoint.",
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
						Validators:          []validator.String{stringvalidator.RegexMatches(documentDBTier, "must be a general machine tier such as R20; the RC cache tiers are for Valkey")},
						MarkdownDescription: "Machine catalog name, for example `R20`. CPU and memory are derived from it; an unknown name is rejected. Changing it restarts the group's pods.",
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
			"deletion_protection": deletionProtectionAttribute("cluster"),
			"default_connections": documentDBDefaultConnectionsAttribute(),
			"phase":               schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle phase after the last apply."},
			"phase_message":       schema.StringAttribute{Computed: true, MarkdownDescription: "Detail for the phase."},
			"endpoint_host":       schema.StringAttribute{Computed: true, MarkdownDescription: "Public read-write endpoint host, the same as the `rw` entry of `public_endpoints`. Empty until the platform publishes it."},
			"endpoint_port":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Public read-write endpoint port, 27017."},
			"public_endpoints": schema.ListNestedAttribute{
				Computed: true, NestedObject: endpoint,
				MarkdownDescription: "Public endpoints by role: `rw` for the primary and `ro`, the `<name>-ro` host, for the replicas. Clients connect with `mongodb://<user>:<password>@<host>:27017/?tls=true`.",
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

func (r *documentDBClusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *documentDBClusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	var plan, prior documentDBClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	rejectShrink(plan.Storage, prior.Storage, &resp.Diagnostics)
	changed := false
	for name, planned := range plan.Nodes {
		current, ok := prior.Nodes[name]
		if ok && (planned.Tier.IsUnknown() || planned.Tier.Equal(current.Tier)) {
			continue
		}
		if !ok {
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root("nodes"))
		}
		planned.Resources.CPU = types.StringUnknown()
		planned.Resources.Memory = types.StringUnknown()
		plan.Nodes[name] = planned
		changed = true
	}
	if changed {
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
	}
}

func (r *documentDBClusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan documentDBClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 30*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	out, err := r.client.API.CreateDatabase(scoped, &serverv1.CreateDatabaseRequest{
		Name: plan.Name.ValueString(), Spec: documentDBSpecToProto(plan), DefaultConnections: defaultConnectionsToProto(plan.DefaultConnections),
		DeletionProtection: plan.DeletionProtection.ValueBool(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create cluster", err.Error())
		return
	}
	id := out.GetDatabase().GetId()
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), plan.WorkspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), plan.Name)...)

	db, err := waitDatabaseRunning(scoped, r.client, id, timeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not become ready", err.Error())
		return
	}
	state := documentDBClusterFromProto(db, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *documentDBClusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state documentDBClusterModel
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
	if actual := strings.ToLower(out.GetDatabase().GetSpec().GetEngine()); actual != engineDocumentDB {
		resp.Diagnostics.AddError("Wrong cluster engine", fmt.Sprintf("Cluster %q is a %s cluster; manage it with relay_%s_cluster.", state.ID.ValueString(), actual, actual))
		return
	}
	state = documentDBClusterFromProto(out.GetDatabase(), state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *documentDBClusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior documentDBClusterModel
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
	if paths := documentDBUpdateMask(plan, prior); len(paths) > 0 {
		if _, err := r.client.API.UpdateDatabase(scoped, &serverv1.UpdateDatabaseRequest{
			Id:         plan.ID.ValueString(),
			Spec:       documentDBSpecToProto(plan),
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
	db, err := waitDatabaseRunning(scoped, r.client, plan.ID.ValueString(), timeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not settle after update", err.Error())
		return
	}
	state := documentDBClusterFromProto(db, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *documentDBClusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state documentDBClusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	timeout, diags := state.Timeouts.Delete(ctx, 20*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if refuseProtectedDelete(state.DeletionProtection, &resp.Diagnostics) {
		return
	}
	deleteDatabase(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), r.client, state.ID.ValueString(), false, timeout, &resp.Diagnostics)
}

func (r *documentDBClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	workspaceID, id, ok := strings.Cut(req.ID, "/")
	if !ok || workspaceID == "" || id == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cluster_id>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), workspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

func documentDBSpecToProto(m documentDBClusterModel) *serverv1.DatabaseSpec {
	spec := &serverv1.DatabaseSpec{
		Engine:  engineDocumentDB,
		Version: m.Version.ValueString(),
		Storage: documentDBStorageToProto(m.Storage),
	}
	for name, n := range m.Nodes {
		spec.PrimaryNodeGroup = name
		spec.Nodes = append(spec.Nodes, &serverv1.NodeSpec{
			Name:      name,
			Role:      documentDBRoleData,
			Count:     int32(n.Count.ValueInt64()),
			Cloud:     n.Cloud.ValueString(),
			Region:    n.Region.ValueString(),
			Tier:      n.Tier.ValueString(),
			Resources: &serverv1.ResourceRequirements{},
		})
	}
	return spec
}

func documentDBUpdateMask(plan, prior documentDBClusterModel) []string {
	var paths []string
	if !reflect.DeepEqual(plan.Nodes, prior.Nodes) {
		paths = append(paths, "nodes")
	}
	if !plan.Storage.Equal(prior.Storage) {
		paths = append(paths, "storage")
	}
	return paths
}

func documentDBStorageToProto(o types.Object) *serverv1.ClusterStorage {
	size := storageSizeOf(o)
	if size.IsNull() || size.IsUnknown() {
		return nil
	}
	return &serverv1.ClusterStorage{Size: size.ValueString()}
}

func documentDBStorageFromProto(s *serverv1.ClusterStorage) types.Object {
	if s == nil {
		return types.ObjectNull(documentDBStorageAttrTypes)
	}
	return types.ObjectValueMust(documentDBStorageAttrTypes, map[string]attr.Value{"size": types.StringValue(s.GetSize())})
}

func documentDBNodesFromProto(nodes []*serverv1.NodeSpec) map[string]documentDBNodeGroupModel {
	out := make(map[string]documentDBNodeGroupModel, len(nodes))
	for _, n := range nodes {
		res := n.GetResources()
		out[n.GetName()] = documentDBNodeGroupModel{
			Count:     types.Int64Value(int64(n.GetCount())),
			Cloud:     types.StringValue(n.GetCloud()),
			Region:    types.StringValue(n.GetRegion()),
			Tier:      types.StringValue(n.GetTier()),
			Resources: valkeyResourcesModel{CPU: types.StringValue(res.GetCpu()), Memory: types.StringValue(res.GetMemory())},
		}
	}
	return out
}

func documentDBClusterFromProto(db *serverv1.Database, prior documentDBClusterModel) documentDBClusterModel {
	spec, st := db.GetSpec(), db.GetStatus()
	endpointHost, endpointPort := primaryEndpointFromProto(st)
	return documentDBClusterModel{
		ID:                 types.StringValue(db.GetId()),
		WorkspaceID:        prior.WorkspaceID,
		Name:               types.StringValue(db.GetName()),
		Version:            types.StringValue(spec.GetVersion()),
		Storage:            documentDBStorageFromProto(spec.GetStorage()),
		Nodes:              documentDBNodesFromProto(spec.GetNodes()),
		DeletionProtection: types.BoolValue(db.GetDeletionProtection()),
		DefaultConnections: documentDBDefaultConnections.fromProto(db.GetDefaultConnections(), prior.DefaultConnections),
		Phase:              types.StringValue(st.GetPhase()),
		PhaseMessage:       types.StringValue(st.GetMessage()),
		EndpointHost:       endpointHost,
		EndpointPort:       endpointPort,
		PublicEndpoints:    endpointsFromProto(st.GetPublicEndpoints()),
		ReadyNodes:         types.Int64Value(int64(st.GetReadyNodes())),
		TotalNodes:         types.Int64Value(int64(st.GetTotalNodes())),
		CreatedAt:          timestampValue(st.GetCreatedAt()),
		UpdatedAt:          timestampValue(st.GetUpdatedAt()),
		Timeouts:           prior.Timeouts,
	}
}
