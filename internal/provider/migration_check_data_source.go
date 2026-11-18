package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const (
	migrationCheckTimeout = 10 * time.Minute
	defaultSourcePort     = 5432
)

var (
	migrationSSLModes = map[string]serverv1.MigrationSslMode{
		"verify-full": serverv1.MigrationSslMode_MIGRATION_SSL_MODE_VERIFY_FULL,
		"require":     serverv1.MigrationSslMode_MIGRATION_SSL_MODE_REQUIRE,
	}
	migrationModes = map[string]serverv1.MigrationMode{
		"online":  serverv1.MigrationMode_MIGRATION_MODE_ONLINE,
		"offline": serverv1.MigrationMode_MIGRATION_MODE_OFFLINE,
	}

	migrationResults = schema.ListNestedAttribute{
		Computed:            true,
		MarkdownDescription: "What the check found, one entry per rule.",
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"name":            schema.StringAttribute{Computed: true, MarkdownDescription: "Rule that produced the result."},
			"level":           schema.StringAttribute{Computed: true, MarkdownDescription: "`pass`, `warning` or `blocker`."},
			"mode":            schema.StringAttribute{Computed: true, MarkdownDescription: "`online` or `offline` when the result applies to that mode only, empty when it applies to every mode."},
			"message":         schema.StringAttribute{Computed: true, MarkdownDescription: "What was found."},
			"fix":             schema.StringAttribute{Computed: true, MarkdownDescription: "How to resolve it, empty when there is nothing to fix."},
			"acknowledgement": schema.StringAttribute{Computed: true, MarkdownDescription: "The acknowledgement that accepts this warning when a migration is created, for example `timetz`, empty when none is needed."},
		}},
	}
	migrationFacts = schema.SingleNestedAttribute{
		Computed:            true,
		MarkdownDescription: "What the check read from the source.",
		Attributes: map[string]schema.Attribute{
			"server_version":         schema.StringAttribute{Computed: true, MarkdownDescription: "PostgreSQL version of the source."},
			"encoding":               schema.StringAttribute{Computed: true, MarkdownDescription: "Database encoding."},
			"database_size_bytes":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Size of the source database, in bytes."},
			"owner":                  schema.StringAttribute{Computed: true, MarkdownDescription: "Owner of the source database."},
			"max_slot_wal_keep_size": schema.StringAttribute{Computed: true, MarkdownDescription: "The source's `max_slot_wal_keep_size` setting."},
			"wal_bytes_per_second":   schema.Int64Attribute{Computed: true, MarkdownDescription: "WAL the source writes per second, in bytes."},
			"largest_row_bytes":      schema.Int64Attribute{Computed: true, MarkdownDescription: "Size of the largest row found, in bytes."},
			"provider":               schema.StringAttribute{Computed: true, MarkdownDescription: "Managed provider the source runs on, empty when none was recognized."},
		},
	}
)

type migrationCheckDataSource struct {
	client *client.Client
}

type migrationSourceModel struct {
	ConnectionString         types.String `tfsdk:"connection_string"`
	Host                     types.String `tfsdk:"host"`
	Port                     types.Int64  `tfsdk:"port"`
	Database                 types.String `tfsdk:"database"`
	Username                 types.String `tfsdk:"username"`
	Password                 types.String `tfsdk:"password"`
	CAPEM                    types.String `tfsdk:"ca_pem"`
	SSLMode                  types.String `tfsdk:"ssl_mode"`
	AcknowledgeUnverifiedTLS types.Bool   `tfsdk:"acknowledge_unverified_tls"`
}

type migrationCheckModel struct {
	ID               types.String         `tfsdk:"id"`
	WorkspaceID      types.String         `tfsdk:"workspace_id"`
	DatabaseID       types.String         `tfsdk:"database_id"`
	Cloud            types.String         `tfsdk:"cloud"`
	Region           types.String         `tfsdk:"region"`
	TargetVersion    types.String         `tfsdk:"target_version"`
	TargetStorage    types.String         `tfsdk:"target_storage"`
	Source           migrationSourceModel `tfsdk:"source"`
	Mode             types.String         `tfsdk:"mode"`
	Timeouts         timeouts.Value       `tfsdk:"timeouts"`
	ExpiresAt        types.String         `tfsdk:"expires_at"`
	Results          types.List           `tfsdk:"results"`
	Modes            types.List           `tfsdk:"modes"`
	HasBlockers      types.Bool           `tfsdk:"has_blockers"`
	Acknowledgements types.List           `tfsdk:"acknowledgements"`
	Facts            types.Object         `tfsdk:"facts"`
}

