package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type connectionResource struct {
	client *client.Client
}

type connectionModel struct {
	ID                types.String      `tfsdk:"id"`
	WorkspaceID       types.String      `tfsdk:"workspace_id"`
	Name              types.String      `tfsdk:"name"`
	Engine            types.String      `tfsdk:"engine"`
	Host              types.String      `tfsdk:"host"`
	User              types.String      `tfsdk:"user"`
	Password          types.String      `tfsdk:"password"`
	CredentialRef     types.String      `tfsdk:"credential_ref"`
	CredentialSource  types.String      `tfsdk:"credential_source"`
	Database          types.String      `tfsdk:"database"`
	Listen            types.String      `tfsdk:"listen"`
	AgentPool         types.String      `tfsdk:"agent_pool"`
	ManagedDatabaseID types.String      `tfsdk:"managed_database_id"`
	RedactEnabled     types.Bool        `tfsdk:"redact_enabled"`
	AuditEnabled      types.Bool        `tfsdk:"audit_enabled"`
	PolicyFailMode    types.String      `tfsdk:"policy_fail_mode"`
	RedactRules       []redactRuleModel `tfsdk:"redact_rules"`
	TLSCert           types.String      `tfsdk:"tls_cert"`
	TLSKey            types.String      `tfsdk:"tls_key"`
	TLSCA             types.String      `tfsdk:"tls_ca"`
	HostTLSCA         types.String      `tfsdk:"host_tls_ca"`
	Status            types.String      `tfsdk:"status"`
	StatusError       types.String      `tfsdk:"status_error"`
	PasswordSet       types.Bool        `tfsdk:"password_set"`
	ManagedBy         types.String      `tfsdk:"managed_by"`
}

type redactRuleModel struct {
	ColumnPattern types.String `tfsdk:"column_pattern"`
	Strategy      types.String `tfsdk:"strategy"`
	PartialShow   types.Int64  `tfsdk:"partial_show"`
}

func newConnectionResource() resource.Resource {
	return &connectionResource{}
}

func (r *connectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_connection"
}

func (r *connectionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	emptyString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: description}
	}
	computedString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: description}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A database connection reached through a Relay agent pool. Upstream TLS is always on; only its CA can be configured.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Connection ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the connection belongs to."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Name, unique in the workspace. Changing it replaces the connection."},
			"engine":       schema.StringAttribute{Required: true, MarkdownDescription: "Database engine, for example `postgres` or `mongodb`."},
			"host":         schema.StringAttribute{Required: true, MarkdownDescription: "Upstream `host:port`."},
			"agent_pool":   schema.StringAttribute{Required: true, MarkdownDescription: "Agent pool that proxies this connection."},
			"user":         emptyString("Upstream user."),
			"password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Upstream password. Write-only on the API: changes made outside Terraform are not detected. Prefer `credential_ref`.",
			},
			"credential_ref":      emptyString("Reference the agent resolves to a credential. Takes precedence over `password`."),
			"credential_source":   emptyString("Name of the external secret source that resolves `credential_ref`. Empty uses the agent's built-in source."),
			"database":            emptyString("Default database name."),
			"listen":              emptyString("Local listen address on the agent."),
			"managed_database_id": schema.StringAttribute{Optional: true, MarkdownDescription: "Managed database this connection belongs to."},
			"redact_enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Apply `redact_rules` to query results.",
			},
			"audit_enabled": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "Record queries in the audit log.",
			},
			"policy_fail_mode": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Validators:          []validator.String{stringvalidator.OneOf("", "open", "closed")},
				MarkdownDescription: "What the query firewall does with a query it cannot parse. Empty means `closed`.",
			},
			"redact_rules": schema.ListNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Column masking rules. Removing the attribute clears them.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"column_pattern": schema.StringAttribute{Required: true, MarkdownDescription: "Column name or glob."},
						"strategy":       schema.StringAttribute{Required: true, MarkdownDescription: "Masking strategy."},
						"partial_show": schema.Int64Attribute{
							Optional: true, Computed: true, Default: int64default.StaticInt64(0),
							MarkdownDescription: "Characters left visible by partial strategies.",
						},
					},
				},
			},
			"tls_cert":     emptyString("Client certificate, PEM."),
			"tls_key":      schema.StringAttribute{Optional: true, Computed: true, Sensitive: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Client key, PEM."},
			"tls_ca":       emptyString("CA bundle for the client certificate, PEM."),
			"host_tls_ca":  emptyString("CA bundle used to verify the upstream server, PEM."),
			"status":       computedString("`healthy`, `error` or `unknown`, as last reported by the agent."),
			"status_error": computedString("Last error reported by the agent."),
			"password_set": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a password is stored."},
			"managed_by": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(managedBy),
				MarkdownDescription: "Owner tag the dashboard shows. Defaults to `terraform`; set it to another tool or an empty string.",
			},
		},
	}
}

