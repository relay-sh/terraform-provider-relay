package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

type connectionResource struct {
	client *client.Client
}

type connectionModel struct {
	ID                 types.String      `tfsdk:"id"`
	WorkspaceID        types.String      `tfsdk:"workspace_id"`
	Name               types.String      `tfsdk:"name"`
	Engine             types.String      `tfsdk:"engine"`
	Host               types.String      `tfsdk:"host"`
	User               types.String      `tfsdk:"user"`
	Password           types.String      `tfsdk:"password"`
	CredentialRef      types.String      `tfsdk:"credential_ref"`
	CredentialSource   types.String      `tfsdk:"credential_source"`
	Database           types.String      `tfsdk:"database"`
	Listen             types.String      `tfsdk:"listen"`
	AgentPool          types.String      `tfsdk:"agent_pool"`
	ManagedDatabaseID  types.String      `tfsdk:"managed_database_id"`
	RedactEnabled      types.Bool        `tfsdk:"redact_enabled"`
	AuditEnabled       types.Bool        `tfsdk:"audit_enabled"`
	PolicyFailMode     types.String      `tfsdk:"policy_fail_mode"`
	RedactRules        []redactRuleModel `tfsdk:"redact_rules"`
	TLSCert            types.String      `tfsdk:"tls_cert"`
	TLSKey             types.String      `tfsdk:"tls_key"`
	TLSCA              types.String      `tfsdk:"tls_ca"`
	HostTLSCA          types.String      `tfsdk:"host_tls_ca"`
	Status             types.String      `tfsdk:"status"`
	StatusError        types.String      `tfsdk:"status_error"`
	PasswordSet        types.Bool        `tfsdk:"password_set"`
	ManagedBy          types.String      `tfsdk:"managed_by"`
	DatabaseUser       types.String      `tfsdk:"database_user"`
	DeletionProtection types.Bool        `tfsdk:"deletion_protection"`
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
		MarkdownDescription: "A database connection reached through a Relay agent. Upstream TLS is always on; only its CA can be configured.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Connection ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the connection belongs to."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Name, unique in the workspace. Changing it replaces the connection."},
			"engine":       schema.StringAttribute{Required: true, MarkdownDescription: "Database engine: `postgres`, `mongodb`, `mysql`, `valkey` (Valkey and Redis), `sqlserver` or `oracle`."},
			"host":         schema.StringAttribute{Optional: true, MarkdownDescription: "Upstream `host:port`. Required unless `managed_database_id` and `user` are set, which creates the connection on that database user and lets the platform set the address. The API does not return it for a connection with `managed_database_id`, so Terraform keeps the configured value, and an import leaves it empty until the next apply."},
			"agent_pool":   schema.StringAttribute{Required: true, MarkdownDescription: "The agent (pool) that serves this connection, for example `default`, the workspace's managed agent."},
			"user":         emptyString("Upstream user. With `managed_database_id` and no `host`, the PostgreSQL user of the managed database the connection is created on."),
			"password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Upstream password. Write-only on the API: changes made outside Terraform are not detected. Prefer `credential_ref`. Not allowed on a connection created for a database user.",
			},
			"credential_ref":      emptyString("Reference the agent resolves to a credential. Takes precedence over `password`. Set by the platform, and not allowed, on a connection created for a database user."),
			"credential_source":   emptyString("Name of the external secret source that resolves `credential_ref`. Empty uses the agent's built-in source. Set by the platform, and not allowed, on a connection created for a database user."),
			"database":            emptyString("Default database name. For a connection created for a database user, the logical database it is pinned to."),
			"listen":              emptyString("Local listen address on the agent."),
			"managed_database_id": schema.StringAttribute{Optional: true, MarkdownDescription: "Managed database this connection belongs to. Together with `user` and no `host`, the connection is created on that database user: the platform sets the address, CA and credential, and `host`, `credential_ref`, `credential_source`, `password`, `tls_cert`, `tls_key`, `tls_ca` and `host_tls_ca` are not allowed."},
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
			"tls_cert":            emptyString("Client certificate, PEM. Set by the platform, and not allowed, on a connection created for a database user."),
			"tls_key":             schema.StringAttribute{Optional: true, Computed: true, Sensitive: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Client key, PEM. Set by the platform, and not allowed, on a connection created for a database user."},
			"tls_ca":              emptyString("CA bundle for the client certificate, PEM. Set by the platform, and not allowed, on a connection created for a database user."),
			"host_tls_ca":         emptyString("CA bundle used to verify the upstream server, PEM. Set by the platform, and not allowed, on a connection created for a database user."),
			"status":              computedString("`healthy`, `error` or `unknown`, as last reported by the agent."),
			"status_error":        computedString("Last error reported by the agent."),
			"database_user":       schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, MarkdownDescription: "PostgreSQL user of the managed database that the platform bound this connection to. Empty for a connection that is not created for a database user."},
			"password_set":        schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a password is stored."},
			"deletion_protection": deletionProtectionAttribute("connection"),
			"managed_by": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(managedBy),
				MarkdownDescription: "Owner tag the dashboard shows. Defaults to `terraform`; set it to another tool or an empty string.",
			},
		},
	}
}

