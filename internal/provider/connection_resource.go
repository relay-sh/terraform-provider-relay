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
	DeletionProtection types.Bool        `tfsdk:"deletion_protection"`
	DatabaseUser       types.String      `tfsdk:"database_user"`
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
		MarkdownDescription: "A database connection reached through a Relay agent. Upstream TLS is always on; only its CA can be configured.\n\nSet `host` to connect to an address you give. Or omit `host` and set `managed_database_id`, `user` and `database` to bind a managed per-user connection: the platform checks that the PostgreSQL user can reach the database and supplies the address, CA and credential itself, so `credential_ref`, `credential_source`, `password`, `host_tls_ca` and the client TLS attributes must stay unset. Redaction, audit, firewall and the other settings still apply, and `database_user` reports the bound user. Changing `managed_database_id`, `user`, `database`, `agent_pool` or `engine`, or switching between the two forms, replaces the connection.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Connection ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace the connection belongs to."},
			"name":         schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Name, unique in the workspace. Changing it replaces the connection."},
			"engine":       schema.StringAttribute{Required: true, MarkdownDescription: "Database engine: `postgres`, `mongodb`, `mysql`, `valkey` (Valkey and Redis), `sqlserver` or `oracle`."},
			"host": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				MarkdownDescription: "Upstream `host:port`. Required unless `managed_database_id` and `user` are both set, which creates a managed per-user connection that the platform addresses itself. The API does not return it for a connection with `managed_database_id`, so Terraform keeps the configured value, and an import leaves it empty until the next apply.",
			},
			"agent_pool": schema.StringAttribute{Required: true, MarkdownDescription: "The agent (pool) that serves this connection, for example `default`, the workspace's managed agent."},
			"user":       emptyString("Upstream user. With `managed_database_id` and no `host`, the PostgreSQL user of the managed database the connection logs in as; changing it replaces the connection."),
			"password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Upstream password. Write-only on the API: changes made outside Terraform are not detected. Prefer `credential_ref`.",
			},
			"credential_ref":      emptyString("Reference the agent resolves to a credential. Takes precedence over `password`."),
			"credential_source":   emptyString("Name of the external secret source that resolves `credential_ref`. Empty uses the agent's built-in source."),
			"database":            emptyString("Default database name. Required, and replaces the connection when changed, with `managed_database_id` and no `host`."),
			"listen":              emptyString("Local listen address on the agent."),
			"managed_database_id": schema.StringAttribute{Optional: true, MarkdownDescription: "Managed database this connection belongs to. Together with `user` and no `host`, it creates a managed per-user connection; changing it then replaces the connection."},
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
			"tls_cert":            emptyString("Client certificate, PEM."),
			"tls_key":             schema.StringAttribute{Optional: true, Computed: true, Sensitive: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Client key, PEM."},
			"tls_ca":              emptyString("CA bundle for the client certificate, PEM."),
			"host_tls_ca":         emptyString("CA bundle used to verify the upstream server, PEM."),
			"status":              computedString("`healthy`, `error` or `unknown`, as last reported by the agent."),
			"status_error":        computedString("Last error reported by the agent."),
			"password_set":        schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a password is stored."},
			"deletion_protection": deletionProtectionAttribute("connection"),
			"database_user":       computedString("PostgreSQL user the platform bound to a managed per-user connection. Empty for any other connection."),
			"managed_by": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(managedBy),
				MarkdownDescription: "Owner tag the dashboard shows. Defaults to `terraform`; set it to another tool or an empty string.",
			},
		},
	}
}

var perUserForbidden = []string{"credential_ref", "credential_source", "password", "host_tls_ca", "tls_cert", "tls_key", "tls_ca"}

func perUserMode(host, managedDatabaseID, user types.String) bool {
	return host.IsNull() && !managedDatabaseID.IsNull() && (user.IsUnknown() || user.ValueString() != "")
}

func (r *connectionResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	attr := func(name string) types.String {
		var v types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(name), &v)...)
		return v
	}
	host, managedDatabaseID, user := attr("host"), attr("managed_database_id"), attr("user")
	if host.IsUnknown() || !host.IsNull() {
		return
	}
	if !perUserMode(host, managedDatabaseID, user) {
		resp.Diagnostics.AddAttributeError(path.Root("host"), "Missing host",
			"host is required unless both managed_database_id and user are set. Omit host only for a managed per-user connection, where the platform provides the address.")
		return
	}
	for _, name := range perUserForbidden {
		if v := attr(name); !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
			resp.Diagnostics.AddAttributeError(path.Root(name), "Not allowed on a managed per-user connection",
				name+" cannot be set when host is omitted: the platform builds the address, CA and credential from managed_database_id and user.")
		}
	}
	if engine := attr("engine"); !engine.IsNull() && !engine.IsUnknown() && engine.ValueString() != "postgres" {
		resp.Diagnostics.AddAttributeError(path.Root("engine"), "Unsupported engine",
			"A managed per-user connection is PostgreSQL only: engine must be \"postgres\".")
	}
	if database := attr("database"); !database.IsUnknown() && database.ValueString() == "" {
		resp.Diagnostics.AddAttributeError(path.Root("database"), "Missing database",
			"database is required when host is omitted: it is the database the connection is pinned to.")
	}
}

