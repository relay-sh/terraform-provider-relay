package provider

import (
	"context"
	"os"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type relayProvider struct {
	version string
	dial    func(relay.Options) (*relay.Client, error)
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIKey   types.String `tfsdk:"api_key"`
	OrgID    types.String `tfsdk:"org_id"`
	Insecure types.Bool   `tfsdk:"insecure"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &relayProvider{version: version, dial: relay.Dial}
	}
}

func (p *relayProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "relay"
	resp.Version = p.version
}

func (p *relayProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages Relay workspaces, API keys, connections, managed PostgreSQL clusters and workspace network access.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Organization or workspace API key (`rly_key_…`). Falls back to `RELAY_API_KEY`.",
				Optional:            true,
				Sensitive:           true,
			},
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "Relay API address as `host:port`. Defaults to `api.userelay.sh:443`. Falls back to `RELAY_ENDPOINT`.",
				Optional:            true,
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization to manage. Defaults to the API key's organization. Falls back to `RELAY_ORG_ID`.",
				Optional:            true,
			},
			"insecure": schema.BoolAttribute{
				MarkdownDescription: "Dial without TLS. Only for a local server. Falls back to `RELAY_INSECURE`.",
				Optional:            true,
			},
		},
	}
}

func (p *relayProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiKey := stringSetting(cfg.APIKey, "RELAY_API_KEY", path.Root("api_key"), true, resp)
	endpoint := stringSetting(cfg.Endpoint, "RELAY_ENDPOINT", path.Root("endpoint"), false, resp)
	orgID := stringSetting(cfg.OrgID, "RELAY_ORG_ID", path.Root("org_id"), false, resp)
	insecure := boolSetting(cfg.Insecure, "RELAY_INSECURE", path.Root("insecure"), resp)
	if resp.Diagnostics.HasError() {
		return
	}

	api, err := p.dial(relay.Options{
		Endpoint:    endpoint,
		Credentials: relay.APIKey(apiKey),
		Insecure:    insecure,
		Origin:      "terraform",
		UserAgent:   "terraform-provider-relay/" + p.version,
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create Relay client", err.Error())
		return
	}

	data := &client.Client{API: api.Server, OrgID: orgID}
	resp.DataSourceData = data
	resp.ResourceData = data
}

func (p *relayProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newWorkspaceResource,
		newAPIKeyResource,
		newConnectionResource,
		newPostgreSQLClusterResource,
		newPostgreSQLUserResource,
		newPostgreSQLDatabaseResource,
		newValkeyClusterResource,
		newValkeyUserResource,
		newIPAccessListResource,
		newPrivateEndpointServiceResource,
		newPrivateEndpointResource,
	}
}

func (p *relayProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newOrganizationDataSource,
		newWorkspaceDataSource,
		newAPIKeyDataSource,
		newPostgreSQLClusterDataSource,
		newValkeyClusterDataSource,
		newIPAccessListDataSource,
		newPrivateEndpointServiceDataSource,
		newPrivateEndpointDataSource,
		newRegionsDataSource,
		newCloudProvidersDataSource,
	}
}

func stringSetting(v types.String, env string, attr path.Path, required bool, resp *provider.ConfigureResponse) string {
	if v.IsUnknown() {
		resp.Diagnostics.AddAttributeError(attr, "Unknown provider setting", "The value is not known until apply. Set it from a static value or "+env+".")
		return ""
	}
	value := v.ValueString()
	if value == "" {
		value = os.Getenv(env)
	}
	if value == "" && required {
		resp.Diagnostics.AddAttributeError(attr, "Missing provider setting", "Set it in the provider block or through "+env+".")
	}
	return value
}

func boolSetting(v types.Bool, env string, attr path.Path, resp *provider.ConfigureResponse) bool {
	if v.IsUnknown() {
		resp.Diagnostics.AddAttributeError(attr, "Unknown provider setting", "The value is not known until apply. Set it from a static value or "+env+".")
		return false
	}
	if !v.IsNull() {
		return v.ValueBool()
	}
	raw := os.Getenv(env)
	if raw == "" {
		return false
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		resp.Diagnostics.AddAttributeError(attr, "Invalid "+env, "Expected true or false, got "+strconv.Quote(raw)+".")
	}
	return parsed
}
