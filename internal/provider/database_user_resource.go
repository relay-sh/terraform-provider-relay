package provider

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type databaseUserResource struct {
	client *client.Client
}

type databaseUserModel struct {
	ID                  types.String    `tfsdk:"id"`
	WorkspaceID         types.String    `tfsdk:"workspace_id"`
	DatabaseID          types.String    `tfsdk:"database_id"`
	Username            types.String    `tfsdk:"username"`
	Password            types.String    `tfsdk:"password"`
	GeneratePassword    types.Bool      `tfsdk:"generate_password"`
	GeneratedPassword   types.String    `tfsdk:"generated_password"`
	Description         types.String    `tfsdk:"description"`
	AuthMethod          types.String    `tfsdk:"auth_method"`
	Roles               []userRoleModel `tfsdk:"roles"`
	RestrictedResources types.List      `tfsdk:"restricted_resources"`
	ExpiresAt           types.String    `tfsdk:"expires_at"`
	ConnectionLimit     types.Int64     `tfsdk:"connection_limit"`
	StatementTimeout    types.String    `tfsdk:"statement_timeout"`
	Phase               types.String    `tfsdk:"phase"`
	PhaseMessage        types.String    `tfsdk:"phase_message"`
	Stale               types.Bool      `tfsdk:"stale"`
	CreatedAt           types.String    `tfsdk:"created_at"`
	Timeouts            timeouts.Value  `tfsdk:"timeouts"`
}

type userRoleModel struct {
	Role       types.String `tfsdk:"role"`
	DB         types.String `tfsdk:"db"`
	Collection types.String `tfsdk:"collection"`
}

func newDatabaseUserResource() resource.Resource {
	return &databaseUserResource{}
}

func (r *databaseUserResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database_user"
}

func (r *databaseUserResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "An engine-level user on a managed database. Roles, databases and the auth method cannot change in place; " +
			"changing them replaces the user. `description`, `connection_limit` and `statement_timeout` can be changed but not cleared.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "`<database_id>/<username>`."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace of the database."},
			"database_id":  schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Managed database ID."},
			"username":     schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Engine username."},
			"password": schema.StringAttribute{
				Optional: true, Sensitive: true,
				MarkdownDescription: "Password. Write-only on the API: changes made outside Terraform are not detected.",
			},
			"generate_password": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
				MarkdownDescription: "Let the platform generate the password. It is returned once in `generated_password`.",
			},
			"generated_password": schema.StringAttribute{
				Computed: true, Sensitive: true, PlanModifiers: keep,
				MarkdownDescription: "Generated password, stored in state. Not recoverable from the API.",
			},
			"description": schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: keep, MarkdownDescription: "Free-text description."},
			"auth_method": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("password"),
				PlanModifiers:       replace,
				Validators:          []validator.String{stringvalidator.OneOf("password")},
				MarkdownDescription: "Authentication method. Only `password` is supported.",
			},
			"roles": schema.ListNestedAttribute{
				Required:            true,
				PlanModifiers:       []planmodifier.List{listplanmodifier.RequiresReplace()},
				MarkdownDescription: "Roles granted to the user.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"role":       schema.StringAttribute{Required: true, MarkdownDescription: "Role name, for example `readOnly` or `readWrite`."},
					"db":         schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Database the role applies to."},
					"collection": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "MongoDB collection the role applies to."},
				}},
			},
			"restricted_resources": schema.ListAttribute{
				ElementType: types.StringType, Optional: true,
				PlanModifiers:       []planmodifier.List{listplanmodifier.RequiresReplace()},
				MarkdownDescription: "Resources the user is restricted to.",
			},
			"expires_at": schema.StringAttribute{Optional: true, MarkdownDescription: "RFC 3339 expiry. Removing it makes the user permanent."},
			"connection_limit": schema.Int64Attribute{
				Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
				MarkdownDescription: "Maximum concurrent connections.",
			},
			"statement_timeout": schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: keep, MarkdownDescription: "Statement timeout, for example `30s`."},
			"phase":             schema.StringAttribute{Computed: true, MarkdownDescription: "Reconcile phase."},
			"phase_message":     schema.StringAttribute{Computed: true, MarkdownDescription: "Detail for the phase."},
			"stale":             schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the platform considers the user stale."},
			"created_at":        schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true}),
		},
	}
}

func (r *databaseUserResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var password types.String
	var generate types.Bool
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("password"), &password)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("generate_password"), &generate)...)
	if !password.IsNull() && generate.ValueBool() {
		resp.Diagnostics.AddAttributeError(path.Root("generate_password"), "Conflicting password settings", "Set password or generate_password = true, not both.")
	}
}

