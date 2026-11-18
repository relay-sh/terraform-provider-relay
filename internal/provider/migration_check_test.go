package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const (
	migrationWorkspace = "ws-mig"
	migrationAddr      = "data.relay_migration_check.this"
)

func checkResult(id string, severity serverv1.MigrationCheckSeverity, mode serverv1.MigrationMode, ack serverv1.MigrationAcknowledgement) *serverv1.MigrationSourceCheckResult {
	return &serverv1.MigrationSourceCheckResult{
		Id: id, Severity: severity, Mode: mode, Acknowledgement: ack,
		Message: "message of " + id, Fix: "fix for " + id,
	}
}

func migrationFake(t *testing.T) *fakeServer {
	f := startFakeServer(t)
	f.checkPendingFor = 2
	const (
		pass    = serverv1.MigrationCheckSeverity_MIGRATION_CHECK_SEVERITY_PASS
		warning = serverv1.MigrationCheckSeverity_MIGRATION_CHECK_SEVERITY_WARNING
		blocker = serverv1.MigrationCheckSeverity_MIGRATION_CHECK_SEVERITY_BLOCKER
		any     = serverv1.MigrationMode_MIGRATION_MODE_UNSPECIFIED
		online  = serverv1.MigrationMode_MIGRATION_MODE_ONLINE
		offline = serverv1.MigrationMode_MIGRATION_MODE_OFFLINE
		none    = serverv1.MigrationAcknowledgement_MIGRATION_ACKNOWLEDGEMENT_UNSPECIFIED
	)
	f.checkStatus = completedCheck(
		checkResult("server_version", pass, any, none),
		checkResult("huge_interval", blocker, online, none),
		checkResult("timetz_columns", warning, any, serverv1.MigrationAcknowledgement_MIGRATION_ACKNOWLEDGEMENT_TIMETZ),
		checkResult("timetz_defaults", warning, any, serverv1.MigrationAcknowledgement_MIGRATION_ACKNOWLEDGEMENT_TIMETZ),
		checkResult("large_objects", warning, offline, serverv1.MigrationAcknowledgement_MIGRATION_ACKNOWLEDGEMENT_LARGE_OBJECTS),
		checkResult("unlogged_tables", warning, online, serverv1.MigrationAcknowledgement_MIGRATION_ACKNOWLEDGEMENT_UNLOGGED_TABLES),
		checkResult("extensions", warning, any, none),
	)
	return f
}

func migrationConfig(f *fakeServer, placement, source, extra string) string {
	return providerBlock(f) + fmt.Sprintf(`
data "relay_migration_check" "this" {
  workspace_id = %q
%s
  source = {
%s
  }
%s
}
`, migrationWorkspace, placement, source, extra)
}

const (
	existingTarget = `  database_id = "db-1"`
	plannedTarget  = "  cloud  = \"aws\"\n  region = \"eu-west-1\""
	explicitSource = `    host     = "db.example.com"
    database = "orders"
    username = "relay"
    password = "s3cret"`
	uriSource = `    connection_string = "postgresql://relay:p%40ss@db.example.com:6543/orders?sslmode=require"`
)