var (
	databaseUserServerSet = []string{"credential_ref", "credential_source", "tls_cert", "tls_key", "tls_ca", "host_tls_ca"}
	databaseUserForbidden = append([]string{"password"}, databaseUserServerSet...)
)

func (r *connectionResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var host, managedDatabaseID, user types.String
	req.Config.GetAttribute(ctx, path.Root("host"), &host)
	req.Config.GetAttribute(ctx, path.Root("managed_database_id"), &managedDatabaseID)
	req.Config.GetAttribute(ctx, path.Root("user"), &user)
	if host.IsUnknown() || user.IsUnknown() || host.ValueString() != "" {
		return
	}
	if !isDatabaseUserConnection(host, managedDatabaseID, user) {
		resp.Diagnostics.AddAttributeError(path.Root("host"), "Missing host", "host is required unless managed_database_id and user are set, which creates the connection on that database user.")
		return
	}
	for _, name := range databaseUserForbidden {
		var value types.String
		req.Config.GetAttribute(ctx, path.Root(name), &value)
		if !value.IsUnknown() && value.ValueString() != "" {
			resp.Diagnostics.AddAttributeError(path.Root(name), "Set by the platform", name+" cannot be set on a connection created for a database user, because the platform sets it.")
		}
	}
}

func (r *connectionResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var host, managedDatabaseID, user types.String
	req.Plan.GetAttribute(ctx, path.Root("host"), &host)
	req.Plan.GetAttribute(ctx, path.Root("managed_database_id"), &managedDatabaseID)
	req.Plan.GetAttribute(ctx, path.Root("user"), &user)
	if host.IsUnknown() || user.IsUnknown() || !isDatabaseUserConnection(host, managedDatabaseID, user) {
		return
	}
	if req.State.Raw.IsNull() {
		for _, name := range databaseUserServerSet {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), types.StringUnknown())...)
		}
		return
	}
	resp.Diagnostics.Append(copyFromState(ctx, req.State, &resp.Plan, databaseUserServerSet)...)
	settled := resp.Plan
	resp.Diagnostics.Append(copyFromState(ctx, req.State, &settled, []string{"password_set", "status", "status_error"})...)
	if settled.Raw.Equal(req.State.Raw) {
		resp.Plan = settled
	}
}

