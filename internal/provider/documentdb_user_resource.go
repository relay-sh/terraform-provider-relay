package provider

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type documentDBUserResource struct {
	client *client.Client
}

type documentDBUserModel struct {
	ID                types.String   `tfsdk:"id"`
	WorkspaceID       types.String   `tfsdk:"workspace_id"`
	ClusterID         types.String   `tfsdk:"cluster_id"`
	Username          types.String   `tfsdk:"username"`
	Password          types.String   `tfsdk:"password"`
	GeneratePassword  types.Bool     `tfsdk:"generate_password"`
	GeneratedPassword types.String   `tfsdk:"generated_password"`
	Role              types.String   `tfsdk:"role"`
	Description       types.String   `tfsdk:"description"`
	Phase             types.String   `tfsdk:"phase"`
	PhaseMessage      types.String   `tfsdk:"phase_message"`
	CreatedAt         types.String   `tfsdk:"created_at"`
	Timeouts          timeouts.Value `tfsdk:"timeouts"`
}

var documentDBUsername = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func newDocumentDBUserResource() resource.Resource {
	return &documentDBUserResource{}
}

func (r *documentDBUserResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_documentdb_user"
}

func (r *documentDBUserResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A password user on a managed DocumentDB cluster, granted on every database. The username cannot change in place; the role and password can. " +
			"`description` can be changed but not cleared. DocumentDB users have no IAM authentication and no expiry.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "`<cluster_id>/<username>`."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace of the cluster."},
			"cluster_id":   schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "`relay_documentdb_cluster` ID."},
			"username": schema.StringAttribute{
				Required: true, PlanModifiers: replace,
				Validators: []validator.String{
					stringvalidator.LengthAtMost(63),
					stringvalidator.RegexMatches(documentDBUsername, "must start with a lowercase letter or underscore and contain only lowercase letters, digits and underscores"),
					stringvalidator.NoneOf("postgres"),
				},
				MarkdownDescription: "User name. `postgres` and names starting with `pg_` are reserved.",
			},
			"password": schema.StringAttribute{
				Optional: true, Sensitive: true,
				MarkdownDescription: "Password. Write-only on the API: changes made outside Terraform are not detected.",
			},
			"generate_password": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplaceIf(
					func(_ context.Context, req planmodifier.BoolRequest, resp *boolplanmodifier.RequiresReplaceIfFuncResponse) {
						resp.RequiresReplace = !req.StateValue.IsNull()
					},
					"Replace when generate_password changes on a user Terraform created; an imported user has no prior value and is not replaced.",
					"Replace when `generate_password` changes on a user Terraform created; an imported user has no prior value and is not replaced.",
				)},
				MarkdownDescription: "Let the platform generate the password. It is returned once in `generated_password`. An imported user's generated password cannot be recovered, so `generated_password` stays empty after an import.",
			},
			"generated_password": schema.StringAttribute{
				Computed: true, Sensitive: true, PlanModifiers: keep,
				MarkdownDescription: "Generated password, stored in state. Not recoverable from the API.",
			},
			"role": schema.StringAttribute{
				Required:   true,
				Validators: []validator.String{stringvalidator.OneOf("readOnly", "admin")},
				MarkdownDescription: "`readOnly` reads every database. `admin` reads and writes every database and administers the cluster; DocumentDB has no write role without administration. " +
					"Changing it updates the user in place.",
			},
			"description":   schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: keep, MarkdownDescription: "Free-text description."},
			"phase":         schema.StringAttribute{Computed: true, MarkdownDescription: "Reconcile phase."},
			"phase_message": schema.StringAttribute{Computed: true, MarkdownDescription: "Detail for the phase."},
			"created_at":    schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true}),
		},
	}
}

func (r *documentDBUserResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var username, password types.String
	var generate types.Bool
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("username"), &username)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("password"), &password)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("generate_password"), &generate)...)
	if strings.HasPrefix(username.ValueString(), "pg_") {
		resp.Diagnostics.AddAttributeError(path.Root("username"), "Reserved username", "Names starting with pg_ belong to the engine's own roles.")
	}
	switch {
	case !password.IsNull() && generate.ValueBool():
		resp.Diagnostics.AddAttributeError(path.Root("generate_password"), "Conflicting password settings", "Set password or generate_password = true, not both.")
	case password.IsNull() && !generate.IsUnknown() && !generate.ValueBool():
		resp.Diagnostics.AddAttributeError(path.Root("password"), "Missing password", "A DocumentDB user needs password or generate_password = true.")
	}
}

