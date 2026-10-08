package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type privateEndpointDataSource struct {
	client *client.Client
}

func newPrivateEndpointDataSource() datasource.DataSource {
	return &privateEndpointDataSource{}
}

func (d *privateEndpointDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_private_endpoint"
}

func (d *privateEndpointDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computed := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an endpoint registered on a private endpoint service, by ID or by VPC endpoint ID.",
		Attributes: map[string]schema.Attribute{
			"workspace_id":      schema.StringAttribute{Required: true, MarkdownDescription: "Workspace to look in."},
			"service_id":        schema.StringAttribute{Required: true, MarkdownDescription: "Service the endpoint is registered on."},
			"id":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Endpoint ID."},
			"vpc_endpoint_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "AWS interface VPC endpoint ID."},
			"cloud":             computed("Cloud provider."),
			"region":            computed("Cloud region."),
			"index":             schema.Int64Attribute{Computed: true, MarkdownDescription: "Index in the endpoint's hostnames."},
			"connection_status": computed("Connection status."),
			"dns_name":          computed("DNS name the endpoint's hostnames point at."),
			"error_message":     computed("Why the connection failed, when it did."),
			"created_at":        computed("Creation time, RFC 3339."),
		},
	}
}

func (d *privateEndpointDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "vpc_endpoint_id")
}

func (d *privateEndpointDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *privateEndpointDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config privateEndpointModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	workspaceID, serviceID := config.WorkspaceID.ValueString(), config.ServiceID.ValueString()
	var ep *serverv1.PrivateEndpoint
	var err error
	if config.ID.IsNull() {
		ep, err = privateEndpointByVPCEndpoint(ctx, d.client, workspaceID, serviceID, config.VPCEndpointID.ValueString())
	} else {
		ep, err = privateEndpoint(ctx, d.client, workspaceID, serviceID, config.ID.ValueString())
	}
	if ep == nil && (err == nil || relay.IsNotFound(err)) {
		resp.Diagnostics.AddError("Private endpoint not found", "No endpoint matches on the service, or the token cannot see it.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read private endpoint", err.Error())
		return
	}
	state := privateEndpointFromProto(config.WorkspaceID, ep)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