func (r *connectionResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan connectionModel
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("host"), &plan.Host)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("managed_database_id"), &plan.ManagedDatabaseID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("user"), &plan.User)...)
	planned := perUserMode(plan.Host, plan.ManagedDatabaseID, plan.User)
	databaseUser := types.StringValue("")
	if planned {
		databaseUser = plan.User
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("database_user"), databaseUser)...)
	if req.State.Raw.IsNull() {
		return
	}
	var prior types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("database_user"), &prior)...)
	if planned != (prior.ValueString() != "") {
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("host"))
		return
	}
	if !planned {
		return
	}
	for _, name := range []string{"managed_database_id", "user", "database", "agent_pool", "engine"} {
		var before, after types.String
		resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root(name), &before)...)
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root(name), &after)...)
		if !before.Equal(after) {
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root(name))
		}
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
	conn := &serverv1.Connection{
		Name:               plan.Name.ValueString(),
		Engine:             plan.Engine.ValueString(),
		Host:               plan.Host.ValueString(),
		User:               plan.User.ValueString(),
		Password:           plan.Password.ValueString(),
		CredentialRef:      plan.CredentialRef.ValueString(),
		CredentialSource:   plan.CredentialSource.ValueString(),
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
		Tls:     &serverv1.TLSConfig{Cert: plan.TLSCert.ValueString(), Key: plan.TLSKey.ValueString(), Ca: plan.TLSCA.ValueString()},
		HostTls: &serverv1.HostTLSConfig{Enabled: true, Ca: plan.HostTLSCA.ValueString()},
	}
	if perUserMode(plan.Host, plan.ManagedDatabaseID, plan.User) {
		conn.Tls, conn.HostTls = nil, nil
	}
	out, err := r.client.API.CreateConnection(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), &serverv1.CreateConnectionRequest{Connection: conn})
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
	perUser := perUserMode(plan.Host, plan.ManagedDatabaseID, plan.User)
	update := &serverv1.UpdateConnectionRequest{
		Name:             plan.Name.ValueString(),
		Listen:           plan.Listen.ValueStringPointer(),
		RedactEnabled:    plan.RedactEnabled.ValueBoolPointer(),
		AuditEnabled:     plan.AuditEnabled.ValueBoolPointer(),
		PolicyFailMode:   plan.PolicyFailMode.ValueStringPointer(),
		RedactRules:      redactRulesToProto(plan.RedactRules),
		ClearRedactRules: len(plan.RedactRules) == 0,
		ManagedBy:        plan.ManagedBy.ValueStringPointer(),
	}
	if !perUser {
		update.Engine = plan.Engine.ValueStringPointer()
		update.Host = plan.Host.ValueStringPointer()
		update.User = plan.User.ValueStringPointer()
		update.CredentialRef = plan.CredentialRef.ValueStringPointer()
		update.CredentialSource = plan.CredentialSource.ValueStringPointer()
		update.Database = plan.Database.ValueStringPointer()
		update.AgentPool = plan.AgentPool.ValueStringPointer()
		update.Tls = &serverv1.TLSConfig{Cert: plan.TLSCert.ValueString(), Key: plan.TLSKey.ValueString(), Ca: plan.TLSCA.ValueString()}
		update.HostTls = &serverv1.HostTLSConfig{Enabled: true, Ca: plan.HostTLSCA.ValueString()}
		if !plan.Password.Equal(prior.Password) {
			update.Password = stringPtr(plan.Password.ValueString())
		}
		if plan.ManagedDatabaseID.IsNull() {
			update.ClearManagedDatabaseId = !prior.ManagedDatabaseID.IsNull()
		} else {
			update.ManagedDatabaseId = plan.ManagedDatabaseID.ValueStringPointer()
		}
	}
	disabling := false
	if !plan.DeletionProtection.Equal(prior.DeletionProtection) {
		update.DeletionProtection = boolPtr(plan.DeletionProtection.ValueBool())
		disabling = !plan.DeletionProtection.ValueBool()
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

func connectionFromProto(c *serverv1.Connection, prior connectionModel) connectionModel {
	cfg := c.GetConfig()
	state := connectionModel{
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
		DeletionProtection: types.BoolValue(c.GetDeletionProtection()),
		DatabaseUser:       types.StringValue(c.GetDatabaseUser()),
	}
	if c.GetDatabaseUser() != "" {
		state.Password = types.StringNull()
		state.CredentialRef = types.StringValue("")
		state.CredentialSource = types.StringValue("")
		state.TLSCert = types.StringValue("")
		state.TLSKey = types.StringValue("")
		state.TLSCA = types.StringValue("")
		state.HostTLSCA = types.StringValue("")
	}
	return state
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
	if c.GetDatabaseUser() != "" {
		return types.StringNull()
	}
	if c.GetHost() == "" && c.GetManagedDatabaseId() != "" && !prior.Host.IsNull() && !prior.Host.IsUnknown() {
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
