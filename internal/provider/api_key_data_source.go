package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type apiKeyDataSource struct {
	client *client.Client
}

type apiKeyDataModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	WorkspaceID  types.String `tfsdk:"workspace_id"`
	Role         types.String `tfsdk:"role"`
	AllowedCIDRs types.Set    `tfsdk:"allowed_cidrs"`
	ExpiresAt    types.String `tfsdk:"expires_at"`
	LastUsedAt   types.String `tfsdk:"last_used_at"`
	ParentKeyID  types.String `tfsdk:"parent_key_id"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

func newAPIKeyDataSource() datasource.DataSource {
	return &apiKeyDataSource{}
}

func (d *apiKeyDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (d *apiKeyDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing organization or workspace API key by ID or name. The secret is never readable.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "API key ID."},
			"name":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "API key name. Must match exactly one key."},
			"workspace_id":  schema.StringAttribute{Optional: true, MarkdownDescription: "Workspace to look in. Omit it for organization keys."},
			"role":          schema.StringAttribute{Computed: true, MarkdownDescription: "Role slug granted to the key."},
			"allowed_cidrs": schema.SetAttribute{ElementType: types.StringType, Computed: true, MarkdownDescription: "CIDR blocks the key may be used from; empty allows any address."},
			"expires_at":    schema.StringAttribute{Computed: true, MarkdownDescription: "Expiry, RFC 3339. Null when the key never expires."},
			"last_used_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Last use, RFC 3339. Null when never used."},
			"parent_key_id": schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the API key that created this one, empty when a user did."},
			"created_at":    schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time, RFC 3339."},
		},
	}
}

func (d *apiKeyDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return exactlyOneOf("id", "name")
}

func (d *apiKeyDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *apiKeyDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config apiKeyDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	keys, err := listAPIKeys(ctx, d.client, config.WorkspaceID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot read API key", err.Error())
		return
	}
	var matches []*serverv1.APIKey
	for _, k := range keys {
		if k.GetId() == config.ID.ValueString() || (!config.Name.IsNull() && k.GetName() == config.Name.ValueString()) {
			matches = append(matches, k)
		}
	}
	switch len(matches) {
	case 0:
		resp.Diagnostics.AddError("API key not found", "No API key matches in the given scope, or the provider's key cannot see it.")
		return
	case 1:
	default:
		resp.Diagnostics.AddError("Ambiguous API key name", "More than one API key is named "+config.Name.ValueString()+". Look it up by id.")
		return
	}
	k := matches[0]
	state := apiKeyDataModel{
		ID:           types.StringValue(k.GetId()),
		Name:         types.StringValue(k.GetName()),
		WorkspaceID:  config.WorkspaceID,
		Role:         types.StringValue(k.GetRoleSlug()),
		AllowedCIDRs: stringSetFromGo(k.GetAllowedCidrs()),
		ExpiresAt:    optionalTimestamp(k.GetExpiresAt()),
		LastUsedAt:   optionalTimestamp(k.GetLastUsedAt()),
		ParentKeyID:  types.StringValue(k.GetParentKeyId()),
		CreatedAt:    timestampValue(k.GetCreatedAt()),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
