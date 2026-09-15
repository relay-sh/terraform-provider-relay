package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type workspaceDataSource struct {
	client *client.Client
}

func newWorkspaceDataSource() datasource.DataSource {
	return &workspaceDataSource{}
}

func (d *workspaceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_workspace"
}

func (d *workspaceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing workspace by name or ID, for workspaces not managed by this configuration.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Workspace ID."},
			"name":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Workspace name."},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "Free-text description."},
			"slug":        schema.StringAttribute{Computed: true, MarkdownDescription: "URL slug."},
			"is_default":  schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this is the organization's default workspace."},
			"managed_by":  schema.StringAttribute{Computed: true, MarkdownDescription: "Owner tag, empty when unmanaged."},
		},
	}
}

func (d *workspaceDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *workspaceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *workspaceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config workspaceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	lookup := &serverv1.GetWorkspaceRequest{Key: &serverv1.GetWorkspaceRequest_Name{Name: config.Name.ValueString()}}
	if !config.ID.IsNull() {
		lookup.Key = &serverv1.GetWorkspaceRequest_Id{Id: config.ID.ValueString()}
	}
	out, err := d.client.API.GetWorkspace(d.client.Org(ctx), lookup)
	if relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Workspace not found", "No workspace matches in the provider's organization, or the token cannot see it.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read workspace", err.Error())
		return
	}
	state := workspaceFromProto(out.GetWorkspace())
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
