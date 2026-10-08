package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type ipAccessListDataSource struct {
	client *client.Client
}

func newIPAccessListDataSource() datasource.DataSource {
	return &ipAccessListDataSource{}
}

func (d *ipAccessListDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ip_access_list"
}

func (d *ipAccessListDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads one entry of a workspace's IP access list, by ID or by CIDR.",
		Attributes: map[string]schema.Attribute{
			"workspace_id": schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"id":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Entry ID."},
			"cidr":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Network, as the API stores it."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-text label."},
			"created_by":   schema.StringAttribute{Computed: true, MarkdownDescription: "Principal that added the entry."},
			"created_at":   schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time, RFC 3339."},
		},
	}
}

func (d *ipAccessListDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "cidr")
}

func (d *ipAccessListDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *ipAccessListDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config ipAccessListModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := ipAccessList(ctx, d.client, config.WorkspaceID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot read IP access list", err.Error())
		return
	}
	entry := findIPAccessListEntry(out.GetEntries(), func(e *serverv1.IPAllowlistEntry) bool {
		if !config.ID.IsNull() {
			return e.GetId() == config.ID.ValueString()
		}
		return e.GetCidr() == config.CIDR.ValueString()
	})
	if entry == nil {
		resp.Diagnostics.AddError("IP access list entry not found", "No entry matches in the workspace, or the token cannot see it.")
		return
	}
	state := ipAccessListFromProto(config.WorkspaceID, entry)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
