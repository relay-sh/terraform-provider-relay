package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

var serviceEndpointAttrTypes = map[string]attr.Type{
	"id":                types.StringType,
	"vpc_endpoint_id":   types.StringType,
	"index":             types.Int64Type,
	"connection_status": types.StringType,
	"dns_name":          types.StringType,
	"error_message":     types.StringType,
}

type privateEndpointServiceDataSource struct {
	client *client.Client
}

type privateEndpointServiceDataSourceModel struct {
	privateEndpointServiceModel
	Endpoints types.List `tfsdk:"endpoints"`
}

func newPrivateEndpointServiceDataSource() datasource.DataSource {
	return &privateEndpointServiceDataSource{}
}

func (d *privateEndpointServiceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_private_endpoint_service"
}

func (d *privateEndpointServiceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computed := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a workspace's private endpoint service by ID or by cloud and region, with the endpoints registered on it.",
		Attributes: map[string]schema.Attribute{
			"workspace_id":          schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"id":                    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Service ID."},
			"cloud":                 schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cloud provider. Set together with `region`."},
			"region":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Cloud region. Set together with `cloud`."},
			"status":                computed("`INITIATING`, `AVAILABLE`, `FAILED` or `DELETING`."),
			"endpoint_service_name": computed("AWS VPC endpoint service name."),
			"error_message":         computed("Why the service failed, when it did."),
			"created_at":            computed("Creation time, RFC 3339."),
			"endpoints": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Endpoints registered on the service, in index order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":                computed("Endpoint ID."),
					"vpc_endpoint_id":   computed("AWS interface VPC endpoint ID."),
					"index":             schema.Int64Attribute{Computed: true, MarkdownDescription: "Index in the endpoint's hostnames."},
					"connection_status": computed("Connection status."),
					"dns_name":          computed("DNS name the endpoint's hostnames point at."),
					"error_message":     computed("Why the connection failed, when it did."),
				}},
			},
		},
	}
}

func (d *privateEndpointServiceDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("cloud")),
		datasourcevalidator.RequiredTogether(path.MatchRoot("cloud"), path.MatchRoot("region")),
	}
}

func (d *privateEndpointServiceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *privateEndpointServiceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config privateEndpointServiceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	workspaceID := config.WorkspaceID.ValueString()
	var svc *serverv1.PrivateEndpointService
	var err error
	if config.ID.IsNull() {
		svc, err = privateEndpointServiceIn(ctx, d.client, workspaceID, config.Cloud.ValueString(), config.Region.ValueString())
	} else {
		svc, err = privateEndpointService(ctx, d.client, workspaceID, config.ID.ValueString())
	}
	if svc == nil && (err == nil || relay.IsNotFound(err)) {
		resp.Diagnostics.AddError("Private endpoint service not found", "No service matches in the workspace, or the token cannot see it.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read private endpoint service", err.Error())
		return
	}
	endpoints := make([]attr.Value, 0, len(svc.GetEndpoints()))
	for _, e := range svc.GetEndpoints() {
		endpoints = append(endpoints, types.ObjectValueMust(serviceEndpointAttrTypes, map[string]attr.Value{
			"id":                types.StringValue(e.GetId()),
			"vpc_endpoint_id":   types.StringValue(e.GetInterfaceEndpointId()),
			"index":             types.Int64Value(int64(e.GetIndex())),
			"connection_status": types.StringValue(e.GetConnectionStatus()),
			"dns_name":          types.StringValue(e.GetDnsName()),
			"error_message":     types.StringValue(e.GetErrorMessage()),
		}))
	}
	state := privateEndpointServiceDataSourceModel{
		privateEndpointServiceFromProto(config.WorkspaceID, svc),
		types.ListValueMust(types.ObjectType{AttrTypes: serviceEndpointAttrTypes}, endpoints),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
