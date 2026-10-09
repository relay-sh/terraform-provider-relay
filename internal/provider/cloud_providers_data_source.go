package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

var cloudProviderAttrTypes = map[string]attr.Type{
	"name":    types.StringType,
	"regions": types.ListType{ElemType: types.StringType},
}

type cloudProvidersDataSource struct {
	client *client.Client
}

type cloudProvidersModel struct {
	CloudProviders types.List `tfsdk:"cloud_providers"`
}

func newCloudProvidersDataSource() datasource.DataSource {
	return &cloudProvidersDataSource{}
}

func (d *cloudProvidersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cloud_providers"
}

func (d *cloudProvidersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the cloud providers the platform serves, with their regions. `relay_regions` has the detail per region.",
		Attributes: map[string]schema.Attribute{
			"cloud_providers": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Cloud providers, ordered by name.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":    schema.StringAttribute{Computed: true, MarkdownDescription: "Cloud provider, as `cloud` takes it elsewhere, for example `aws`."},
					"regions": schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Regions served, ordered."},
				}},
			},
		},
	}
}

func (d *cloudProvidersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *cloudProvidersDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	catalog, err := regionCatalog(ctx, d.client)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list cloud providers", err.Error())
		return
	}
	var names []string
	regions := map[string][]attr.Value{}
	for _, r := range catalog {
		if _, seen := regions[r.cloud]; !seen {
			names = append(names, r.cloud)
		}
		regions[r.cloud] = append(regions[r.cloud], types.StringValue(r.name))
	}
	clouds := make([]attr.Value, 0, len(names))
	for _, n := range names {
		clouds = append(clouds, types.ObjectValueMust(cloudProviderAttrTypes, map[string]attr.Value{
			"name":    types.StringValue(n),
			"regions": types.ListValueMust(types.StringType, regions[n]),
		}))
	}
	state := cloudProvidersModel{CloudProviders: types.ListValueMust(types.ObjectType{AttrTypes: cloudProviderAttrTypes}, clouds)}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
