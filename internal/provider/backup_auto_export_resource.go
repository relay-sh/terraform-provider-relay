package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type backupAutoExportResource struct {
	client *client.Client
}

type backupAutoExportModel struct {
	ID             types.String `tfsdk:"id"`
	WorkspaceID    types.String `tfsdk:"workspace_id"`
	IntegrationID  types.String `tfsdk:"integration_id"`
	Bucket         types.String `tfsdk:"bucket"`
	Prefix         types.String `tfsdk:"prefix"`
	Which          types.String `tfsdk:"which"`
	Scope          types.Object `tfsdk:"scope"`
	Backfill       types.Bool   `tfsdk:"backfill"`
	LastError      types.String `tfsdk:"last_error"`
	LastExportedAt types.String `tfsdk:"last_exported_at"`
}

var (
	uuidPattern           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	bucketPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	untrimmedPattern      = regexp.MustCompile(`^(\S(.*\S)?)?$`)
	clusterScopeAttrTypes = map[string]attr.Type{"mode": types.StringType, "ids": types.SetType{ElemType: types.StringType}}
)

func newBackupAutoExportResource() resource.Resource {
	return &backupAutoExportResource{}
}

func (r *backupAutoExportResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_backup_auto_export"
}

func (r *backupAutoExportResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	uuid := stringvalidator.RegexMatches(uuidPattern, "must be a lowercase UUID")

	resp.Schema = schema.Schema{
		MarkdownDescription: "Copies the completed backups of a workspace's clusters to an S3 bucket through a cloud integration. " +
			"There is one auto-export per workspace: two of these resources on the same workspace overwrite each other. " +
			"Destroying the resource deletes the export, so the cloud integration it names can be destroyed in the same run. " +
			"An export disabled outside Terraform is removed from state.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "The workspace ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, MarkdownDescription: "Workspace whose backups are exported."},
			"integration_id": schema.StringAttribute{
				Required: true, Validators: []validator.String{uuid},
				MarkdownDescription: "Cloud integration in the same workspace whose role writes to the bucket, such as `data.relay_cloud_integration.this.id`.",
			},
			"bucket": schema.StringAttribute{
				Required: true, Validators: []validator.String{stringvalidator.RegexMatches(bucketPattern, "must be a valid S3 bucket name")},
				MarkdownDescription: "S3 bucket the backups are copied to.",
			},
			"prefix": schema.StringAttribute{
				Optional: true, Validators: []validator.String{stringvalidator.RegexMatches(untrimmedPattern, "must not start or end with whitespace")},
				MarkdownDescription: "Key prefix inside the bucket.",
			},
			"which": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("all"),
				Validators:          []validator.String{stringvalidator.OneOf("all", "scheduled", "manual")},
				MarkdownDescription: "Backups to export: `all`, `scheduled` or `manual`. Defaults to `all`.",
			},
			"scope": schema.SingleNestedAttribute{
				Optional: true, Computed: true,
				Default: objectdefault.StaticValue(types.ObjectValueMust(clusterScopeAttrTypes, map[string]attr.Value{
					"mode": types.StringValue("all"), "ids": types.SetNull(types.StringType),
				})),
				MarkdownDescription: "Clusters whose backups are exported. Defaults to every cluster in the workspace.",
				Attributes: map[string]schema.Attribute{
					"mode": schema.StringAttribute{
						Required:            true,
						Validators:          []validator.String{stringvalidator.OneOf("all", "selected")},
						MarkdownDescription: "`all` for every cluster in the workspace, including those created later, or `selected` for `ids`.",
					},
					"ids": schema.SetAttribute{
						Optional: true, ElementType: types.StringType,
						Validators:          []validator.Set{setvalidator.ValueStringsAre(uuid)},
						MarkdownDescription: "Cluster IDs, required when `mode` is `selected` and not allowed otherwise.",
					},
				},
			},
			"backfill": schema.BoolAttribute{
				Optional: true, Computed: true, PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Also export the backups that already exist, oldest first, when the resource is created. " +
					"Without it only backups completed from then on are exported. Sent on create only: changing it later plans an update that applies nothing, and an import sets it to false.",
			},
			"last_error":       schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Why the last export failed, when it did."},
			"last_exported_at": schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Time of the last successful export, RFC 3339, empty before the first one."},
		},
	}
}

