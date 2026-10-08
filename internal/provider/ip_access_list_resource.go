package provider

import (
	"context"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const ipAccessListTimeout = 10 * time.Minute

type ipAccessListResource struct {
	client *client.Client
}

type ipAccessListResourceModel struct {
	ipAccessListModel
	Timeouts timeouts.Value `tfsdk:"timeouts"`
}

func newIPAccessListResource() resource.Resource {
	return &ipAccessListResource{}
}

func (r *ipAccessListResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ip_access_list"
}

func (r *ipAccessListResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "One entry of a workspace's IP access list: a source range allowed to reach the public endpoints of every database in the workspace. " +
			"Creating, updating and destroying wait until the change has reached the edge. Private endpoints are not subject to the list.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Entry ID."},
			"workspace_id": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				MarkdownDescription: "Workspace the entry belongs to.",
			},
			"cidr": schema.StringAttribute{
				Required:            true,
				Validators:          []validator.String{canonicalCIDR{}},
				MarkdownDescription: "IPv4 or IPv6 network in canonical form, unique in the workspace. A single host is `/32` or `/128`. Changing it updates the entry in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				Validators:          []validator.String{stringvalidator.LengthAtMost(200), trimmed{}},
				MarkdownDescription: "Free-text label, up to 200 characters.",
			},
			"created_by": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Principal that added the entry."},
			"created_at": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Creation time, RFC 3339."},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

func (r *ipAccessListResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *ipAccessListResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ipAccessListResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	timeout, diags := plan.Timeouts.Create(ctx, ipAccessListTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.AddIPAllowlistEntry(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), &serverv1.AddIPAllowlistEntryRequest{
		Cidr:        plan.CIDR.ValueString(),
		Description: plan.Description.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot add IP access list entry", err.Error())
		return
	}
	state := ipAccessListResourceModel{ipAccessListFromProto(plan.WorkspaceID, out.GetEntry()), plan.Timeouts}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if err := r.waitSynced(ctx, plan.WorkspaceID.ValueString(), timeout); err != nil {
		resp.Diagnostics.AddError("IP access list did not reach the edge", err.Error())
	}
}

func (r *ipAccessListResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ipAccessListResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := ipAccessList(ctx, r.client, state.WorkspaceID.ValueString())
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read IP access list", err.Error())
		return
	}
	id := state.ID.ValueString()
	entry := findIPAccessListEntry(out.GetEntries(), func(e *serverv1.IPAllowlistEntry) bool { return e.GetId() == id })
	if entry == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	state.ipAccessListModel = ipAccessListFromProto(state.WorkspaceID, entry)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ipAccessListResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior ipAccessListResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	timeout, diags := plan.Timeouts.Update(ctx, ipAccessListTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	update := &serverv1.UpdateIPAllowlistEntryRequest{Id: prior.ID.ValueString()}
	if !plan.CIDR.Equal(prior.CIDR) {
		update.Cidr = plan.CIDR.ValueStringPointer()
	}
	if !plan.Description.Equal(prior.Description) {
		update.Description = plan.Description.ValueStringPointer()
	}
	if update.Cidr == nil && update.Description == nil {
		prior.Timeouts = plan.Timeouts
		resp.Diagnostics.Append(resp.State.Set(ctx, &prior)...)
		return
	}
	out, err := r.client.API.UpdateIPAllowlistEntry(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), update)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update IP access list entry", err.Error())
		return
	}
	state := ipAccessListResourceModel{ipAccessListFromProto(plan.WorkspaceID, out.GetEntry()), plan.Timeouts}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if err := r.waitSynced(ctx, plan.WorkspaceID.ValueString(), timeout); err != nil {
		resp.Diagnostics.AddError("IP access list did not reach the edge", err.Error())
	}
}

func (r *ipAccessListResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ipAccessListResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	timeout, diags := state.Timeouts.Delete(ctx, ipAccessListTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	workspaceID := state.WorkspaceID.ValueString()
	_, err := r.client.API.RemoveIPAllowlistEntry(r.client.Workspace(ctx, workspaceID), &serverv1.RemoveIPAllowlistEntryRequest{Id: state.ID.ValueString()})
	if relay.IsNotFound(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot remove IP access list entry", err.Error())
		return
	}
	if err := r.waitSynced(ctx, workspaceID, timeout); err != nil {
		resp.Diagnostics.AddError("IP access list did not reach the edge", err.Error())
	}
}

func (r *ipAccessListResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	workspaceID, cidr, ok := strings.Cut(req.ID, "/")
	if !ok || workspaceID == "" || cidr == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cidr>, got "+req.ID+".")
		return
	}
	out, err := ipAccessList(ctx, r.client, workspaceID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read IP access list", err.Error())
		return
	}
	entry := findIPAccessListEntry(out.GetEntries(), func(e *serverv1.IPAllowlistEntry) bool { return e.GetCidr() == cidr })
	switch {
	case entry == nil:
		resp.Diagnostics.AddError("IP access list entry not found", "Workspace "+workspaceID+" has no entry for "+cidr+". The CIDR must be written as the API stores it.")
		return
	case entry.GetExpiresAt() != nil:
		resp.Diagnostics.AddError("Temporary entry", cidr+" expires at "+entry.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)+". Terraform would recreate it after it expires; make it permanent first.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), workspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), entry.GetId())...)
}

func (r *ipAccessListResource) waitSynced(ctx context.Context, workspaceID string, timeout time.Duration) error {
	return waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		out, err := ipAccessList(ctx, r.client, workspaceID)
		if err != nil {
			return false, err
		}
		return !out.GetSyncing(), nil
	})
}