func (r *connectionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *connectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan connectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.CreateConnection(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), &serverv1.CreateConnectionRequest{
		Connection: &serverv1.Connection{
			Name:              plan.Name.ValueString(),
			Engine:            plan.Engine.ValueString(),
			Host:              plan.Host.ValueString(),
			User:              plan.User.ValueString(),
			Password:          plan.Password.ValueString(),
			CredentialRef:     plan.CredentialRef.ValueString(),
			CredentialSource:  plan.CredentialSource.ValueString(),
			Database:          plan.Database.ValueString(),
			Listen:            plan.Listen.ValueString(),
			AgentPool:         plan.AgentPool.ValueString(),
			ManagedDatabaseId: plan.ManagedDatabaseID.ValueString(),
			ManagedBy:         plan.ManagedBy.ValueString(),
			Config: &serverv1.ConnectionConfig{
				RedactEnabled:  plan.RedactEnabled.ValueBool(),
				AuditEnabled:   plan.AuditEnabled.ValueBool(),
				PolicyFailMode: plan.PolicyFailMode.ValueString(),
				RedactRules:    redactRulesToProto(plan.RedactRules),
			},
			Tls:     &serverv1.TLSConfig{Cert: plan.TLSCert.ValueString(), Key: plan.TLSKey.ValueString(), Ca: plan.TLSCA.ValueString()},
			HostTls: &serverv1.HostTLSConfig{Enabled: true, Ca: plan.HostTLSCA.ValueString()},
		},
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create connection", err.Error())
		return
	}
	state := connectionFromProto(out.GetConnection(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *connectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state connectionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.GetConnections(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.GetConnectionsRequest{
		Names: []string{state.Name.ValueString()},
	})
	if relay.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read connection", err.Error())
		return
	}
	state = connectionFromProto(out.GetConnections()[0], state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *connectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior connectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	update := &serverv1.UpdateConnectionRequest{
		Name:             plan.Name.ValueString(),
		Engine:           plan.Engine.ValueStringPointer(),
		Host:             plan.Host.ValueStringPointer(),
		User:             plan.User.ValueStringPointer(),
		CredentialRef:    plan.CredentialRef.ValueStringPointer(),
		CredentialSource: plan.CredentialSource.ValueStringPointer(),
		Database:         plan.Database.ValueStringPointer(),
		Listen:           plan.Listen.ValueStringPointer(),
		AgentPool:        plan.AgentPool.ValueStringPointer(),
		RedactEnabled:    plan.RedactEnabled.ValueBoolPointer(),
		AuditEnabled:     plan.AuditEnabled.ValueBoolPointer(),
		PolicyFailMode:   plan.PolicyFailMode.ValueStringPointer(),
		RedactRules:      redactRulesToProto(plan.RedactRules),
		ClearRedactRules: len(plan.RedactRules) == 0,
		Tls:              &serverv1.TLSConfig{Cert: plan.TLSCert.ValueString(), Key: plan.TLSKey.ValueString(), Ca: plan.TLSCA.ValueString()},
		HostTls:          &serverv1.HostTLSConfig{Enabled: true, Ca: plan.HostTLSCA.ValueString()},
		ManagedBy:        plan.ManagedBy.ValueStringPointer(),
	}
	if !plan.Password.Equal(prior.Password) {
		update.Password = stringPtr(plan.Password.ValueString())
	}
	if plan.ManagedDatabaseID.IsNull() {
		update.ClearManagedDatabaseId = !prior.ManagedDatabaseID.IsNull()
	} else {
		update.ManagedDatabaseId = plan.ManagedDatabaseID.ValueStringPointer()
	}

	out, err := r.client.API.UpdateConnection(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), update)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update connection", err.Error())
		return
	}
	state := connectionFromProto(out.GetConnection(), plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *connectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state connectionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.DeleteConnection(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.DeleteConnectionRequest{Name: state.Name.ValueString()})
	if err != nil && !relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot delete connection", err.Error())
	}
}