func copyFromState(ctx context.Context, state tfsdk.State, plan *tfsdk.Plan, names []string) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, name := range names {
		var value attr.Value
		diags.Append(state.GetAttribute(ctx, path.Root(name), &value)...)
		diags.Append(plan.SetAttribute(ctx, path.Root(name), value)...)
	}
	return diags
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
	out, err := r.client.API.CreateConnection(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), &serverv1.CreateConnectionRequest{Connection: connectionToProto(plan)})
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
		User:             plan.User.ValueStringPointer(),
		Database:         plan.Database.ValueStringPointer(),
		Listen:           plan.Listen.ValueStringPointer(),
		AgentPool:        plan.AgentPool.ValueStringPointer(),
		RedactEnabled:    plan.RedactEnabled.ValueBoolPointer(),
		AuditEnabled:     plan.AuditEnabled.ValueBoolPointer(),
		PolicyFailMode:   plan.PolicyFailMode.ValueStringPointer(),
		RedactRules:      redactRulesToProto(plan.RedactRules),
		ClearRedactRules: len(plan.RedactRules) == 0,
		ManagedBy:        plan.ManagedBy.ValueStringPointer(),
	}
	if !plan.forDatabaseUser() {
		update.Host = plan.Host.ValueStringPointer()
		update.CredentialRef = plan.CredentialRef.ValueStringPointer()
		update.CredentialSource = plan.CredentialSource.ValueStringPointer()
		update.Tls = &serverv1.TLSConfig{Cert: plan.TLSCert.ValueString(), Key: plan.TLSKey.ValueString(), Ca: plan.TLSCA.ValueString()}
		update.HostTls = &serverv1.HostTLSConfig{Enabled: true, Ca: plan.HostTLSCA.ValueString()}
		if !plan.Password.Equal(prior.Password) {
			update.Password = stringPtr(plan.Password.ValueString())
		}
	}
	disabling := false
	if !plan.DeletionProtection.Equal(prior.DeletionProtection) {
		update.DeletionProtection = boolPtr(plan.DeletionProtection.ValueBool())
		disabling = !plan.DeletionProtection.ValueBool()
	}
	if plan.ManagedDatabaseID.IsNull() {
		update.ClearManagedDatabaseId = !prior.ManagedDatabaseID.IsNull()
	} else {
		update.ManagedDatabaseId = plan.ManagedDatabaseID.ValueStringPointer()
	}

	out, err := r.client.API.UpdateConnection(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), update)
	if err != nil {
		addDeletionProtectionError(&resp.Diagnostics, "Cannot update connection", err, disabling)
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
	if refuseProtectedDelete(state.DeletionProtection, &resp.Diagnostics) {
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

func connectionToProto(plan connectionModel) *serverv1.Connection {
	c := &serverv1.Connection{
		Name:               plan.Name.ValueString(),
		Engine:             plan.Engine.ValueString(),
		User:               plan.User.ValueString(),
		Database:           plan.Database.ValueString(),
		Listen:             plan.Listen.ValueString(),
		AgentPool:          plan.AgentPool.ValueString(),
		ManagedDatabaseId:  plan.ManagedDatabaseID.ValueString(),
		ManagedBy:          plan.ManagedBy.ValueString(),
		DeletionProtection: plan.DeletionProtection.ValueBool(),
		Config: &serverv1.ConnectionConfig{
			RedactEnabled:  plan.RedactEnabled.ValueBool(),
			AuditEnabled:   plan.AuditEnabled.ValueBool(),
			PolicyFailMode: plan.PolicyFailMode.ValueString(),
			RedactRules:    redactRulesToProto(plan.RedactRules),
		},
	}
	if !plan.forDatabaseUser() {
		c.Host = plan.Host.ValueString()
		c.Password = plan.Password.ValueString()
		c.CredentialRef = plan.CredentialRef.ValueString()
		c.CredentialSource = plan.CredentialSource.ValueString()
		c.Tls = &serverv1.TLSConfig{Cert: plan.TLSCert.ValueString(), Key: plan.TLSKey.ValueString(), Ca: plan.TLSCA.ValueString()}
		c.HostTls = &serverv1.HostTLSConfig{Enabled: true, Ca: plan.HostTLSCA.ValueString()}
	}
	return c
}

func (m connectionModel) forDatabaseUser() bool {
	return isDatabaseUserConnection(m.Host, m.ManagedDatabaseID, m.User)
}

func isDatabaseUserConnection(host, managedDatabaseID, user types.String) bool {
	return host.ValueString() == "" && !managedDatabaseID.IsNull() && user.ValueString() != ""
}

func connectionFromProto(c *serverv1.Connection, prior connectionModel) connectionModel {
	cfg := c.GetConfig()
	return connectionModel{
		ID:                 types.StringValue(c.GetId()),
		WorkspaceID:        prior.WorkspaceID,
		Name:               types.StringValue(c.GetName()),
		Engine:             types.StringValue(c.GetEngine()),
		Host:               connectionHost(c, prior),
		User:               types.StringValue(c.GetUser()),
		Password:           prior.Password,
		CredentialRef:      types.StringValue(c.GetCredentialRef()),
		CredentialSource:   types.StringValue(c.GetCredentialSource()),
		Database:           types.StringValue(c.GetDatabase()),
		Listen:             types.StringValue(c.GetListen()),
		AgentPool:          types.StringValue(c.GetAgentPool()),
		ManagedDatabaseID:  nullIfEmpty(c.GetManagedDatabaseId()),
		RedactEnabled:      types.BoolValue(cfg.GetRedactEnabled()),
		AuditEnabled:       types.BoolValue(cfg.GetAuditEnabled()),
		PolicyFailMode:     types.StringValue(cfg.GetPolicyFailMode()),
		RedactRules:        redactRulesFromProto(cfg.GetRedactRules(), prior.RedactRules),
		TLSCert:            types.StringValue(c.GetTls().GetCert()),
		TLSKey:             types.StringValue(c.GetTls().GetKey()),
		TLSCA:              types.StringValue(c.GetTls().GetCa()),
		HostTLSCA:          types.StringValue(c.GetHostTls().GetCa()),
		Status:             types.StringValue(c.GetStatus()),
		StatusError:        types.StringValue(c.GetStatusError()),
		PasswordSet:        types.BoolValue(c.GetPasswordSet()),
		ManagedBy:          types.StringValue(c.GetManagedBy()),
		DatabaseUser:       types.StringValue(c.GetDatabaseUser()),
		DeletionProtection: types.BoolValue(c.GetDeletionProtection()),
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

func connectionHost(c *serverv1.Connection, prior connectionModel) types.String {
	if c.GetHost() == "" && c.GetManagedDatabaseId() != "" {
		return prior.Host
	}
	return types.StringValue(c.GetHost())
}

func nullIfEmpty(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