func (r *backupAutoExportResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var scope types.Object
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("scope"), &scope)...)
	if !known(scope) {
		return
	}
	mode, ids := scope.Attributes()["mode"].(types.String), scope.Attributes()["ids"].(types.Set)
	at := path.Root("scope").AtName("ids")
	switch {
	case mode.IsUnknown() || ids.IsUnknown():
	case mode.ValueString() == "selected" && len(ids.Elements()) == 0:
		resp.Diagnostics.AddAttributeError(at, "Missing cluster IDs", "A selected scope exports the clusters in ids; list at least one.")
	case mode.ValueString() == "all" && !ids.IsNull():
		resp.Diagnostics.AddAttributeError(at, "Cluster IDs not allowed", "A scope of all exports every cluster in the workspace; remove ids or set mode to selected.")
	}
}

func (r *backupAutoExportResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *backupAutoExportResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan backupAutoExportModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.upsert(ctx, plan, plan.Backfill.ValueBool(), &resp.State, &resp.Diagnostics)
}

func (r *backupAutoExportResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state backupAutoExportModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.GetBackupAutoExport(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.GetBackupAutoExportRequest{})
	if relay.IsNotFound(err) || (err == nil && !out.GetAutoExport().GetEnabled()) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read backup auto-export", err.Error())
		return
	}
	state = backupAutoExportFromProto(out.GetAutoExport(), state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *backupAutoExportResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan backupAutoExportModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.upsert(ctx, plan, false, &resp.State, &resp.Diagnostics)
}

func (r *backupAutoExportResource) upsert(ctx context.Context, plan backupAutoExportModel, backfill bool, state *tfsdk.State, diags *diag.Diagnostics) {
	out, err := r.client.API.UpsertBackupAutoExport(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), &serverv1.UpsertBackupAutoExportRequest{
		AutoExport: backupAutoExportToProto(plan),
		Backfill:   backfill,
	})
	if err != nil {
		diags.AddError("Cannot set backup auto-export", err.Error())
		return
	}
	got := backupAutoExportFromProto(out.GetAutoExport(), plan)
	if known(plan.LastError) {
		got.LastError, got.LastExportedAt = plan.LastError, plan.LastExportedAt
	}
	diags.Append(state.Set(ctx, &got)...)
}

func (r *backupAutoExportResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state backupAutoExportModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.DeleteBackupAutoExport(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.DeleteBackupAutoExportRequest{})
	if err != nil && !relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot delete backup auto-export", err.Error())
	}
}

func (r *backupAutoExportResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

func backupAutoExportToProto(m backupAutoExportModel) *serverv1.BackupAutoExport {
	scope := m.Scope.Attributes()
	var ids []string
	for _, id := range scope["ids"].(types.Set).Elements() {
		ids = append(ids, id.(types.String).ValueString())
	}
	return &serverv1.BackupAutoExport{
		IntegrationId: m.IntegrationID.ValueString(),
		Bucket:        m.Bucket.ValueString(),
		Prefix:        m.Prefix.ValueString(),
		Which:         m.Which.ValueString(),
		Scope:         &serverv1.ClusterScope{Mode: scope["mode"].(types.String).ValueString(), Ids: ids},
		Enabled:       true,
	}
}

func backupAutoExportFromProto(e *serverv1.BackupAutoExport, prior backupAutoExportModel) backupAutoExportModel {
	out := prior
	out.ID = prior.WorkspaceID
	out.IntegrationID = types.StringValue(e.GetIntegrationId())
	out.Bucket = types.StringValue(e.GetBucket())
	if e.GetPrefix() != "" || !prior.Prefix.IsNull() {
		out.Prefix = types.StringValue(e.GetPrefix())
	}
	out.Which = types.StringValue(e.GetWhich())
	ids := types.SetNull(types.StringType)
	if e.GetScope().GetMode() == "selected" {
		elems := make([]attr.Value, 0, len(e.GetScope().GetIds()))
		for _, id := range e.GetScope().GetIds() {
			elems = append(elems, types.StringValue(id))
		}
		ids = types.SetValueMust(types.StringType, elems)
	}
	out.Scope = types.ObjectValueMust(clusterScopeAttrTypes, map[string]attr.Value{"mode": types.StringValue(e.GetScope().GetMode()), "ids": ids})
	if !known(prior.Backfill) {
		out.Backfill = types.BoolValue(false)
	}
	out.LastError = types.StringValue(e.GetLastError())
	out.LastExportedAt = timestampValue(e.GetLastExportedAt())
	return out
}
