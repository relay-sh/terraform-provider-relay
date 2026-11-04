package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type accessGrantResource struct {
	client *client.Client
}

type accessGrantModel struct {
	ID          types.String `tfsdk:"id"`
	WorkspaceID types.String `tfsdk:"workspace_id"`
	GranteeType types.String `tfsdk:"grantee_type"`
	Grantee     types.String `tfsdk:"grantee"`
	TargetType  types.String `tfsdk:"target_type"`
	Target      types.String `tfsdk:"target"`
	ExpiresAt   types.String `tfsdk:"expires_at"`
	GrantedBy   types.String `tfsdk:"granted_by"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

func newAccessGrantResource() resource.Resource {
	return &accessGrantResource{}
}

func (r *accessGrantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_access_grant"
}

func (r *accessGrantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	nonEmpty := stringvalidator.LengthAtLeast(1)
	resp.Schema = schema.Schema{
		MarkdownDescription: "Gives a user or a team access to one connection or connection group of a workspace. " +
			"A grant cannot be changed in place: changing any argument replaces it. " +
			"A grant that has expired, or was revoked outside Terraform, is removed from state on the next refresh, so the next apply recreates it; " +
			"an `expires_at` that is already in the past is rejected by the platform at that point. Recreating also clears the expired grant the platform still holds for the same grantee and target.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Grant ID."},
			"workspace_id": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				MarkdownDescription: "Workspace the grant belongs to.",
			},
			"grantee_type": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				Validators:          []validator.String{stringvalidator.OneOf("user", "team")},
				MarkdownDescription: "Kind of principal receiving access: `user` or `team`.",
			},
			"grantee": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				Validators:          []validator.String{nonEmpty},
				MarkdownDescription: "Email of the organization member when `grantee_type` is `user`, name of the team when it is `team`.",
			},
			"target_type": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				Validators:          []validator.String{stringvalidator.OneOf("connection", "connection_group")},
				MarkdownDescription: "Kind of resource the grant opens: `connection` or `connection_group`.",
			},
			"target": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       replace,
				Validators:          []validator.String{nonEmpty},
				MarkdownDescription: "Name of the connection or connection group, in the workspace. Renaming it replaces the grant.",
			},
			"expires_at": schema.StringAttribute{
				Optional:            true,
				PlanModifiers:       replace,
				Validators:          []validator.String{utcTimestamp{}},
				MarkdownDescription: "When the grant stops taking effect, as an RFC 3339 timestamp in UTC with a `Z` suffix and whole seconds, in the future. Omit for a permanent grant. Not available on every plan.",
			},
			"granted_by": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Principal that created the grant."},
			"created_at": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
	}
}

func (r *accessGrantResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *accessGrantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan accessGrantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := &serverv1.CreateAccessGrantRequest{
		GranteeType: plan.GranteeType.ValueString(),
		GranteeName: plan.Grantee.ValueString(),
		TargetType:  plan.TargetType.ValueString(),
		TargetName:  plan.Target.ValueString(),
	}
	if !plan.ExpiresAt.IsNull() {
		t, _ := time.Parse(time.RFC3339, plan.ExpiresAt.ValueString())
		in.ExpiresAt = timestamppb.New(t)
	}
	wctx := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	out, err := r.client.API.CreateAccessGrant(wctx, in)
	if status.Code(err) == codes.AlreadyExists && r.clearExpired(wctx, in) {
		out, err = r.client.API.CreateAccessGrant(wctx, in)
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot create access grant", err.Error())
		return
	}
	state := accessGrantFromProto(plan.WorkspaceID, out.GetGrant(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *accessGrantResource) clearExpired(ctx context.Context, in *serverv1.CreateAccessGrantRequest) bool {
	list, err := r.client.API.ListAccessGrants(ctx, &serverv1.ListAccessGrantsRequest{TargetType: in.GetTargetType(), TargetName: in.GetTargetName()})
	if err != nil {
		return false
	}
	for _, g := range list.GetGrants() {
		if g.GetGranteeType() == in.GetGranteeType() && strings.EqualFold(g.GetGranteeName(), in.GetGranteeName()) && accessGrantExpired(g) {
			_, err := r.client.API.DeleteAccessGrant(ctx, &serverv1.DeleteAccessGrantRequest{Id: g.GetId()})
			return err == nil
		}
	}
	return false
}

func (r *accessGrantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state accessGrantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.ListAccessGrants(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.ListAccessGrantsRequest{
		TargetType: state.TargetType.ValueString(),
		TargetName: state.Target.ValueString(),
	})
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read access grant", err.Error())
		return
	}
	grant := findAccessGrant(out.GetGrants(), state.ID.ValueString())
	if grant == nil || accessGrantExpired(grant) {
		resp.State.RemoveResource(ctx)
		return
	}
	state = accessGrantFromProto(state.WorkspaceID, grant, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *accessGrantResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Access grants cannot be updated", "Every argument forces a replacement; this is a provider bug.")
}

func (r *accessGrantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state accessGrantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.DeleteAccessGrant(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.DeleteAccessGrantRequest{Id: state.ID.ValueString()})
	if relay.IsNotFound(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot revoke access grant", err.Error())
	}
}

func (r *accessGrantResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	workspaceID, id, ok := strings.Cut(req.ID, "/")
	if !ok || workspaceID == "" || id == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<grant_id>, got "+req.ID+".")
		return
	}
	out, err := r.client.API.ListAccessGrants(r.client.Workspace(ctx, workspaceID), &serverv1.ListAccessGrantsRequest{})
	if err != nil {
		resp.Diagnostics.AddError("Cannot read access grants", err.Error())
		return
	}
	grant := findAccessGrant(out.GetGrants(), id)
	switch {
	case grant == nil:
		resp.Diagnostics.AddError("Access grant not found", "Workspace "+workspaceID+" has no access grant "+id+".")
		return
	case accessGrantExpired(grant):
		resp.Diagnostics.AddError("Expired access grant", id+" expired at "+grant.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)+" and no longer grants access.")
		return
	}
	state := accessGrantFromProto(types.StringValue(workspaceID), grant, accessGrantModel{})
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func findAccessGrant(grants []*serverv1.AccessGrant, id string) *serverv1.AccessGrant {
	for _, g := range grants {
		if g.GetId() == id {
			return g
		}
	}
	return nil
}

func accessGrantExpired(g *serverv1.AccessGrant) bool {
	return g.GetExpiresAt() != nil && !g.GetExpiresAt().AsTime().After(time.Now())
}

func accessGrantFromProto(workspaceID types.String, g *serverv1.AccessGrant, prior accessGrantModel) accessGrantModel {
	grantee := g.GetGranteeName()
	if g.GetGranteeType() == "user" && strings.EqualFold(grantee, prior.Grantee.ValueString()) {
		grantee = prior.Grantee.ValueString()
	}
	expires := types.StringNull()
	if g.GetExpiresAt() != nil {
		expires = types.StringValue(g.GetExpiresAt().AsTime().UTC().Format(time.RFC3339))
	}
	return accessGrantModel{
		ID:          types.StringValue(g.GetId()),
		WorkspaceID: workspaceID,
		GranteeType: types.StringValue(g.GetGranteeType()),
		Grantee:     types.StringValue(grantee),
		TargetType:  types.StringValue(g.GetTargetType()),
		Target:      types.StringValue(g.GetTargetName()),
		ExpiresAt:   expires,
		GrantedBy:   types.StringValue(g.GetGrantedBy()),
		CreatedAt:   timestampValue(g.GetCreatedAt()),
	}
}

type utcTimestamp struct{}

func (utcTimestamp) Description(context.Context) string {
	return "must be an RFC 3339 timestamp in UTC"
}

func (v utcTimestamp) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (utcTimestamp) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid timestamp", fmt.Sprintf("Expected an RFC 3339 timestamp such as 2026-12-01T10:00:00Z, got %q.", raw))
		return
	}
	if canonical := t.UTC().Format(time.RFC3339); canonical != raw {
		resp.Diagnostics.AddAttributeError(req.Path, "Timestamp is not canonical", fmt.Sprintf("The API returns %q as %q. Write %q so the plan stays empty.", raw, canonical, canonical))
	}
}