func TestMigrationCheckDataSource(t *testing.T) {
	f := migrationFake(t)
	expires := fakeNow.Add(time.Hour).UTC().Format(time.RFC3339)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: migrationConfig(f, existingTarget, explicitSource+"\n    ca_pem   = \"CA\"", ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(migrationAddr, "id", "check-1"),
				resource.TestCheckResourceAttr(migrationAddr, "mode", "online"),
				resource.TestCheckResourceAttr(migrationAddr, "expires_at", expires),
				resource.TestCheckResourceAttr(migrationAddr, "modes.#", "2"),
				resource.TestCheckResourceAttr(migrationAddr, "modes.0", "online"),
				resource.TestCheckResourceAttr(migrationAddr, "modes.1", "offline"),
				resource.TestCheckResourceAttr(migrationAddr, "has_blockers", "true"),
				resource.TestCheckResourceAttr(migrationAddr, "acknowledgements.#", "2"),
				resource.TestCheckResourceAttr(migrationAddr, "acknowledgements.0", "timetz"),
				resource.TestCheckResourceAttr(migrationAddr, "acknowledgements.1", "unlogged_tables"),
				resource.TestCheckResourceAttr(migrationAddr, "results.#", "7"),
				resource.TestCheckResourceAttr(migrationAddr, "results.0.name", "server_version"),
				resource.TestCheckResourceAttr(migrationAddr, "results.0.level", "pass"),
				resource.TestCheckResourceAttr(migrationAddr, "results.0.mode", ""),
				resource.TestCheckResourceAttr(migrationAddr, "results.0.acknowledgement", ""),
				resource.TestCheckResourceAttr(migrationAddr, "results.1.name", "huge_interval"),
				resource.TestCheckResourceAttr(migrationAddr, "results.1.level", "blocker"),
				resource.TestCheckResourceAttr(migrationAddr, "results.1.mode", "online"),
				resource.TestCheckResourceAttr(migrationAddr, "results.1.message", "message of huge_interval"),
				resource.TestCheckResourceAttr(migrationAddr, "results.1.fix", "fix for huge_interval"),
				resource.TestCheckResourceAttr(migrationAddr, "results.2.level", "warning"),
				resource.TestCheckResourceAttr(migrationAddr, "results.2.acknowledgement", "timetz"),
				resource.TestCheckResourceAttr(migrationAddr, "results.4.mode", "offline"),
				resource.TestCheckResourceAttr(migrationAddr, "results.4.acknowledgement", "large_objects"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.server_version", "16.4"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.encoding", "UTF8"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.database_size_bytes", "53687091200"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.owner", "app"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.max_slot_wal_keep_size", "-1"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.wal_bytes_per_second", "1048576"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.largest_row_bytes", "4096"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.provider", "aws-rds"),
				func(*terraform.State) error {
					want := &serverv1.CheckMigrationSourceRequest{
						DatabaseId: "db-1",
						Source:     &serverv1.MigrationSourceConnection{Host: "db.example.com", Port: 5432, Database: "orders", Username: "relay", Password: "s3cret", CaPem: "CA"},
					}
					if !proto.Equal(f.lastCheck, want) {
						return fmt.Errorf("check request = %v, want %v", f.lastCheck, want)
					}
					if calls := f.callsTo("CheckMigrationSource"); len(calls) == 0 || calls[0].workspace != migrationWorkspace || calls[0].org != testOrgID {
						return fmt.Errorf("the check must run in workspace %q: %+v", migrationWorkspace, calls)
					}
					polls := f.callsTo("GetMigrationSourceCheck")
					if len(polls) < 3 {
						return fmt.Errorf("the check must be polled until it completes, got %d reads", len(polls))
					}
					if polls[0].workspace != migrationWorkspace {
						return fmt.Errorf("the check must be read in workspace %q: %+v", migrationWorkspace, polls[0])
					}
					return nil
				},
			),
		}},
	})
}

func TestMigrationCheckDataSourcePlannedTargetOffline(t *testing.T) {
	f := migrationFake(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: migrationConfig(f, plannedTarget, uriSource+"\n    acknowledge_unverified_tls = true", "  mode = \"offline\"\n  target_version = \"17\"\n  target_storage = \"200Gi\""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(migrationAddr, "mode", "offline"),
				resource.TestCheckResourceAttr(migrationAddr, "has_blockers", "false"),
				resource.TestCheckResourceAttr(migrationAddr, "acknowledgements.#", "2"),
				resource.TestCheckResourceAttr(migrationAddr, "acknowledgements.0", "timetz"),
				resource.TestCheckResourceAttr(migrationAddr, "acknowledgements.1", "large_objects"),
				func(*terraform.State) error {
					want := &serverv1.CheckMigrationSourceRequest{
						Cloud: "aws", Region: "eu-west-1", TargetVersion: "17", TargetStorage: "200Gi",
						Source: &serverv1.MigrationSourceConnection{
							Host: "db.example.com", Port: 6543, Database: "orders", Username: "relay", Password: "p@ss",
							SslMode: serverv1.MigrationSslMode_MIGRATION_SSL_MODE_REQUIRE, AcknowledgeUnverifiedTls: true,
						},
					}
					if !proto.Equal(f.lastCheck, want) {
						return fmt.Errorf("check request = %v, want %v", f.lastCheck, want)
					}
					return nil
				},
			),
		}},
	})
}