type migrationResultModel struct {
	Name            string `tfsdk:"name"`
	Level           string `tfsdk:"level"`
	Mode            string `tfsdk:"mode"`
	Message         string `tfsdk:"message"`
	Fix             string `tfsdk:"fix"`
	Acknowledgement string `tfsdk:"acknowledgement"`
}

type migrationFactsModel struct {
	ServerVersion     string `tfsdk:"server_version"`
	Encoding          string `tfsdk:"encoding"`
	DatabaseSizeBytes int64  `tfsdk:"database_size_bytes"`
	Owner             string `tfsdk:"owner"`
	MaxSlotWALKeep    string `tfsdk:"max_slot_wal_keep_size"`
	WALBytesPerSecond int64  `tfsdk:"wal_bytes_per_second"`
	LargestRowBytes   int64  `tfsdk:"largest_row_bytes"`
	Provider          string `tfsdk:"provider"`
}

func newMigrationCheckDataSource() datasource.DataSource {
	return &migrationCheckDataSource{}
}

func (d *migrationCheckDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_migration_check"
}

func (d *migrationCheckDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	sibling := func(name string) path.Expression { return path.MatchRoot("source").AtName(name) }
	resp.Schema = schema.Schema{
		MarkdownDescription: "Checks whether a PostgreSQL database outside Relay can be migrated into Relay. " +
			"Each read runs a new check against the source, from the network path the migration will use, and waits for it to finish, up to the `read` timeout, 10 minutes by default. " +
			"The password and connection string are kept in the Terraform state like every data source argument. " +
			"There is no `relay_migration` resource: a migration is run from the Relay CLI or the dashboard, once the check has no blockers. " +
			"Reading it needs the `database_migration` feature enabled for the organization.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, MarkdownDescription: "Check ID."},
			"workspace_id": schema.StringAttribute{Required: true, MarkdownDescription: "Workspace the target belongs to."},
			"database_id":  schema.StringAttribute{Optional: true, MarkdownDescription: "Existing database the source would migrate into. Set it, or `cloud` and `region` for a target that does not exist yet."},
			"cloud":        schema.StringAttribute{Optional: true, MarkdownDescription: "Cloud of the planned target. Needs `region`, and cannot be set with `database_id`."},
			"region":       schema.StringAttribute{Optional: true, MarkdownDescription: "Region of the planned target. Needs `cloud`."},
			"target_version": schema.StringAttribute{
				Optional: true, MarkdownDescription: "PostgreSQL version of the planned target, for example `17`.",
			},
			"target_storage": schema.StringAttribute{
				Optional: true, MarkdownDescription: "Storage of the planned target, for example `100Gi`.",
			},
			"source": schema.SingleNestedAttribute{
				Required:            true,
				MarkdownDescription: "The database to check. Set `connection_string`, or `host`, `database` and `username`.",
				Attributes: map[string]schema.Attribute{
					"connection_string": schema.StringAttribute{
						Optional: true, Sensitive: true,
						Validators: []validator.String{
							stringvalidator.ExactlyOneOf(sibling("connection_string"), sibling("host")),
							stringvalidator.ConflictsWith(sibling("port"), sibling("database"), sibling("username"), sibling("password")),
						},
						MarkdownDescription: "A `postgres://` or `postgresql://` URI with the user, password, host, port and database. `sslmode` may be `verify-full` or `require`.",
					},
					"host": schema.StringAttribute{
						Optional:            true,
						Validators:          []validator.String{stringvalidator.AlsoRequires(sibling("database"), sibling("username"))},
						MarkdownDescription: "Host of the source.",
					},
					"port": schema.Int64Attribute{
						Optional:            true,
						Validators:          []validator.Int64{int64validator.Between(1, 65535)},
						MarkdownDescription: "Port of the source. Defaults to `5432`.",
					},
					"database": schema.StringAttribute{Optional: true, MarkdownDescription: "Database to check."},
					"username": schema.StringAttribute{Optional: true, MarkdownDescription: "User the check connects as."},
					"password": schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Password of `username`."},
					"ca_pem": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "PEM bundle that verifies the source's certificate. Without it, a host on AWS RDS or Aurora is verified with the provider's public CA bundle.",
					},
					"ssl_mode": schema.StringAttribute{
						Optional:            true,
						Validators:          []validator.String{stringvalidator.OneOf("verify-full", "require")},
						MarkdownDescription: "How the source's certificate is verified: `verify-full`, the default, or `require`, which needs `acknowledge_unverified_tls`. Cannot be set when `connection_string` has a `sslmode`.",
					},
					"acknowledge_unverified_tls": schema.BoolAttribute{Optional: true, MarkdownDescription: "Accept that `ssl_mode = \"require\"` does not verify the source's certificate."},
				},
			},
			"mode": schema.StringAttribute{
				Optional: true, Computed: true,
				Validators:          []validator.String{stringvalidator.OneOf("online", "offline")},
				MarkdownDescription: "How the migration would move the data: `online`, the default, or `offline`. It decides which results count for `has_blockers` and `acknowledgements`.",
			},
			"expires_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When the check stops being valid for creating a migration, RFC 3339."},
			"results":    migrationResults,
			"modes":      schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The modes the source can migrate in, `online` and `offline`."},
			"has_blockers": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether a `blocker` result applies to `mode`: one for every mode, or for that mode only. For example, a huge interval value blocks `online` only.",
			},
			"acknowledgements": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The acknowledgements that the warnings applying to `mode` need before a migration can be created.",
			},
			"facts": migrationFacts,
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx),
		},
	}
}