func (r *connectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	workspaceID, name, ok := strings.Cut(req.ID, "/")
	if !ok || workspaceID == "" || name == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<name>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), workspaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
}

func connectionFromProto(c *serverv1.Connection, prior connectionModel) connectionModel {
	cfg := c.GetConfig()
	return connectionModel{
		ID:                types.StringValue(c.GetId()),
		WorkspaceID:       prior.WorkspaceID,
		Name:              types.StringValue(c.GetName()),
		Engine:            types.StringValue(c.GetEngine()),
		Host:              types.StringValue(c.GetHost()),
		User:              types.StringValue(c.GetUser()),
		Password:          prior.Password,
		CredentialRef:     types.StringValue(c.GetCredentialRef()),
		CredentialSource:  types.StringValue(c.GetCredentialSource()),
		Database:          types.StringValue(c.GetDatabase()),
		Listen:            types.StringValue(c.GetListen()),
		AgentPool:         types.StringValue(c.GetAgentPool()),
		ManagedDatabaseID: nullIfEmpty(c.GetManagedDatabaseId()),
		RedactEnabled:     types.BoolValue(cfg.GetRedactEnabled()),
		AuditEnabled:      types.BoolValue(cfg.GetAuditEnabled()),
		PolicyFailMode:    types.StringValue(cfg.GetPolicyFailMode()),
		RedactRules:       redactRulesFromProto(cfg.GetRedactRules(), prior.RedactRules),
		TLSCert:           types.StringValue(c.GetTls().GetCert()),
		TLSKey:            types.StringValue(c.GetTls().GetKey()),
		TLSCA:             types.StringValue(c.GetTls().GetCa()),
		HostTLSCA:         types.StringValue(c.GetHostTls().GetCa()),
		Status:            types.StringValue(c.GetStatus()),
		StatusError:       types.StringValue(c.GetStatusError()),
		PasswordSet:       types.BoolValue(c.GetPasswordSet()),
		ManagedBy:         types.StringValue(c.GetManagedBy()),
	}
}

func redactRulesToProto(rules []redactRuleModel) []*serverv1.RedactRule {
	out := make([]*serverv1.RedactRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, &serverv1.RedactRule{
			ColumnPattern: r.ColumnPattern.ValueString(),
			Strategy:      r.Strategy.ValueString(),
			PartialShow:   int32(r.PartialShow.ValueInt64()),
		})
	}
	return out
}

func redactRulesFromProto(rules []*serverv1.RedactRule, prior []redactRuleModel) []redactRuleModel {
	if len(rules) == 0 {
		if prior != nil {
			return []redactRuleModel{}
		}
		return nil
	}
	out := make([]redactRuleModel, 0, len(rules))
	for _, r := range rules {
		out = append(out, redactRuleModel{
			ColumnPattern: types.StringValue(r.GetColumnPattern()),
			Strategy:      types.StringValue(r.GetStrategy()),
			PartialShow:   types.Int64Value(int64(r.GetPartialShow())),
		})
	}
	return out
}

func nullIfEmpty(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
