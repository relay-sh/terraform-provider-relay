package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
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

type databaseResource struct {
	client *client.Client
}

type databaseResourceModel struct {
	ID                   types.String             `tfsdk:"id"`
	WorkspaceID          types.String             `tfsdk:"workspace_id"`
	Name                 types.String             `tfsdk:"name"`
	Engine               types.String             `tfsdk:"engine"`
	Version              types.String             `tfsdk:"version"`
	Nodes                []nodeModel              `tfsdk:"nodes"`
	Params               types.Map                `tfsdk:"params"`
	PostgreSQLParameters types.Map                `tfsdk:"postgresql_parameters"`
	BackupsEnabled       types.Bool               `tfsdk:"backups_enabled"`
	Replication          types.Object             `tfsdk:"replication"`
	DefaultConnections   *defaultConnectionsModel `tfsdk:"default_connections"`
	KeepBackupsOnDestroy types.Bool               `tfsdk:"keep_backups_on_destroy"`
	Phase                types.String             `tfsdk:"phase"`
	PhaseMessage         types.String             `tfsdk:"phase_message"`
	EndpointHost         types.String             `tfsdk:"endpoint_host"`
	EndpointPort         types.Int64              `tfsdk:"endpoint_port"`
	PublicEndpoints      types.List               `tfsdk:"public_endpoints"`
	PrivateEndpoints     types.List               `tfsdk:"private_endpoints"`
	ReadyNodes           types.Int64              `tfsdk:"ready_nodes"`
	TotalNodes           types.Int64              `tfsdk:"total_nodes"`
	Timeouts             timeouts.Value           `tfsdk:"timeouts"`
}

type defaultConnectionsModel struct {
	Readonly  types.Bool `tfsdk:"readonly"`
	Readwrite types.Bool `tfsdk:"readwrite"`
	Admin     types.Bool `tfsdk:"admin"`
}

func newDatabaseResource() resource.Resource {
	return &databaseResource{}
}

func (r *databaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database"
}

func (r *databaseResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keepString := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	keepInt := []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}
	endpoint := schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
		"role": schema.StringAttribute{Computed: true},
		"host": schema.StringAttribute{Computed: true},
		"port": schema.Int64Attribute{Computed: true},
	}}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed database cluster. Creating, updating and destroying wait for the cluster to settle. " +
			"Changing `name`, `engine` or `workspace_id` replaces the cluster and its data; use `lifecycle { prevent_destroy = true }`.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keepString, MarkdownDescription: "Database ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the cluster belongs to."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Cluster name, unique in the workspace."},
			"engine": schema.StringAttribute{
				Required: true, PlanModifiers: replace,
				Validators:          []validator.String{stringvalidator.OneOf("postgresql", "mongodb")},
				MarkdownDescription: "`postgresql` or `mongodb`.",
			},
			"version": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Engine version. It cannot change here: major upgrades go through Relay's upgrade flow, and a plan that changes it fails.",
			},
			"nodes": schema.ListNestedAttribute{
				Required:            true,
				MarkdownDescription: "Node groups. Storage can only grow.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"role":     schema.StringAttribute{Required: true, MarkdownDescription: "Node role, for example `data`."},
					"count":    schema.Int64Attribute{Required: true, MarkdownDescription: "Number of nodes."},
					"provider": schema.StringAttribute{Required: true, MarkdownDescription: "Infrastructure provider."},
					"region":   schema.StringAttribute{Required: true, MarkdownDescription: "Provider region."},
					"resources": schema.SingleNestedAttribute{
						Required: true,
						Attributes: map[string]schema.Attribute{
							"cpu":           schema.StringAttribute{Required: true, MarkdownDescription: "CPU request, Kubernetes quantity."},
							"memory":        schema.StringAttribute{Required: true, MarkdownDescription: "Memory request, Kubernetes quantity."},
							"storage":       schema.StringAttribute{Required: true, MarkdownDescription: "Volume size, Kubernetes quantity."},
							"storage_class": schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: keepString, MarkdownDescription: "Storage class. Defaults to the platform's."},
							"iops":          schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: keepInt, MarkdownDescription: "Provisioned IOPS."},
							"throughput":    schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: keepInt, MarkdownDescription: "Provisioned throughput."},
						},
					},
					"tags": schema.MapAttribute{
						ElementType: types.StringType, Optional: true, Computed: true,
						PlanModifiers:       []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
						MarkdownDescription: "Engine-specific labels for every node in the group.",
					},
				}},
			},
			"params": schema.MapAttribute{
				ElementType: types.StringType, Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Engine parameters.",
			},
			"postgresql_parameters": schema.MapAttribute{
				ElementType: types.StringType, Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "PostgreSQL settings (GUCs). An empty map resets them to defaults; omitting the attribute leaves them unmanaged.",
			},
			"backups_enabled": schema.BoolAttribute{
				Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Scheduled backups.",
			},
			"replication": schema.SingleNestedAttribute{
				Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "PostgreSQL replication tuning.",
				Attributes: map[string]schema.Attribute{
					"mode":                 schema.StringAttribute{Required: true, MarkdownDescription: "`async` or `synchronous`."},
					"synchronous_replicas": schema.Int64Attribute{Optional: true, MarkdownDescription: "Synchronous replicas when `mode` is `synchronous`."},
				},
			},
			"default_connections": schema.SingleNestedAttribute{
				Optional:            true,
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.RequiresReplace()},
				MarkdownDescription: "Relay-managed users and connections seeded once the cluster is ready. Create-only.",
				Attributes: map[string]schema.Attribute{
					"readonly":  schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false)},
					"readwrite": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false)},
					"admin":     schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false)},
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
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

