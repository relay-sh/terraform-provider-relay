package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type organizationDataSource struct {
	client *client.Client
}

type organizationModel struct {
	ID                        types.String `tfsdk:"id"`
	Slug                      types.String `tfsdk:"slug"`
	Name                      types.String `tfsdk:"name"`
	Timezone                  types.String `tfsdk:"timezone"`
	Locale                    types.String `tfsdk:"locale"`
	SessionLengthHours        types.Int64  `tfsdk:"session_length_hours"`
	CLITokenTTLHours          types.Int64  `tfsdk:"cli_token_ttl_hours"`
	AuditRetentionDays        types.Int64  `tfsdk:"audit_retention_days"`
	SessionIdleTimeoutMinutes types.Int64  `tfsdk:"session_idle_timeout_minutes"`
	SessionAbsoluteMaxHours   types.Int64  `tfsdk:"session_absolute_max_hours"`
}

func newOrganizationDataSource() datasource.DataSource {
	return &organizationDataSource{}
}

func (d *organizationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization"
}

func (d *organizationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an organization the token's user belongs to. Without `id` or `slug` it reads the provider's `org_id`.",
		Attributes: map[string]schema.Attribute{
			"id":                           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Organization ID."},
			"slug":                         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Organization slug."},
			"name":                         schema.StringAttribute{Computed: true, MarkdownDescription: "Display name."},
			"timezone":                     schema.StringAttribute{Computed: true, MarkdownDescription: "Default timezone."},
			"locale":                       schema.StringAttribute{Computed: true, MarkdownDescription: "Default locale."},
			"session_length_hours":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Dashboard session length, in hours."},
			"cli_token_ttl_hours":          schema.Int64Attribute{Computed: true, MarkdownDescription: "CLI login lifetime, in hours."},
			"audit_retention_days":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Audit log retention. 0 keeps logs forever."},
			"session_idle_timeout_minutes": schema.Int64Attribute{Computed: true, MarkdownDescription: "Dashboard idle timeout. 0 uses the default."},
			"session_absolute_max_hours":   schema.Int64Attribute{Computed: true, MarkdownDescription: "Dashboard absolute session cap. 0 uses the default."},
		},
	}
}

func (d *organizationDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.Conflicting(path.MatchRoot("id"), path.MatchRoot("slug")),
	}
}

func (d *organizationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *organizationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var state organizationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	lookup := &serverv1.GetOrganizationRequest{Key: &serverv1.GetOrganizationRequest_Id{Id: d.client.OrgID}}
	switch {
	case !state.Slug.IsNull():
		lookup.Key = &serverv1.GetOrganizationRequest_Slug{Slug: state.Slug.ValueString()}
	case !state.ID.IsNull():
		lookup.Key = &serverv1.GetOrganizationRequest_Id{Id: state.ID.ValueString()}
	}

	out, err := d.client.API.GetOrganization(d.client.Org(ctx), lookup)
	if relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Organization not found", "No organization matches, or the token's user is not a member of it.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read organization", err.Error())
		return
	}

	org := out.GetOrganization()
	state = organizationModel{
		ID:                        types.StringValue(org.GetId()),
		Slug:                      types.StringValue(org.GetSlug()),
		Name:                      types.StringValue(org.GetName()),
		Timezone:                  types.StringValue(org.GetTimezone()),
		Locale:                    types.StringValue(org.GetLocale()),
		SessionLengthHours:        types.Int64Value(int64(org.GetSessionLengthHours())),
		CLITokenTTLHours:          types.Int64Value(int64(org.GetCliTokenTtlHours())),
		AuditRetentionDays:        types.Int64Value(int64(org.GetAuditRetentionDays())),
		SessionIdleTimeoutMinutes: types.Int64Value(int64(org.GetSessionIdleTimeoutMinutes())),
		SessionAbsoluteMaxHours:   types.Int64Value(int64(org.GetSessionAbsoluteMaxHours())),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
