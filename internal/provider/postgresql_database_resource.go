package provider

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type postgresqlDatabaseResource struct {
	client *client.Client
}

type postgresqlDatabaseModel struct {
	ID           types.String   `tfsdk:"id"`
	WorkspaceID  types.String   `tfsdk:"workspace_id"`
	ClusterID    types.String   `tfsdk:"cluster_id"`
	Name         types.String   `tfsdk:"name"`
	Owner        types.String   `tfsdk:"owner"`
	Description  types.String   `tfsdk:"description"`
	Origin       types.String   `tfsdk:"origin"`
	Phase        types.String   `tfsdk:"phase"`
	PhaseMessage types.String   `tfsdk:"phase_message"`
	CreatedAt    types.String   `tfsdk:"created_at"`
	Timeouts     timeouts.Value `tfsdk:"timeouts"`
}

var errLogicalDatabaseNotFound = errors.New("logical database not found")

func newPostgreSQLDatabaseResource() resource.Resource {
	return &postgresqlDatabaseResource{}
}

func (r *postgresqlDatabaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_postgresql_database"
}

func (r *postgresqlDatabaseResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	nonEmpty := []validator.String{stringvalidator.LengthAtLeast(1)}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A logical database on a managed PostgreSQL cluster. `readOnly` and `readWrite` roles of `relay_postgresql_user` apply to one, so create it before the user. The name cannot change in place; `owner` and `description` can, and removing them clears them. A database the platform adopted rather than created is kept on the cluster when the resource is destroyed.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "`<cluster_id>/<name>`."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace of the cluster."},
			"cluster_id":   schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "`relay_postgresql_cluster` ID."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, Validators: nonEmpty, MarkdownDescription: "Database name. Reserved names such as `postgres` are refused."},
			"owner": schema.StringAttribute{
				Optional: true, Validators: nonEmpty,
				MarkdownDescription: "Username of the `relay_postgresql_user` that owns the database. Unset, the platform's superuser owns it.",
			},
			"description":   schema.StringAttribute{Optional: true, Validators: nonEmpty, MarkdownDescription: "Free-text description."},
			"origin":        schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "`Created` when the platform created the database, `Adopted` when it took over an existing one."},
			"phase":         schema.StringAttribute{Computed: true, MarkdownDescription: "Reconcile phase."},
			"phase_message": schema.StringAttribute{Computed: true, MarkdownDescription: "Detail for the phase."},
			"created_at":    schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true}),
		},
	}
}

func (r *postgresqlDatabaseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *postgresqlDatabaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan postgresqlDatabaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	_, err := r.client.API.CreateLogicalDatabase(scoped, &serverv1.CreateLogicalDatabaseRequest{
		DatabaseId:  plan.ClusterID.ValueString(),
		Name:        plan.Name.ValueString(),
		Owner:       plan.Owner.ValueString(),
		Description: plan.Description.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create PostgreSQL database", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.ClusterID.ValueString() + "/" + plan.Name.ValueString())
	for _, attr := range []struct {
		path  string
		value types.String
	}{
		{"id", plan.ID},
		{"workspace_id", plan.WorkspaceID},
		{"cluster_id", plan.ClusterID},
		{"name", plan.Name},
	} {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(attr.path), attr.value)...)
	}

	var db *serverv1.LogicalDatabase
	err = waitFor(scoped, timeout, func(ctx context.Context) (bool, error) {
		got, err := r.findLogicalDatabase(ctx, plan.ClusterID.ValueString(), plan.Name.ValueString())
		if errors.Is(err, errLogicalDatabaseNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		db = got
		switch strings.ToLower(db.GetPhase()) {
		case "ready":
			return true, nil
		case phaseFailed:
			return false, errors.New(db.GetPhaseMessage())
		}
		return false, nil
	})
	if err != nil {
		resp.Diagnostics.AddError("PostgreSQL database did not become ready", err.Error())
		return
	}
	state := logicalDatabaseFromProto(db, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *postgresqlDatabaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state postgresqlDatabaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	db, err := r.findLogicalDatabase(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), state.ClusterID.ValueString(), state.Name.ValueString())
	if relay.IsNotFound(err) || errors.Is(err, errLogicalDatabaseNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read PostgreSQL database", err.Error())
		return
	}
	state = logicalDatabaseFromProto(db, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *postgresqlDatabaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior postgresqlDatabaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := &serverv1.UpdateLogicalDatabaseRequest{
		DatabaseId: plan.ClusterID.ValueString(),
		Name:       plan.Name.ValueString(),
	}
	if !plan.Owner.Equal(prior.Owner) {
		update.Owner = stringPtr(plan.Owner.ValueString())
	}
	if !plan.Description.Equal(prior.Description) {
		update.Description = stringPtr(plan.Description.ValueString())
	}
	out, err := r.client.API.UpdateLogicalDatabase(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), update)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update PostgreSQL database", err.Error())
		return
	}
	state := logicalDatabaseFromProto(out.GetDatabase(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *postgresqlDatabaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state postgresqlDatabaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.DeleteLogicalDatabase(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.DeleteLogicalDatabaseRequest{
		DatabaseId: state.ClusterID.ValueString(),
		Name:       state.Name.ValueString(),
	})
	if err != nil && !relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot delete PostgreSQL database", err.Error())
	}
}

func (r *postgresqlDatabaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cluster_id>/<name>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[2])...)
}

func (r *postgresqlDatabaseResource) findLogicalDatabase(ctx context.Context, clusterID, name string) (*serverv1.LogicalDatabase, error) {
	out, err := r.client.API.ListLogicalDatabases(ctx, &serverv1.ListLogicalDatabasesRequest{DatabaseId: clusterID})
	if err != nil {
		return nil, err
	}
	for _, db := range out.GetDatabases() {
		if db.GetName() == name {
			return db, nil
		}
	}
	return nil, errLogicalDatabaseNotFound
}

func logicalDatabaseFromProto(db *serverv1.LogicalDatabase, prior postgresqlDatabaseModel) postgresqlDatabaseModel {
	out := prior
	out.ID = types.StringValue(prior.ClusterID.ValueString() + "/" + db.GetName())
	out.Name = types.StringValue(db.GetName())
	out.Owner = optionalString(db.GetOwner())
	out.Description = optionalString(db.GetDescription())
	out.Origin = types.StringValue(db.GetOrigin())
	out.Phase = types.StringValue(db.GetPhase())
	out.PhaseMessage = types.StringValue(db.GetPhaseMessage())
	if created := timestampValue(db.GetCreatedAt()); created.ValueString() != "" || out.CreatedAt.IsNull() || out.CreatedAt.IsUnknown() {
		out.CreatedAt = created
	}
	return out
}
