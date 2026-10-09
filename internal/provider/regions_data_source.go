package provider

import (
	"context"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type regionsDataSource struct {
	client *client.Client
}

type regionsModel struct {
	Cloud            types.String `tfsdk:"cloud"`
	Engine           types.String `tfsdk:"engine"`
	PrivateEndpoints types.Bool   `tfsdk:"private_endpoints"`
	Regions          types.List   `tfsdk:"regions"`
}

func newRegionsDataSource() datasource.DataSource {
	return &regionsDataSource{}
}

func (d *regionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_regions"
}

func (d *regionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the cloud regions the platform serves, with the database engines and whether private endpoints are available in each. Every filter is optional.",
		Attributes: map[string]schema.Attribute{
			"cloud":             schema.StringAttribute{Optional: true, MarkdownDescription: "Only regions of this cloud, for example `aws`."},
			"engine":            schema.StringAttribute{Optional: true, MarkdownDescription: "Only regions that serve this database engine, for example `postgresql`. Case-insensitive."},
			"private_endpoints": schema.BoolAttribute{Optional: true, MarkdownDescription: "`true` keeps only regions with private endpoints, `false` only regions without."},
			"regions": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Matching regions, ordered by cloud and region.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"cloud":             schema.StringAttribute{Computed: true, MarkdownDescription: "Cloud provider."},
					"region":            schema.StringAttribute{Computed: true, MarkdownDescription: "Cloud region."},
					"engines":           schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Database engines served, lowercase."},
					"private_endpoints": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a `relay_private_endpoint_service` can be created here."},
				}},
			},
		},
	}
}

func (d *regionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *regionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config regionsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	catalog, err := regionCatalog(ctx, d.client)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list regions", err.Error())
		return
	}
	engine := strings.ToLower(config.Engine.ValueString())
	matches := []attr.Value{}
	for _, r := range catalog {
		switch {
		case !config.Cloud.IsNull() && r.cloud != config.Cloud.ValueString():
		case !config.Engine.IsNull() && !slices.Contains(r.engines, engine):
		case !config.PrivateEndpoints.IsNull() && r.privateEndpoints != config.PrivateEndpoints.ValueBool():
		default:
			matches = append(matches, r.value())
		}
	}
	config.Regions = types.ListValueMust(types.ObjectType{AttrTypes: regionAttrTypes}, matches)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