func TestMigrationCheckDataSourceWithoutResults(t *testing.T) {
	f := migrationFake(t)
	f.checkStatus = completedCheck()
	f.checkStatus.Facts = nil
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: migrationConfig(f, existingTarget, explicitSource+"\n    port = 5433\n    ssl_mode = \"verify-full\"", "  timeouts {\n    read = \"1m\"\n  }"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(migrationAddr, "results.#", "0"),
				resource.TestCheckResourceAttr(migrationAddr, "has_blockers", "false"),
				resource.TestCheckResourceAttr(migrationAddr, "acknowledgements.#", "0"),
				resource.TestCheckResourceAttr(migrationAddr, "facts.server_version", ""),
				resource.TestCheckResourceAttr(migrationAddr, "facts.database_size_bytes", "0"),
				func(*terraform.State) error {
					src := f.lastCheck.GetSource()
					if src.GetPort() != 5433 || src.GetSslMode() != serverv1.MigrationSslMode_MIGRATION_SSL_MODE_VERIFY_FULL {
						return fmt.Errorf("source = %v", src)
					}
					return nil
				},
			),
		}},
	})
}

func TestMigrationCheckDataSourceInvalidConfig(t *testing.T) {
	f := migrationFake(t)
	step := func(placement, source, extra, expect string) resource.TestStep {
		return resource.TestStep{
			Config:      migrationConfig(f, placement, source, extra),
			ExpectError: regexp.MustCompile(expect),
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			step("", explicitSource, "", `database_id,cloud`),
			step(existingTarget+"\n"+plannedTarget, explicitSource, "", `database_id,cloud`),
			step(`  cloud = "aws"`, explicitSource, "", `cloud,region`),
			step(existingTarget+"\n"+`  region = "eu-west-1"`, explicitSource, "", `cloud,region`),
			step(existingTarget, "", "", `source.host`),
			step(existingTarget, uriSource+"\n    host = \"db.example.com\"", "", `source.host`),
			step(existingTarget, uriSource+"\n    port = 5432", "", `source.port`),
			step(existingTarget, uriSource+"\n    password = \"s3cret\"", "", `source.password`),
			step(existingTarget, "    host = \"db.example.com\"", "", `source.database`),
			step(existingTarget, explicitSource+"\n    port = 70000", "", `70000`),
			step(existingTarget, explicitSource+"\n    ssl_mode = \"disable\"", "", `disable`),
			step(existingTarget, explicitSource, "  mode = \"bogus\"", `bogus`),
			step(existingTarget, explicitSource, "  timeouts {\n    read = \"soon\"\n  }", `soon`),
			step(existingTarget, `    connection_string = "not-a-uri"`, "", `Invalid connection string`),
			step(existingTarget, uriSource+"\n    ssl_mode = \"require\"", "", `not both`),
		},
	})
	for _, calls := range []string{"CheckMigrationSource", "GetMigrationSourceCheck"} {
		if n := len(f.callsTo(calls)); n != 0 {
			t.Errorf("%s was called %d times for an invalid configuration", calls, n)
		}
	}
}

func TestMigrationCheckDataSourceErrors(t *testing.T) {
	f := migrationFake(t)
	f.checkStatus.Phase = serverv1.MigrationPhase_MIGRATION_PHASE_FAILED
	f.checkStatus.Conditions = []*serverv1.MigrationCondition{
		{Type: "Connected", Message: "no pg_hba.conf entry for host"},
		{Type: "Ready"},
		{Type: "Verified", Message: "certificate has expired"},
	}
	failed := f.checkStatus
	resetCheck := func(method string, err error) func() {
		return func() {
			f.failWith("CheckMigrationSource", nil)
			f.failWith("GetMigrationSourceCheck", nil)
			f.failWith(method, err)
		}
	}
	step := func(pre func(), extra, expect string) resource.TestStep {
		return resource.TestStep{
			PreConfig:   pre,
			Config:      migrationConfig(f, existingTarget, explicitSource, extra),
			ExpectError: regexp.MustCompile(expect),
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			step(nil, "", `Migration source check failed`),
			step(func() { f.set(func() { f.checkStatus = failed }) }, "", `pg_hba.conf entry`),
			step(resetCheck("CheckMigrationSource", status.Error(codes.FailedPrecondition, "Database migration is not available yet")), "", `Database migration is not available`),
			step(resetCheck("CheckMigrationSource", status.Error(codes.PermissionDenied, "database_migration is off for this organization")), "", `is off for this`),
			step(resetCheck("GetMigrationSourceCheck", status.Error(codes.PermissionDenied, "database_migration is off for this organization")), "", `Database migration is not available`),
			step(resetCheck("CheckMigrationSource", status.Error(codes.Internal, "maestro is down")), "", `Cannot check migration source`),
			step(resetCheck("GetMigrationSourceCheck", status.Error(codes.Internal, "maestro lost the check")), "", `Migration source check did not finish`),
			step(func() {
				resetCheck("", nil)()
				f.set(func() { f.checkPendingFor = 1 << 20 })
			}, "  timeouts {\n    read = \"1ms\"\n  }", `timed out`),
		},
	})
}