func (d *migrationCheckDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("database_id"), path.MatchRoot("cloud")),
		datasourcevalidator.RequiredTogether(path.MatchRoot("cloud"), path.MatchRoot("region")),
	}
}

func (d *migrationCheckDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *migrationCheckDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config migrationCheckModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := config.Timeouts.Read(ctx, migrationCheckTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	source, err := config.Source.connection()
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("source").AtName("connection_string"), "Invalid connection string", err.Error())
		return
	}
	workspaceID := config.WorkspaceID.ValueString()
	started, err := d.client.API.CheckMigrationSource(d.client.Workspace(ctx, workspaceID), &serverv1.CheckMigrationSourceRequest{
		DatabaseId:    config.DatabaseID.ValueString(),
		Source:        source,
		TargetVersion: config.TargetVersion.ValueString(),
		TargetStorage: config.TargetStorage.ValueString(),
		Cloud:         config.Cloud.ValueString(),
		Region:        config.Region.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.Append(migrationCheckError("Cannot check migration source", err))
		return
	}
	check := started.GetSourceCheck()
	err = waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		out, err := d.client.API.GetMigrationSourceCheck(d.client.Workspace(ctx, workspaceID), &serverv1.GetMigrationSourceCheckRequest{Id: check.GetId()})
		if err != nil {
			return false, err
		}
		check = out.GetSourceCheck()
		phase := check.GetStatus().GetPhase()
		return phase == serverv1.MigrationPhase_MIGRATION_PHASE_COMPLETED || phase == serverv1.MigrationPhase_MIGRATION_PHASE_FAILED, nil
	})
	if err != nil {
		resp.Diagnostics.Append(migrationCheckError("Migration source check did not finish", err))
		return
	}
	if check.GetStatus().GetPhase() == serverv1.MigrationPhase_MIGRATION_PHASE_FAILED {
		resp.Diagnostics.AddError("Migration source check failed", migrationCheckFailure(check))
		return
	}
	state, diags := migrationCheckState(ctx, config, check)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func migrationCheckError(summary string, err error) diag.Diagnostic {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.PermissionDenied:
		return diag.NewErrorDiagnostic("Database migration is not available", status.Convert(err).Message()+".")
	}
	return diag.NewErrorDiagnostic(summary, err.Error())
}

func migrationCheckFailure(check *serverv1.MigrationSourceCheck) string {
	parts := []string{"Check " + check.GetId() + " failed."}
	for _, c := range check.GetStatus().GetConditions() {
		if c.GetMessage() != "" {
			parts = append(parts, c.GetMessage())
		}
	}
	return strings.Join(parts, " ")
}

func (s migrationSourceModel) connection() (*serverv1.MigrationSourceConnection, error) {
	conn := &serverv1.MigrationSourceConnection{
		Host:                     s.Host.ValueString(),
		Port:                     defaultSourcePort,
		Database:                 s.Database.ValueString(),
		Username:                 s.Username.ValueString(),
		Password:                 s.Password.ValueString(),
		CaPem:                    s.CAPEM.ValueString(),
		AcknowledgeUnverifiedTls: s.AcknowledgeUnverifiedTLS.ValueBool(),
	}
	if !s.Port.IsNull() {
		conn.Port = int32(s.Port.ValueInt64())
	}
	sslMode := s.SSLMode.ValueString()
	if !s.ConnectionString.IsNull() {
		uriSSLMode, err := applyConnectionString(conn, s.ConnectionString.ValueString())
		if err != nil {
			return nil, err
		}
		if uriSSLMode != "" && sslMode != "" {
			return nil, errors.New("set sslmode in the connection string or ssl_mode, not both")
		}
		if sslMode == "" {
			sslMode = uriSSLMode
		}
	}
	conn.SslMode = migrationSSLModes[sslMode]
	return conn, nil
}

