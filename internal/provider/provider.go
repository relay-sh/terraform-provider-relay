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
	Token    types.String `tfsdk:"token"`
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
		MarkdownDescription: "Manages Relay workspaces, connections and managed databases.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "Relay API address as `host:port`. Falls back to `RELAY_ENDPOINT`.",
				Optional:            true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Personal access token (`rly_pat_…`). Falls back to `RELAY_TOKEN`.",
				Optional:            true,
				Sensitive:           true,
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization every resource is managed in. Falls back to `RELAY_ORG_ID`.",
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

	endpoint := stringSetting(cfg.Endpoint, "RELAY_ENDPOINT", path.Root("endpoint"), resp)
	token := stringSetting(cfg.Token, "RELAY_TOKEN", path.Root("token"), resp)
	orgID := stringSetting(cfg.OrgID, "RELAY_ORG_ID", path.Root("org_id"), resp)
	insecure := boolSetting(cfg.Insecure, "RELAY_INSECURE", path.Root("insecure"), resp)
	if resp.Diagnostics.HasError() {
		return
	}

	api, err := p.dial(relay.Options{
		Endpoint:  endpoint,
		Token:     token,
		Insecure:  insecure,
		Origin:    "terraform",
		UserAgent: "terraform-provider-relay/" + p.version,
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
	return nil
}

func (p *relayProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newOrganizationDataSource,
	}
}

func stringSetting(v types.String, env string, attr path.Path, resp *provider.ConfigureResponse) string {
	if v.IsUnknown() {
		resp.Diagnostics.AddAttributeError(attr, "Unknown provider setting", "The value is not known until apply. Set it from a static value or "+env+".")
		return ""
	}
	value := v.ValueString()
	if value == "" {
		value = os.Getenv(env)
	}
	if value == "" {
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