func (r *documentDBUserResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *documentDBUserResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan documentDBUserModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	out, err := r.client.API.CreateDatabaseUser(scoped, &serverv1.CreateDatabaseUserRequest{
		DatabaseId:       plan.ClusterID.ValueString(),
		Username:         plan.Username.ValueString(),
		Password:         plan.Password.ValueString(),
		GeneratePassword: plan.GeneratePassword.ValueBool(),
		Description:      plan.Description.ValueString(),
		AuthMethod:       authMethodPassword,
		Roles:            []*serverv1.DatabaseUserRole{{Role: plan.Role.ValueString()}},
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create DocumentDB user", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.ClusterID.ValueString() + "/" + plan.Username.ValueString())
	plan.GeneratedPassword = types.StringValue(out.GetGeneratedPassword())
	for _, attr := range []struct {
		path  string
		value types.String
	}{
		{"id", plan.ID},
		{"generated_password", plan.GeneratedPassword},
		{"workspace_id", plan.WorkspaceID},
		{"cluster_id", plan.ClusterID},
		{"username", plan.Username},
	} {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(attr.path), attr.value)...)
	}

	var user *serverv1.DatabaseUser
	err = waitFor(scoped, timeout, func(ctx context.Context) (bool, error) {
		got, err := r.client.API.GetDatabaseUser(ctx, &serverv1.GetDatabaseUserRequest{DatabaseId: plan.ClusterID.ValueString(), Username: plan.Username.ValueString()})
		if err != nil {
			return false, err
		}
		user = got.GetUser()
		switch strings.ToLower(user.GetPhase()) {
		case "ready":
			return true, nil
		case phaseFailed:
			return false, errors.New(user.GetPhaseMessage())
		}
		return false, nil
	})
	if err != nil {
		resp.Diagnostics.AddError("DocumentDB user did not become ready", err.Error())
		return
	}
	state := documentDBUserFromProto(user, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *documentDBUserResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state documentDBUserModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.GetDatabaseUser(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.GetDatabaseUserRequest{
		DatabaseId: state.ClusterID.ValueString(),
		Username:   state.Username.ValueString(),
	})
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read DocumentDB user", err.Error())
		return
	}
	state = documentDBUserFromProto(out.GetUser(), state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *documentDBUserResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior documentDBUserModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := &serverv1.UpdateDatabaseUserRequest{
		DatabaseId:  plan.ClusterID.ValueString(),
		Username:    plan.Username.ValueString(),
		Description: stringOrNil(plan.Description),
	}
	if !plan.Password.Equal(prior.Password) {
		update.Password = stringPtr(plan.Password.ValueString())
	}
	if !plan.Role.Equal(prior.Role) {
		update.Roles = []*serverv1.DatabaseUserRole{{Role: plan.Role.ValueString()}}
	}
	out, err := r.client.API.UpdateDatabaseUser(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), update)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update DocumentDB user", err.Error())
		return
	}
	plan.Phase, plan.PhaseMessage = prior.Phase, prior.PhaseMessage
	state := documentDBUserFromProto(out.GetUser(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *documentDBUserResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state documentDBUserModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.DeleteDatabaseUser(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.DeleteDatabaseUserRequest{
		DatabaseId: state.ClusterID.ValueString(),
		Username:   state.Username.ValueString(),
	})
	if err != nil && !relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot delete DocumentDB user", err.Error())
	}
}

func (r *documentDBUserResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cluster_id>/<username>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("username"), parts[2])...)
}

func documentDBUserFromProto(u *serverv1.DatabaseUser, prior documentDBUserModel) documentDBUserModel {
	out := prior
	out.ID = types.StringValue(prior.ClusterID.ValueString() + "/" + u.GetUsername())
	out.Username = types.StringValue(u.GetUsername())
	out.Description = types.StringValue(u.GetDescription())
	if roles := u.GetRoles(); len(roles) > 0 {
		out.Role = types.StringValue(roles[0].GetRole())
	}
	if u.GetPhase() != "" {
		out.Phase = types.StringValue(u.GetPhase())
		out.PhaseMessage = types.StringValue(u.GetPhaseMessage())
	}
	if out.Phase.IsNull() || out.Phase.IsUnknown() {
		out.Phase, out.PhaseMessage = types.StringValue(""), types.StringValue("")
	}
	if created := u.GetCreatedAt(); created.IsValid() && created.GetSeconds() > 0 {
		out.CreatedAt = types.StringValue(created.AsTime().UTC().Format(time.RFC3339))
	} else if out.CreatedAt.IsNull() || out.CreatedAt.IsUnknown() {
		out.CreatedAt = types.StringValue("")
	}
	if out.GeneratedPassword.IsNull() || out.GeneratedPassword.IsUnknown() {
		out.GeneratedPassword = types.StringValue("")
	}
	return out
}
