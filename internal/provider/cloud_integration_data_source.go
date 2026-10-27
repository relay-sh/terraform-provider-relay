package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type cloudIntegrationDataSource struct {
	client *client.Client
}

type cloudIntegrationDataModel struct {
	ID           types.String `tfsdk:"id"`
	WorkspaceID  types.String `tfsdk:"workspace_id"`
	Name         types.String `tfsdk:"name"`
	Cloud        types.String `tfsdk:"cloud"`
	AWSAccountID types.String `tfsdk:"aws_account_id"`
	RoleARN      types.String `tfsdk:"role_arn"`
	ExternalID   types.String `tfsdk:"external_id"`
	Status       types.String `tfsdk:"status"`
	LastError    types.String `tfsdk:"last_error"`
}

func newCloudIntegrationDataSource() datasource.DataSource {
	return &cloudIntegrationDataSource{}
}

func (d *cloudIntegrationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cloud_integration"
}

func (d *cloudIntegrationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computed := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a cloud integration, a customer cloud account Relay assumes a role into, by ID or by name.",
		Attributes: map[string]schema.Attribute{
			"workspace_id":   schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"id":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Integration ID."},
			"name":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Integration name, unique in the workspace."},
			"cloud":          computed("Cloud provider, `aws`."),
			"aws_account_id": computed("AWS account the role belongs to."),
			"role_arn":       computed("ARN of the IAM role Relay assumes."),
			"external_id":    computed("External ID Relay presents when assuming the role."),
			"status":         computed("`pending`, `connected` or `error`."),
			"last_error":     computed("Why the last role assumption failed, when it did."),
		},
	}
}

func (d *cloudIntegrationDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "name")
}

func (d *cloudIntegrationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *cloudIntegrationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config cloudIntegrationDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	scoped := d.client.Workspace(ctx, config.WorkspaceID.ValueString())

	var it *serverv1.CloudIntegration
	if !config.ID.IsNull() {
		out, err := d.client.API.GetCloudIntegration(scoped, &serverv1.GetCloudIntegrationRequest{Id: config.ID.ValueString()})
		if err != nil && !relay.IsNotFound(err) {
			resp.Diagnostics.AddError("Cannot read cloud integration", err.Error())
			return
		}
		it = out.GetIntegration()
	} else {
		out, err := d.client.API.ListCloudIntegrations(scoped, &serverv1.ListCloudIntegrationsRequest{})
		if err != nil {
			resp.Diagnostics.AddError("Cannot list cloud integrations", err.Error())
			return
		}
		var matches []*serverv1.CloudIntegration
		for _, c := range out.GetIntegrations() {
			if c.GetName() == config.Name.ValueString() {
				matches = append(matches, c)
			}
		}
		if len(matches) > 1 {
			resp.Diagnostics.AddError("Cloud integration name is ambiguous", fmt.Sprintf("%d integrations in the workspace are named %q; look it up by id instead.", len(matches), config.Name.ValueString()))
			return
		}
		if len(matches) == 1 {
			it = matches[0]
		}
	}
	if it == nil {
		resp.Diagnostics.AddError("Cloud integration not found", "No cloud integration matches in the workspace, or the token cannot see it.")
		return
	}

	state := cloudIntegrationDataModel{
		ID:           types.StringValue(it.GetId()),
		WorkspaceID:  config.WorkspaceID,
		Name:         types.StringValue(it.GetName()),
		Cloud:        types.StringValue(it.GetProvider()),
		AWSAccountID: types.StringValue(it.GetAwsAccountId()),
		RoleARN:      types.StringValue(it.GetRoleArn()),
		ExternalID:   types.StringValue(it.GetExternalId()),
		Status:       types.StringValue(it.GetStatus()),
		LastError:    types.StringValue(it.GetLastError()),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