func (r *databaseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *databaseResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var planned, current types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("version"), &planned)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("version"), &current)...)
	if !planned.IsUnknown() && !planned.Equal(current) {
		resp.Diagnostics.AddAttributeError(path.Root("version"), "Version cannot change here",
			"Upgrading from "+current.ValueString()+" to "+planned.ValueString()+" goes through Relay's upgrade flow. Run the upgrade, then set version to the new value.")
	}
}

func (r *databaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan databaseResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 30*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	create := &serverv1.CreateDatabaseRequest{Name: plan.Name.ValueString(), Spec: databaseSpecToProto(plan)}
	if dc := plan.DefaultConnections; dc != nil {
		create.DefaultConnections = &serverv1.DefaultConnectionsIntent{Readonly: dc.Readonly.ValueBool(), Readwrite: dc.Readwrite.ValueBool(), Admin: dc.Admin.ValueBool()}
	}
	out, err := r.client.API.CreateDatabase(scoped, create)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create database", err.Error())
		return
	}
	id := out.GetDatabase().GetId()
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)

	db, err := r.waitRunning(scoped, id, timeout)
	if err != nil {
		resp.Diagnostics.AddError("Database did not become ready", err.Error())
		return
	}
	state := databaseFromProto(db, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state databaseResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.GetDatabase(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.GetDatabaseRequest{Id: state.ID.ValueString()})
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read database", err.Error())
		return
	}
	if phase := out.GetDatabase().GetStatus().GetPhase(); phase == "deleting" || phase == phaseDeleted {
		resp.State.RemoveResource(ctx)
		return
	}
	state = databaseFromProto(out.GetDatabase(), state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior databaseResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	timeout, diags := plan.Timeouts.Update(ctx, 30*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	if paths := databaseUpdateMask(plan, prior); len(paths) > 0 {
		_, err := r.client.API.UpdateDatabase(scoped, &serverv1.UpdateDatabaseRequest{
			Id:         plan.ID.ValueString(),
			Spec:       databaseSpecToProto(plan),
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		})
		if err != nil {
			resp.Diagnostics.AddError("Cannot update database", err.Error())
			return
		}
	}
	db, err := r.waitRunning(scoped, plan.ID.ValueString(), timeout)
	if err != nil {
		resp.Diagnostics.AddError("Database did not settle after update", err.Error())
		return
	}
	state := databaseFromProto(db, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state databaseResourceModel
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
		resp.Diagnostics.AddError("Cannot delete database", err.Error())
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
		resp.Diagnostics.AddError("Database was not deleted", err.Error())
	}
}

func (r *databaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	workspaceID, id, ok := strings.Cut(req.ID, "/")
	if !ok || workspaceID == "" || id == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<database_id>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), workspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("keep_backups_on_destroy"), false)...)
}

