package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type cloudIntegrationSetupDataSource struct {
	client *client.Client
}

type cloudIntegrationSetupModel struct {
	WorkspaceID       types.String `tfsdk:"workspace_id"`
	Cloud             types.String `tfsdk:"cloud"`
	RelayAWSAccountID types.String `tfsdk:"relay_aws_account_id"`
	ExternalID        types.String `tfsdk:"external_id"`
	TrustPolicy       types.String `tfsdk:"trust_policy"`
	PermissionsPolicy types.String `tfsdk:"permissions_policy"`
}

func newCloudIntegrationSetupDataSource() datasource.DataSource {
	return &cloudIntegrationSetupDataSource{}
}

func (d *cloudIntegrationSetupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cloud_integration_setup"
}

func (d *cloudIntegrationSetupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "What an IAM role needs for Relay to assume it: the trust policy and external ID for the role, and the S3 permissions to attach to it. " +
			"Reading it needs permission to create cloud integrations in the workspace.",
		Attributes: map[string]schema.Attribute{
			"workspace_id": schema.StringAttribute{Required: true, MarkdownDescription: "Workspace the integration will belong to."},
			"cloud": schema.StringAttribute{
				Optional: true, Computed: true,
				Validators:          []validator.String{stringvalidator.OneOf("aws")},
				MarkdownDescription: "Cloud provider. Only `aws`, the default, is available.",
			},
			"relay_aws_account_id": schema.StringAttribute{Computed: true, MarkdownDescription: "AWS account Relay assumes the role from."},
			"external_id":          schema.StringAttribute{Computed: true, MarkdownDescription: "External ID Relay presents when assuming the role. It is the same for every integration in the organization."},
			"trust_policy":         schema.StringAttribute{Computed: true, MarkdownDescription: "Trust policy JSON for the role's `assume_role_policy`."},
			"permissions_policy": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Permissions policy JSON for the role. It names the bucket `YOUR_BUCKET`; replace it with the export bucket, for example with `replace()`.",
			},
		},
	}
}

func (d *cloudIntegrationSetupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *cloudIntegrationSetupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config cloudIntegrationSetupModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cloud := config.Cloud.ValueString()
	if cloud == "" {
		cloud = "aws"
	}
	out, err := d.client.API.GetCloudIntegrationSetup(d.client.Workspace(ctx, config.WorkspaceID.ValueString()), &serverv1.GetCloudIntegrationSetupRequest{Provider: cloud})
	if status.Code(err) == codes.FailedPrecondition {
		resp.Diagnostics.AddError("Cloud integrations are not available", status.Convert(err).Message()+".")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read cloud integration setup", err.Error())
		return
	}
	state := cloudIntegrationSetupModel{
		WorkspaceID:       config.WorkspaceID,
		Cloud:             types.StringValue(cloud),
		RelayAWSAccountID: types.StringValue(out.GetRelayAwsAccountId()),
		ExternalID:        types.StringValue(out.GetExternalId()),
		TrustPolicy:       types.StringValue(out.GetTrustPolicy()),
		PermissionsPolicy: types.StringValue(out.GetPermissionsPolicy()),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