func TestMigrationSourceConnection(t *testing.T) {
	const (
		verifyFull = serverv1.MigrationSslMode_MIGRATION_SSL_MODE_VERIFY_FULL
		require    = serverv1.MigrationSslMode_MIGRATION_SSL_MODE_REQUIRE
	)
	tests := []struct {
		name    string
		source  migrationSourceModel
		want    *serverv1.MigrationSourceConnection
		wantErr string
	}{
		{
			name:   "explicit fields default the port",
			source: migrationSourceModel{Host: types.StringValue("h"), Database: types.StringValue("d"), Username: types.StringValue("u")},
			want:   &serverv1.MigrationSourceConnection{Host: "h", Port: 5432, Database: "d", Username: "u"},
		},
		{
			name: "uri without a port, password or sslmode",
			source: migrationSourceModel{
				ConnectionString: types.StringValue("postgres://u@h/d"),
				SSLMode:          types.StringValue("require"),
			},
			want: &serverv1.MigrationSourceConnection{Host: "h", Port: 5432, Database: "d", Username: "u", SslMode: require},
		},
		{
			name:   "uri sslmode",
			source: migrationSourceModel{ConnectionString: types.StringValue("postgresql://u:p@h:5433/d?sslmode=verify-full")},
			want:   &serverv1.MigrationSourceConnection{Host: "h", Port: 5433, Database: "d", Username: "u", Password: "p", SslMode: verifyFull},
		},
		{name: "wrong scheme", source: migrationSourceModel{ConnectionString: types.StringValue("mysql://u:hunter2@h/d")}, wantErr: "postgres://"},
		{name: "unparsable", source: migrationSourceModel{ConnectionString: types.StringValue("postgres://u:hunter2@h:port/d")}, wantErr: "postgres://"},
		{name: "no host", source: migrationSourceModel{ConnectionString: types.StringValue("postgres://u:hunter2@/d")}, wantErr: "postgres://"},
		{name: "no database", source: migrationSourceModel{ConnectionString: types.StringValue("postgres://u:hunter2@h/")}, wantErr: "postgres://"},
		{name: "port too large", source: migrationSourceModel{ConnectionString: types.StringValue("postgres://u:hunter2@h:65536/d")}, wantErr: "port"},
		{name: "port zero", source: migrationSourceModel{ConnectionString: types.StringValue("postgres://u:hunter2@h:0/d")}, wantErr: "port"},
		{name: "unsupported sslmode", source: migrationSourceModel{ConnectionString: types.StringValue("postgres://u:hunter2@h/d?sslmode=disable")}, wantErr: "verify-full or require"},
		{
			name:    "sslmode in both places",
			source:  migrationSourceModel{ConnectionString: types.StringValue("postgres://u:hunter2@h/d?sslmode=require"), SSLMode: types.StringValue("require")},
			wantErr: "not both",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.source.Host = withNull(tt.source.Host)
			tt.source.Port = types.Int64Null()
			tt.source.ConnectionString = withNull(tt.source.ConnectionString)
			tt.source.SSLMode = withNull(tt.source.SSLMode)
			got, err := tt.source.connection()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || strings.Contains(err.Error(), "hunter2") {
					t.Fatalf("error = %v, want one with %q and without the password", err, tt.wantErr)
				}
				return
			}
			if err != nil || !proto.Equal(got, tt.want) {
				t.Fatalf("connection = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func withNull(v types.String) types.String {
	if v.ValueString() == "" {
		return types.StringNull()
	}
	return v
}

func TestMigrationCheckReadStopsOnInvalidTimeout(t *testing.T) {
	ctx := context.Background()
	d := newMigrationCheckDataSource().(*migrationCheckDataSource)
	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	objType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		values[name] = tftypes.NewValue(typ, nil)
	}
	sourceType := objType.AttributeTypes["source"].(tftypes.Object)
	sourceValues := map[string]tftypes.Value{}
	for name, typ := range sourceType.AttributeTypes {
		sourceValues[name] = tftypes.NewValue(typ, nil)
	}
	values["source"] = tftypes.NewValue(sourceType, sourceValues)
	timeoutsType := objType.AttributeTypes["timeouts"].(tftypes.Object)
	values["timeouts"] = tftypes.NewValue(timeoutsType, map[string]tftypes.Value{"read": tftypes.NewValue(tftypes.String, "soon")})
	var resp datasource.ReadResponse
	d.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objType, values)}}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Error("Read must stop on a timeout it cannot parse")
	}
}
