package provider

import (
	"context"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
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

type apiKeyResource struct {
	client *client.Client
}

type apiKeyModel struct {
	ID                    types.String `tfsdk:"id"`
	Name                  types.String `tfsdk:"name"`
	WorkspaceID           types.String `tfsdk:"workspace_id"`
	Role                  types.String `tfsdk:"role"`
	AllowedCIDRs          types.Set    `tfsdk:"allowed_cidrs"`
	EffectiveAllowedCIDRs types.Set    `tfsdk:"effective_allowed_cidrs"`
	ExpiresIn             types.String `tfsdk:"expires_in"`
	ExpiresAt             types.String `tfsdk:"expires_at"`
	Secret                types.String `tfsdk:"secret"`
	ParentKeyID           types.String `tfsdk:"parent_key_id"`
	CreatedAt             types.String `tfsdk:"created_at"`
}

func newAPIKeyResource() resource.Resource {
	return &apiKeyResource{}
}

func (r *apiKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (r *apiKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	trimmed := stringvalidator.RegexMatches(regexp.MustCompile(`^\S(?s:.*\S)?$`), "must not be empty or start or end with whitespace, which the API strips")
	resp.Schema = schema.Schema{
		MarkdownDescription: "An organization or workspace API key. A key created with an API key is its child: it cannot outlive it or reach past its allowed addresses, and revoking the parent revokes it. The secret is returned once, at creation.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "API key ID."},
			"name": schema.StringAttribute{
				Required:            true,
				Validators:          []validator.String{stringvalidator.UTF8LengthBetween(1, 100), trimmed},
				MarkdownDescription: "Display name, up to 100 characters. Names need not be unique.",
			},
			"workspace_id": schema.StringAttribute{
				Optional:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				MarkdownDescription: "Workspace the key is confined to. Omit it for an organization key. Changing it replaces the key.",
			},
			"role": schema.StringAttribute{
				Required:            true,
				Validators:          []validator.String{trimmed},
				MarkdownDescription: "Role slug granted to the key, such as `admin` or `viewer`. It cannot grant a permission the caller lacks, nor an owner-level role. Changes in place.",
			},
			"allowed_cidrs": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtMost(20),
					setvalidator.ValueStringsAre(addressValidator{}),
				},
				MarkdownDescription: "IP addresses or CIDR blocks the key may be used from, up to 20. Omit it to allow any address, or the parent key's addresses.",
			},
			"effective_allowed_cidrs": schema.SetAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "Addresses the platform enforces: `allowed_cidrs` normalized to CIDR blocks, or the parent key's when `allowed_cidrs` is empty.",
			},
			"expires_in": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{lifetimeValidator{}},
				MarkdownDescription: "Lifetime as a duration, such as `720h`, at most `8760h`. Counted from the apply that sets or changes it. Omit it for a key that never expires, or that expires with its parent.",
			},
			"expires_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Expiry, RFC 3339. Null when the key never expires."},
			"secret": schema.StringAttribute{
				Computed: true, Sensitive: true, PlanModifiers: keep,
				MarkdownDescription: "The `rly_key_…` secret, stored in state. Not recoverable from the API, so it is null after an import.",
			},
			"parent_key_id": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "ID of the API key that created this one, empty when a user did."},
			"created_at":    schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
	}
}

func (r *apiKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *apiKeyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, prior apiKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.WorkspaceID.Equal(prior.WorkspaceID) {
		return
	}
	if plan.ExpiresIn.Equal(prior.ExpiresIn) {
		plan.ExpiresAt = prior.ExpiresAt
	}
	if plan.AllowedCIDRs.Equal(prior.AllowedCIDRs) {
		plan.EffectiveAllowedCIDRs = prior.EffectiveAllowedCIDRs
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *apiKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan apiKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	create := &serverv1.CreateAPIKeyRequest{
		Name:         plan.Name.ValueString(),
		WorkspaceId:  plan.WorkspaceID.ValueString(),
		RoleSlug:     plan.Role.ValueString(),
		AllowedCidrs: stringSetToGo(plan.AllowedCIDRs),
	}
	if !plan.ExpiresIn.IsNull() {
		create.ExpiresInSeconds = lifetimeSeconds(plan.ExpiresIn)
	}
	out, err := r.client.API.CreateAPIKey(r.client.Org(ctx), create)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create API key", err.Error())
		return
	}
	plan.Secret = types.StringValue(out.GetPlaintext())
	state := apiKeyFromProto(out.GetKey(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *apiKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	keys, err := listAPIKeys(ctx, r.client, state.WorkspaceID.ValueString())
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read API key", err.Error())
		return
	}
	key := apiKeyByID(keys, state.ID.ValueString())
	if key == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	imported := state.EffectiveAllowedCIDRs.IsNull()
	drifted := !imported && !stringSetFromGo(key.GetAllowedCidrs()).Equal(state.EffectiveAllowedCIDRs)
	if drifted || (imported && key.GetParentKeyId() == "") {
		state.AllowedCIDRs = types.SetNull(types.StringType)
		if len(key.GetAllowedCidrs()) > 0 {
			state.AllowedCIDRs = stringSetFromGo(key.GetAllowedCidrs())
		}
	}
	state = apiKeyFromProto(key, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *apiKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior apiKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	update := &serverv1.UpdateAPIKeyRequest{
		Id:           prior.ID.ValueString(),
		Name:         plan.Name.ValueString(),
		AllowedCidrs: stringSetToGo(plan.AllowedCIDRs),
	}
	if !plan.Role.Equal(prior.Role) {
		update.RoleSlug = stringPtr(plan.Role.ValueString())
	}
	if !plan.ExpiresIn.Equal(prior.ExpiresIn) {
		update.ExpiresInSeconds = lifetimeSeconds(plan.ExpiresIn)
	}
	out, err := r.client.API.UpdateAPIKey(r.client.Org(ctx), update)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update API key", err.Error())
		return
	}
	state := apiKeyFromProto(out.GetKey(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *apiKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state apiKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.RevokeAPIKey(r.client.Org(ctx), &serverv1.RevokeAPIKeyRequest{Id: state.ID.ValueString()})
	if err != nil && !relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot revoke API key", err.Error())
	}
}

func (r *apiKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) > 2 || parts[len(parts)-1] == "" || (len(parts) == 2 && parts[0] == "") {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <key_id> for an organization key or <workspace_id>/<key_id> for a workspace key, got "+req.ID+".")
		return
	}
	if len(parts) == 2 {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[len(parts)-1])...)
}

func apiKeyFromProto(k *serverv1.APIKey, prior apiKeyModel) apiKeyModel {
	out := prior
	out.ID = types.StringValue(k.GetId())
	out.Name = types.StringValue(k.GetName())
	out.WorkspaceID = optionalString(k.GetWorkspaceId())
	out.Role = types.StringValue(k.GetRoleSlug())
	out.EffectiveAllowedCIDRs = stringSetFromGo(k.GetAllowedCidrs())
	out.ExpiresAt = optionalTimestamp(k.GetExpiresAt())
	out.ParentKeyID = types.StringValue(k.GetParentKeyId())
	out.CreatedAt = timestampValue(k.GetCreatedAt())
	return out
}

func lifetimeSeconds(v types.String) *int64 {
	var seconds int64
	if lifetime, err := parseLifetime(v); err == nil {
		seconds = int64(lifetime.Seconds())
	}
	return &seconds
}