func (r *databaseResource) waitRunning(ctx context.Context, id string, timeout time.Duration) (*serverv1.Database, error) {
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

func databaseSpecToProto(m databaseResourceModel) *serverv1.DatabaseSpec {
	spec := &serverv1.DatabaseSpec{
		Engine:         m.Engine.ValueString(),
		Version:        m.Version.ValueString(),
		Topology:       "replication",
		Nodes:          nodesToProto(m.Nodes),
		Params:         stringMapToGo(m.Params),
		BackupsEnabled: m.BackupsEnabled.ValueBool(),
		Replication:    replicationToProto(m.Replication),
	}
	if !m.PostgreSQLParameters.IsNull() && !m.PostgreSQLParameters.IsUnknown() {
		spec.Postgresql = &serverv1.PostgreSQLConfig{Parameters: stringMapToGo(m.PostgreSQLParameters)}
	}
	return spec
}

func databaseUpdateMask(plan, prior databaseResourceModel) []string {
	var paths []string
	add := func(changed bool, p string) {
		if changed {
			paths = append(paths, p)
		}
	}
	add(!reflect.DeepEqual(plan.Nodes, prior.Nodes), "nodes")
	add(!plan.Params.Equal(prior.Params), "params")
	add(!plan.BackupsEnabled.Equal(prior.BackupsEnabled), "backups_enabled")
	add(!plan.Replication.Equal(prior.Replication), "replication")
	add(!plan.PostgreSQLParameters.Equal(prior.PostgreSQLParameters), "postgresql")
	return paths
}

func databaseFromProto(db *serverv1.Database, prior databaseResourceModel) databaseResourceModel {
	spec, st := db.GetSpec(), db.GetStatus()
	return databaseResourceModel{
		ID:                   types.StringValue(db.GetId()),
		WorkspaceID:          prior.WorkspaceID,
		Name:                 types.StringValue(db.GetName()),
		Engine:               types.StringValue(spec.GetEngine()),
		Version:              types.StringValue(spec.GetVersion()),
		Nodes:                nodesFromProto(spec.GetNodes()),
		Params:               stringMapFromGo(spec.GetParams()),
		PostgreSQLParameters: stringMapFromGo(spec.GetPostgresql().GetParameters()),
		BackupsEnabled:       types.BoolValue(spec.GetBackupsEnabled()),
		Replication:          replicationFromProto(spec.GetReplication()),
		DefaultConnections:   prior.DefaultConnections,
		KeepBackupsOnDestroy: prior.KeepBackupsOnDestroy,
		Phase:                types.StringValue(st.GetPhase()),
		PhaseMessage:         types.StringValue(st.GetMessage()),
		EndpointHost:         types.StringValue(st.GetEndpoint().GetHost()),
		EndpointPort:         types.Int64Value(int64(st.GetEndpoint().GetPort())),
		PublicEndpoints:      endpointsFromProto(st.GetPublicEndpoints()),
		PrivateEndpoints:     endpointsFromProto(st.GetPrivateEndpoints()),
		ReadyNodes:           types.Int64Value(int64(st.GetReadyNodes())),
		TotalNodes:           types.Int64Value(int64(st.GetTotalNodes())),
		Timeouts:             prior.Timeouts,
	}
}
