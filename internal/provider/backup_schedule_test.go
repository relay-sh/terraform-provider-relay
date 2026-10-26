package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const scheduleAddr = "relay_backup_schedule.this"

func seedCluster(f *fakeServer, id, engine string, backups bool) {
	f.set(func() {
		f.databases[id] = &serverv1.Database{
			Id: id, Name: "cluster-" + id, WorkspaceId: dbWorkspace,
			Spec:   &serverv1.DatabaseSpec{Engine: engine, BackupsEnabled: backups},
			Status: &serverv1.DatabaseStatus{Phase: phaseRunning},
		}
	})
}

func scheduleConfig(f *fakeServer, clusterID, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_backup_schedule" "this" {
  workspace_id = %q
  cluster_id   = %q
%s
}
`, dbWorkspace, clusterID, body)
}

const pgTiers = `
  snapshot_time = { hour = 3, minute = 30 }
  tiers = [
    { frequency = "daily", interval = 1, retention_value = 7 },
    { frequency = "weekly", interval = 2, retention_value = 5, retention_unit = "weeks" },
    { frequency = "hourly", interval = 5, retention_value = 0, enabled = false },
  ]
  pitr_window_days = 14
`

func noCustomSchedules(f *fakeServer) tfresource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.schedules) != 0 {
			return fmt.Errorf("destroy must reset every schedule, %d left", len(f.schedules))
		}
		return nil
	}
}

func TestBackupScheduleResource_PostgreSQLLifecycle(t *testing.T) {
	f := startFakeServer(t)
	seedCluster(f, "db-1", enginePostgreSQL, true)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noCustomSchedules(f),
		Steps: []tfresource.TestStep{
			{
				Config: scheduleConfig(f, "db-1", pgTiers),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(scheduleAddr, "id", "db-1"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "platform_default", "false"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "snapshot_time.hour", "3"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "snapshot_time.minute", "30"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "tiers.#", "3"),
					tfresource.TestCheckNoResourceAttr(scheduleAddr, "tiers.0.retention_unit"),
					tfresource.TestCheckNoResourceAttr(scheduleAddr, "tiers.0.enabled"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "tiers.1.retention_unit", "weeks"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "tiers.2.enabled", "false"),
					tfresource.TestCheckNoResourceAttr(scheduleAddr, "max_backups"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "pitr_window_days", "14"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "last_backup_time", "2023-11-14T22:13:20Z"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "next_backup_time", "2023-11-15T22:13:20Z"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "active_backups", "3"),
					func(*terraform.State) error {
						s := f.lastScheduleSpec
						switch {
						case s.GetPolicyTiers()[0].GetRetentionUnit() != "days" || s.GetPolicyTiers()[2].GetRetentionUnit() != "days":
							return fmt.Errorf("an omitted unit must be sent as the tier's own, got %+v", s.GetPolicyTiers())
						case s.GetRetention() != nil:
							return errors.New("max_backups was not configured and must not be sent")
						case s.GetPitrWindowDays() != 14 || s.GetSnapshotTime().GetHour() != 3:
							return fmt.Errorf("spec = %+v", s)
						}
						if calls := f.callsTo("UpsertBackupSchedule"); calls[0].workspace != dbWorkspace {
							return fmt.Errorf("upsert must be workspace-scoped: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config: scheduleConfig(f, "db-1", `  max_backups = 10`),
				ConfigPlanChecks: tfresource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(scheduleAddr, plancheck.ResourceActionUpdate)},
				},
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(scheduleAddr, "max_backups", "10"),
					tfresource.TestCheckNoResourceAttr(scheduleAddr, "tiers.#"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "snapshot_time.hour", "2"),
					tfresource.TestCheckNoResourceAttr(scheduleAddr, "pitr_window_days"),
					func(*terraform.State) error {
						s := f.lastScheduleSpec
						if len(s.GetPolicyTiers()) != 0 || s.GetSnapshotTime().GetHour() != 2 || s.GetPitrWindowDays() != 0 || s.GetRetention().GetMaxBackups() != 10 {
							return fmt.Errorf("a full replacement must send only what is configured, got %+v", s)
						}
						return nil
					},
				),
			},
			{
				ResourceName:      scheduleAddr,
				ImportState:       true,
				ImportStateId:     dbWorkspace + "/db-1",
				ImportStateVerify: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if a := states[0].Attributes; a["tiers.#"] != "2" || a["tiers.1.retention_unit"] != "weeks" || a["tiers.1.enabled"] != "true" || a["pitr_window_days"] != "7" {
						return fmt.Errorf("an import must read every attribute, got %v", a)
					}
					return nil
				},
				ImportStateVerifyIgnore: []string{"tiers", "pitr_window_days"},
			},
		},
	})
	if n := len(f.callsTo("ResetBackupSchedule")); n != 1 {
		t.Errorf("destroy must reset the schedule once, got %d", n)
	}
}

func TestBackupScheduleResource_Valkey(t *testing.T) {
	f := startFakeServer(t)
	seedCluster(f, "db-1", engineValkey, true)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noCustomSchedules(f),
		Steps: []tfresource.TestStep{
			{
				Config: scheduleConfig(f, "db-1", `
  tiers = [
    { frequency = "weekly", interval = 7, retention_value = 2 },
    { frequency = "monthly", interval = 40, retention_value = 3 },
  ]