func (r *databaseUserResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *databaseUserResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan databaseUserModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)
	expires, err := parseExpiry(plan.ExpiresAt)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("expires_at"), "Invalid expires_at", err.Error())
	}
	if resp.Diagnostics.HasError() {
		return
	}

	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	create := &serverv1.CreateDatabaseUserRequest{
		DatabaseId:          plan.DatabaseID.ValueString(),
		Username:            plan.Username.ValueString(),
		Password:            plan.Password.ValueString(),
		GeneratePassword:    plan.GeneratePassword.ValueBool(),
		Description:         plan.Description.ValueString(),
		AuthMethod:          plan.AuthMethod.ValueString(),
		Roles:               rolesToProto(plan.Roles),
		RestrictedResources: stringListToGo(plan.RestrictedResources),
		ExpiresAt:           expires,
		StatementTimeout:    stringOrNil(plan.StatementTimeout),
	}
	if v := plan.ConnectionLimit; !v.IsNull() && !v.IsUnknown() {
		create.ConnectionLimit = int32Ptr(v.ValueInt64())
	}
	out, err := r.client.API.CreateDatabaseUser(scoped, create)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create database user", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.DatabaseID.ValueString() + "/" + plan.Username.ValueString())
	plan.GeneratedPassword = types.StringValue(out.GetGeneratedPassword())
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), plan.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("generated_password"), plan.GeneratedPassword)...)

	var user *serverv1.DatabaseUser
	err = waitFor(scoped, timeout, func(ctx context.Context) (bool, error) {
		got, err := r.client.API.GetDatabaseUser(ctx, &serverv1.GetDatabaseUserRequest{DatabaseId: plan.DatabaseID.ValueString(), Username: plan.Username.ValueString()})
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
		resp.Diagnostics.AddError("Database user did not become ready", err.Error())
		return
	}
	state := databaseUserFromProto(user, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseUserResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state databaseUserModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.GetDatabaseUser(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.GetDatabaseUserRequest{
		DatabaseId: state.DatabaseID.ValueString(),
		Username:   state.Username.ValueString(),
	})
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read database user", err.Error())
		return
	}
	state = databaseUserFromProto(out.GetUser(), state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseUserResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior databaseUserModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	expires, err := parseExpiry(plan.ExpiresAt)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("expires_at"), "Invalid expires_at", err.Error())
	}
	if resp.Diagnostics.HasError() {
		return
	}

	update := &serverv1.UpdateDatabaseUserRequest{
		DatabaseId:       plan.DatabaseID.ValueString(),
		Username:         plan.Username.ValueString(),
		Description:      stringOrNil(plan.Description),
		StatementTimeout: stringOrNil(plan.StatementTimeout),
	}
	if !plan.Password.Equal(prior.Password) {
		update.Password = stringPtr(plan.Password.ValueString())
	}
	if !plan.ExpiresAt.Equal(prior.ExpiresAt) {
		update.ExpiresAt = expires
		if expires == nil {
			update.ExpiresAt = timestamppb.New(time.Time{})
		}
	}
	if v := plan.ConnectionLimit; !v.IsNull() && !v.IsUnknown() {
		update.ConnectionLimit = int32Ptr(v.ValueInt64())
	}
	out, err := r.client.API.UpdateDatabaseUser(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), update)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update database user", err.Error())
		return
	}
	state := databaseUserFromProto(out.GetUser(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseUserResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state databaseUserModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.DeleteDatabaseUser(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.DeleteDatabaseUserRequest{
		DatabaseId: state.DatabaseID.ValueString(),
		Username:   state.Username.ValueString(),
	})
	if err != nil && !relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot delete database user", err.Error())
	}
}

func (r *databaseUserResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<database_id>/<username>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("username"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("generate_password"), false)...)
}

func databaseUserFromProto(u *serverv1.DatabaseUser, prior databaseUserModel) databaseUserModel {
	out := prior
	out.ID = types.StringValue(prior.DatabaseID.ValueString() + "/" + u.GetUsername())
	out.Username = types.StringValue(u.GetUsername())
	out.Description = types.StringValue(u.GetDescription())
	out.AuthMethod = types.StringValue(u.GetAuthMethod())
	out.Roles = rolesFromProto(u.GetRoles())
	out.RestrictedResources = stringListFromGo(u.GetRestrictedResources(), prior.RestrictedResources)
	out.ExpiresAt = expiryFromProto(u.GetExpiresAt(), prior.ExpiresAt)
	out.ConnectionLimit = optionalInt64(u.ConnectionLimit)
	out.StatementTimeout = types.StringValue(u.GetStatementTimeout())
	out.Phase = types.StringValue(u.GetPhase())
	out.PhaseMessage = types.StringValue(u.GetPhaseMessage())
	out.Stale = types.BoolValue(u.GetStale())
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

func rolesToProto(roles []userRoleModel) []*serverv1.DatabaseUserRole {
	out := make([]*serverv1.DatabaseUserRole, 0, len(roles))
	for _, r := range roles {
		out = append(out, &serverv1.DatabaseUserRole{Role: r.Role.ValueString(), Db: r.DB.ValueString(), Collection: r.Collection.ValueString()})
	}
	return out
}

func rolesFromProto(roles []*serverv1.DatabaseUserRole) []userRoleModel {
	out := make([]userRoleModel, 0, len(roles))
	for _, r := range roles {
		out = append(out, userRoleModel{Role: types.StringValue(r.GetRole()), DB: types.StringValue(r.GetDb()), Collection: types.StringValue(r.GetCollection())})
	}
	return out
}

func parseExpiry(v types.String) (*timestamppb.Timestamp, error) {
	if v.IsNull() || v.IsUnknown() {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v.ValueString())
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

func expiryFromProto(ts *timestamppb.Timestamp, prior types.String) types.String {
	if ts == nil || ts.AsTime().IsZero() {
		return types.StringNull()
	}
	got := ts.AsTime()
	if p, err := time.Parse(time.RFC3339, prior.ValueString()); err == nil && p.Equal(got) {
		return prior
	}
	return types.StringValue(got.UTC().Format(time.RFC3339))
}

func stringOrNil(v types.String) *string {
	if v.IsNull() || v.IsUnknown() || v.ValueString() == "" {
		return nil
	}
	return stringPtr(v.ValueString())
}

func stringListToGo(l types.List) []string {
	out := make([]string, 0, len(l.Elements()))
	for _, e := range l.Elements() {
		out = append(out, e.(types.String).ValueString())
	}
	return out
}

func stringListFromGo(values []string, prior types.List) types.List {
	if len(values) == 0 && prior.IsNull() {
		return types.ListNull(types.StringType)
	}
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	return types.ListValueMust(types.StringType, elems)
}
