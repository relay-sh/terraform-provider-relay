package provider

import (
	"context"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

var roleARNPattern = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/[\w+=,.@/-]+$`)

type cloudIntegrationResource struct {
	client *client.Client
}

type cloudIntegrationModel struct {
	ID           types.String `tfsdk:"id"`
	WorkspaceID  types.String `tfsdk:"workspace_id"`
	Cloud        types.String `tfsdk:"cloud"`
	Name         types.String `tfsdk:"name"`
	RoleARN      types.String `tfsdk:"role_arn"`
	AWSAccountID types.String `tfsdk:"aws_account_id"`
	ExternalID   types.String `tfsdk:"external_id"`
	Status       types.String `tfsdk:"status"`
	LastError    types.String `tfsdk:"last_error"`
}

func newCloudIntegrationResource() resource.Resource {
	return &cloudIntegrationResource{}
}

func (r *cloudIntegrationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cloud_integration"
}

func (r *cloudIntegrationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A cloud integration: an IAM role in the customer's AWS account that Relay assumes, for example to export backups to S3. " +
			"Build the role from `relay_cloud_integration_setup`. After creating the integration, and after changing `role_arn`, the provider asks Relay to assume the role; " +
			"while IAM is still propagating that fails with a warning, not an error, and `status` stays `pending` or `error` until the integration is tested again from the dashboard. " +
			"An integration named by a backup auto-export or log stream cannot be deleted while that export or stream exists; " +
			"destroying `relay_backup_auto_export` deletes the export, so a configuration holding both is destroyed in one run.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "Integration ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the integration belongs to."},
			"cloud": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString("aws"), PlanModifiers: replace,
				Validators:          []validator.String{stringvalidator.OneOf("aws")},
				MarkdownDescription: "Cloud provider. Only `aws`, the default, is available.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1), stringvalidator.RegexMatches(untrimmedPattern, "must not start or end with whitespace")},
				MarkdownDescription: "Integration name, unique in the workspace.",
			},
			"role_arn": schema.StringAttribute{
				Required:            true,
				Validators:          []validator.String{stringvalidator.RegexMatches(roleARNPattern, "must look like arn:aws:iam::123456789012:role/name")},
				MarkdownDescription: "ARN of the IAM role Relay assumes. Changing it resets `status` to `pending` and tests the new role.",
			},
			"aws_account_id": schema.StringAttribute{Computed: true, MarkdownDescription: "AWS account the role belongs to."},
			"external_id":    schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "External ID Relay presents when assuming the role."},
			"status":         schema.StringAttribute{Computed: true, MarkdownDescription: "`pending`, `connected` or `error`."},
			"last_error":     schema.StringAttribute{Computed: true, MarkdownDescription: "Why the last role assumption failed, when it did."},
		},
	}
}

func (r *cloudIntegrationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *cloudIntegrationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan cloudIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	out, err := r.client.API.CreateCloudIntegration(scoped, &serverv1.CreateCloudIntegrationRequest{
		Provider: plan.Cloud.ValueString(),
		Name:     plan.Name.ValueString(),
		RoleArn:  plan.RoleARN.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create cloud integration", err.Error())
		return
	}
	state := r.test(scoped, out.GetIntegration(), plan.WorkspaceID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *cloudIntegrationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state cloudIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.GetCloudIntegration(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.GetCloudIntegrationRequest{Id: state.ID.ValueString()})
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read cloud integration", err.Error())
		return
	}
	state = cloudIntegrationFromProto(out.GetIntegration(), state.WorkspaceID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *cloudIntegrationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior cloudIntegrationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := r.client.Workspace(ctx, plan.WorkspaceID.ValueString())
	update := &serverv1.UpdateCloudIntegrationRequest{Id: prior.ID.ValueString(), Name: plan.Name.ValueStringPointer()}
	roleChanged := !plan.RoleARN.Equal(prior.RoleARN)
	if roleChanged {
		update.RoleArn = plan.RoleARN.ValueStringPointer()
	}
	out, err := r.client.API.UpdateCloudIntegration(scoped, update)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update cloud integration", err.Error())
		return
	}
	state := cloudIntegrationFromProto(out.GetIntegration(), plan.WorkspaceID)
	if roleChanged {
		state = r.test(scoped, out.GetIntegration(), plan.WorkspaceID, &resp.Diagnostics)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *cloudIntegrationResource) test(ctx context.Context, it *serverv1.CloudIntegration, workspaceID types.String, diags *diag.Diagnostics) cloudIntegrationModel {
	out, err := r.client.API.TestCloudIntegration(ctx, &serverv1.TestCloudIntegrationRequest{Id: it.GetId()})
	if err != nil {
		diags.AddWarning("Cloud integration not verified",
			"Relay could not test the role, so status stays "+it.GetStatus()+": "+status.Convert(err).Message()+"\n\nTest the integration again from the dashboard.")
		return cloudIntegrationFromProto(it, workspaceID)
	}
	if !out.GetOk() {
		diags.AddWarning("Relay cannot assume the role yet",
			"Relay could not assume "+it.GetRoleArn()+": "+out.GetIntegration().GetLastError()+"\n\n"+
				"A role or trust policy created in the same apply can take a minute to propagate through IAM. The integration is saved with status "+out.GetIntegration().GetStatus()+
				"; once the role is in place, test it again from the dashboard and the status refreshes on the next plan.")
	}
	return cloudIntegrationFromProto(out.GetIntegration(), workspaceID)
}

func (r *cloudIntegrationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state cloudIntegrationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.DeleteCloudIntegration(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.DeleteCloudIntegrationRequest{Id: state.ID.ValueString()})
	switch {
	case err == nil || relay.IsNotFound(err):
	case status.Code(err) == codes.FailedPrecondition:
		resp.Diagnostics.AddError("Cloud integration is still in use",
			status.Convert(err).Message()+".\n\n"+
				"A backup auto-export or log stream not managed by this configuration, or one disabled by an earlier version of this provider, still names this integration. "+
				"Delete it or point its integration_id at another integration, then destroy this one.")
	default:
		resp.Diagnostics.AddError("Cannot delete cloud integration", err.Error())
	}
}

func (r *cloudIntegrationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 2 || parts[0] == "" || !uuidPattern.MatchString(parts[1]) {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<integration_id>, the integration ID a lowercase UUID, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[1])...)
}

func cloudIntegrationFromProto(it *serverv1.CloudIntegration, workspaceID types.String) cloudIntegrationModel {
	return cloudIntegrationModel{
		ID:           types.StringValue(it.GetId()),
		WorkspaceID:  workspaceID,
		Cloud:        types.StringValue(it.GetProvider()),
		Name:         types.StringValue(it.GetName()),
		RoleARN:      types.StringValue(it.GetRoleArn()),
		AWSAccountID: types.StringValue(it.GetAwsAccountId()),
		ExternalID:   types.StringValue(it.GetExternalId()),
		Status:       types.StringValue(it.GetStatus()),
		LastError:    types.StringValue(it.GetLastError()),
	}
}