`),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckNoResourceAttr(scheduleAddr, "pitr_window_days"),
					tfresource.TestCheckNoResourceAttr(scheduleAddr, "max_backups"),
					tfresource.TestCheckResourceAttr(scheduleAddr, "tiers.1.interval", "40"),
					func(*terraform.State) error {
						if tiers := f.lastScheduleSpec.GetPolicyTiers(); tiers[1].GetRetentionUnit() != "months" || !tiers[1].GetEnabled() {
							return fmt.Errorf("tiers = %+v", tiers)
						}
						return nil
					},
				),
			},
			{
				Config:      scheduleConfig(f, "db-1", "  pitr_window_days = 3"),
				ExpectError: regexp.MustCompile("No point-in-time restore on Valkey"),
			},
		},
	})
}

func TestBackupScheduleResource_ValkeyPITRAtApply(t *testing.T) {
	f := startFakeServer(t)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config: valkeyConfig(f, valkeyGroup("cache", 1, "1Gi", "")) + fmt.Sprintf(`
resource "relay_backup_schedule" "this" {
  workspace_id     = %q
  cluster_id       = relay_valkey_cluster.this.id
  pitr_window_days = 3
}
`, dbWorkspace),
			ExpectError: regexp.MustCompile("No point-in-time restore on Valkey"),
		}},
	})
}

func TestBackupScheduleResource_ConfigGuards(t *testing.T) {
	f := startFakeServer(t)
	tier := func(fields string) string { return "  tiers = [{ " + fields + " }]" }
	cases := map[string]struct{ body, want string }{
		"hour":              {"  snapshot_time = { hour = 24, minute = 0 }", "value must be between 0 and 23"},
		"minute":            {"  snapshot_time = { hour = 1, minute = 60 }", "value must be between 0 and 59"},
		"yearly":            {tier(`frequency = "yearly", interval = 1, retention_value = 1`), "value must be one of"},
		"years":             {tier(`frequency = "monthly", interval = 1, retention_value = 1, retention_unit = "years"`), "value must be one of"},
		"unit mismatch":     {tier(`frequency = "weekly", interval = 1, retention_value = 1, retention_unit = "days"`), "Invalid retention unit"},
		"weekday":           {tier(`frequency = "weekly", interval = 8, retention_value = 1`), `(?s)Invalid interval.*Sunday`},
		"hourly divisor":    {tier(`frequency = "hourly", interval = 5, retention_value = 1`), "divisor of 24"},
		"month day":         {tier(`frequency = "monthly", interval = 32, retention_value = 1`), "day of the month"},
		"retention":         {tier(`frequency = "daily", interval = 1, retention_value = 0`), "Invalid retention"},
		"duplicate":         {"  tiers = [{ frequency = \"daily\", interval = 1, retention_value = 1 }, { frequency = \"daily\", interval = 1, retention_value = 2 }]", "Duplicate tier"},
		"empty tiers":       {"  tiers = []", "list must contain at least 1"},
		"max with tiers":    {"  max_backups = 3\n" + tier(`frequency = "daily", interval = 1, retention_value = 1`), "cannot be specified when"},
		"max backups":       {"  max_backups = 0", "value must be at least 1"},
		"pitr zero":         {"  pitr_window_days = 0", "value must be at least 1"},
		"missing frequency": {tier(`interval = 1, retention_value = 1`), "Incorrect attribute value type|attribute \"frequency\" is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tfresource.UnitTest(t, tfresource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []tfresource.TestStep{{
					Config:      scheduleConfig(f, "db-1", tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}

func TestBackupScheduleResource_PlanOnlyInputs(t *testing.T) {
	f := startFakeServer(t)
	seedCluster(f, "db-1", enginePostgreSQL, true)
	cases := map[string]string{
		"unknown pitr":              `  pitr_window_days = terraform_data.later.output.interval`,
		"disabled tier skips rules": `  tiers = [{ frequency = "weekly", interval = 9, retention_value = 0, enabled = false }]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			tfresource.UnitTest(t, tfresource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []tfresource.TestStep{{
					Config: `resource "terraform_data" "later" {
  input = { frequency = "daily", interval = 1, retention_value = 1, retention_unit = "days", enabled = true }
}
` + scheduleConfig(f, "db-1", body),
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				}},
			})
		})
	}
}