func applyConnectionString(conn *serverv1.MigrationSourceConnection, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || strings.Trim(u.Path, "/") == "" {
		return "", errors.New("must be a postgres:// or postgresql:// URI with a host and a database")
	}
	conn.Port = defaultSourcePort
	if p := u.Port(); p != "" {
		port, err := strconv.ParseUint(p, 10, 16)
		if err != nil || port == 0 {
			return "", errors.New("the port must be between 1 and 65535")
		}
		conn.Port = int32(port)
	}
	conn.Host, conn.Database = u.Hostname(), strings.Trim(u.Path, "/")
	conn.Username = u.User.Username()
	conn.Password, _ = u.User.Password()
	sslMode := u.Query().Get("sslmode")
	if _, ok := migrationSSLModes[sslMode]; sslMode != "" && !ok {
		return "", errors.New("sslmode must be verify-full or require")
	}
	return sslMode, nil
}

func migrationCheckState(ctx context.Context, config migrationCheckModel, check *serverv1.MigrationSourceCheck) (migrationCheckModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	mode := config.Mode.ValueString()
	if mode == "" {
		mode = "online"
	}
	chosen := migrationModes[mode]
	applies := func(m serverv1.MigrationMode) bool {
		return m == serverv1.MigrationMode_MIGRATION_MODE_UNSPECIFIED || m == chosen
	}
	st := check.GetStatus()
	results := make([]migrationResultModel, 0, len(st.GetResults()))
	acknowledgements := []string{}
	hasBlockers := false
	for _, r := range st.GetResults() {
		results = append(results, migrationResultModel{
			Name:            r.GetId(),
			Level:           migrationEnumName(r.GetSeverity(), "MIGRATION_CHECK_SEVERITY_"),
			Mode:            migrationEnumName(r.GetMode(), "MIGRATION_MODE_"),
			Message:         r.GetMessage(),
			Fix:             r.GetFix(),
			Acknowledgement: migrationEnumName(r.GetAcknowledgement(), "MIGRATION_ACKNOWLEDGEMENT_"),
		})
		if !applies(r.GetMode()) {
			continue
		}
		switch r.GetSeverity() {
		case serverv1.MigrationCheckSeverity_MIGRATION_CHECK_SEVERITY_BLOCKER:
			hasBlockers = true
		case serverv1.MigrationCheckSeverity_MIGRATION_CHECK_SEVERITY_WARNING:
			ack := migrationEnumName(r.GetAcknowledgement(), "MIGRATION_ACKNOWLEDGEMENT_")
			if ack != "" && !slices.Contains(acknowledgements, ack) {
				acknowledgements = append(acknowledgements, ack)
			}
		}
	}
	modes := make([]string, 0, len(st.GetModes()))
	for _, m := range st.GetModes() {
		modes = append(modes, migrationEnumName(m, "MIGRATION_MODE_"))
	}
	facts := st.GetFacts()
	state := config
	state.ID = types.StringValue(check.GetId())
	state.Mode = types.StringValue(mode)
	state.ExpiresAt = types.StringValue(st.GetExpiresAt().AsTime().UTC().Format(time.RFC3339))
	state.HasBlockers = types.BoolValue(hasBlockers)
	var d diag.Diagnostics
	state.Results, d = types.ListValueFrom(ctx, migrationResults.NestedObject.Type(), results)
	diags.Append(d...)
	state.Modes, d = types.ListValueFrom(ctx, types.StringType, modes)
	diags.Append(d...)
	state.Acknowledgements, d = types.ListValueFrom(ctx, types.StringType, acknowledgements)
	diags.Append(d...)
	state.Facts, d = types.ObjectValueFrom(ctx, migrationFacts.GetType().(types.ObjectType).AttrTypes, migrationFactsModel{
		ServerVersion:     facts.GetServerVersion(),
		Encoding:          facts.GetEncoding(),
		DatabaseSizeBytes: facts.GetDatabaseSizeBytes(),
		Owner:             facts.GetOwner(),
		MaxSlotWALKeep:    facts.GetMaxSlotWalKeepSize(),
		WALBytesPerSecond: facts.GetWalBytesPerSecond(),
		LargestRowBytes:   facts.GetLargestRowBytes(),
		Provider:          facts.GetProvider(),
	})
	diags.Append(d...)
	return state, diags
}

func migrationEnumName(v fmt.Stringer, prefix string) string {
	name := strings.TrimPrefix(v.String(), prefix)
	if name == "UNSPECIFIED" {
		return ""
	}
	return strings.ToLower(name)
}