func TestBackupScheduleResource_PlanChecksSkipUnreadableCluster(t *testing.T) {
	f := startFakeServer(t)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config:      scheduleConfig(f, "db-missing", "  pitr_window_days = 3"),
			ExpectError: regexp.MustCompile(`(?s)Cannot set backup schedule.*not found`),
		}},
	})
	if n := len(f.callsTo("GetDatabase")); n == 0 {
		t.Error("the plan must look up the engine when pitr_window_days is set")
	}
}

func TestBackupScheduleResource_PlanLimit(t *testing.T) {
	f := startFakeServer(t)
	seedCluster(f, "db-1", enginePostgreSQL, true)
	f.planRetentionDays = 30
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config:      scheduleConfig(f, "db-1", `  tiers = [{ frequency = "monthly", interval = 1, retention_value = 12 }]`),
			ExpectError: regexp.MustCompile(`(?s)Backup retention exceeds the plan.*at most 30 days.*upgrade the\s+organization's\s+plan`),
		}},
	})
}

func TestBackupScheduleResource_BackupsDisabled(t *testing.T) {
	f := startFakeServer(t)
	seedCluster(f, "db-1", enginePostgreSQL, false)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config:      scheduleConfig(f, "db-1", "  max_backups = 2"),
			ExpectError: regexp.MustCompile(`(?s)Cannot set backup schedule.*enable backups`),
		}},
	})
}

func TestBackupScheduleResource_Drift(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	f := startFakeServer(t)
	seedCluster(f, "db-1", enginePostgreSQL, true)
	cfg := scheduleConfig(f, "db-1", "  max_backups = 2")
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{Config: cfg},
			{
				PreConfig:          func() { f.set(func() { delete(f.schedules, "db-1") }) },
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: cfg},
			{
				PreConfig:          func() { f.set(func() { f.databases["db-1"].Spec.BackupsEnabled = false }) },
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { f.set(func() { f.databases["db-1"].Spec.BackupsEnabled = true }) },
				Config:    cfg,
			},
			{
				PreConfig:   func() { f.failWith("GetBackupSchedule", unavailable) },
				Config:      cfg,
				ExpectError: regexp.MustCompile("Cannot read backup schedule"),
			},
			{
				PreConfig: func() {
					f.failWith("GetBackupSchedule", nil)
					f.failWith("UpsertBackupSchedule", unavailable)
				},
				Config:      scheduleConfig(f, "db-1", "  max_backups = 3"),
				ExpectError: regexp.MustCompile("Cannot set backup schedule"),
			},
			{
				PreConfig: func() {
					f.failWith("UpsertBackupSchedule", nil)
					f.failWith("ResetBackupSchedule", unavailable)
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot reset backup schedule"),
			},
			{
				PreConfig: func() { f.failWith("ResetBackupSchedule", status.Error(codes.NotFound, "gone")) },
				Config:    providerBlock(f),
			},
			{
				Config:        cfg,
				ResourceName:  scheduleAddr,
				ImportState:   true,
				ImportStateId: "db-only",
				ExpectError:   regexp.MustCompile("Invalid import ID"),
			},
		},
	})
}

func TestBackupScheduleModifyPlanStopsOnMalformedInput(t *testing.T) {
	ctx := context.Background()
	r := newBackupScheduleResource().(*backupScheduleResource)
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	var resp resource.ModifyPlanResponse
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan:   tfsdk.Plan{Schema: schemaResp.Schema, Raw: malformed},
		Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: malformed},
	}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Error("ModifyPlan must stop on malformed input")
	}
}

func TestBackupScheduleFromProtoFillsImpliedUnit(t *testing.T) {
	got := backupScheduleFromProto(&serverv1.BackupSchedule{Spec: &serverv1.BackupScheduleSpec{
		PolicyTiers: []*serverv1.PolicyTier{{Frequency: "weekly", Interval: 1, RetentionValue: 2}},
	}}, backupScheduleModel{ClusterID: types.StringValue("db-1")})
	unit := got.Tiers.Elements()[0].(types.Object).Attributes()["retention_unit"].(types.String)
	if unit.ValueString() != "weeks" || got.ID.ValueString() != "db-1" || got.LastBackupTime.ValueString() != "" {
		t.Errorf("got %+v", got)
	}
}

func TestBackupScheduleValidateConfigSkipsUnknownTiers(t *testing.T) {
	ctx := context.Background()
	r := newBackupScheduleResource().(*backupScheduleResource)
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	objType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	listType := objType.AttributeTypes["tiers"].(tftypes.List)
	values := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		values[name] = tftypes.NewValue(typ, nil)
	}
	values["tiers"] = tftypes.NewValue(listType, []tftypes.Value{tftypes.NewValue(listType.ElementType, tftypes.UnknownValue)})
	var resp resource.ValidateConfigResponse
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objType, values)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("an unknown tier cannot be judged yet: %v", resp.Diagnostics)
	}
}
