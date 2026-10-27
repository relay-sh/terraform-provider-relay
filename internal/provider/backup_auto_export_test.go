package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const (
	exportAddr        = "relay_backup_auto_export.this"
	exportIntegration = "0d6f4a52-3c1e-4b8e-9f7a-2a1b3c4d5e6f"
	exportCluster1    = "11111111-1111-4111-8111-111111111111"
	exportCluster2    = "22222222-2222-4222-8222-222222222222"
)

func exportFake(t *testing.T) *fakeServer {
	f := startFakeServer(t)
	f.cloudIntegrations = []*serverv1.CloudIntegration{{Id: exportIntegration, Provider: "aws", Name: "backups"}}
	return f
}

func exportConfig(f *fakeServer, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
data "relay_cloud_integration" "this" {
  workspace_id = %[1]q
  name         = "backups"
}

resource "relay_backup_auto_export" "this" {
  workspace_id   = %[1]q
  integration_id = data.relay_cloud_integration.this.id
  bucket         = "relay-backups"
%[2]s
}
`, ciWorkspace, body)
}

func exportDeleted(f *fakeServer) tfresource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.autoExport != nil {
			return fmt.Errorf("destroy must delete the export, got %+v", f.autoExport)
		}
		return nil
	}
}

func TestBackupAutoExportResource_Lifecycle(t *testing.T) {
	f := exportFake(t)
	selected := fmt.Sprintf(`
  prefix   = "relay/"
  which    = "scheduled"
  backfill = true
  scope    = { mode = "selected", ids = [%q, %q] }
`, exportCluster2, exportCluster1)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             exportDeleted(f),
		Steps: []tfresource.TestStep{
			{
				Config: exportConfig(f, selected),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr(exportAddr, "id", ciWorkspace),
					tfresource.TestCheckResourceAttr(exportAddr, "integration_id", exportIntegration),
					tfresource.TestCheckResourceAttr(exportAddr, "prefix", "relay/"),
					tfresource.TestCheckResourceAttr(exportAddr, "which", "scheduled"),
					tfresource.TestCheckResourceAttr(exportAddr, "scope.mode", "selected"),
					tfresource.TestCheckResourceAttr(exportAddr, "scope.ids.#", "2"),
					tfresource.TestCheckResourceAttr(exportAddr, "backfill", "true"),
					tfresource.TestCheckResourceAttr(exportAddr, "last_error", ""),
					tfresource.TestCheckResourceAttr(exportAddr, "last_exported_at", ""),
					func(*terraform.State) error {
						req := f.lastAutoExport
						if !req.GetBackfill() || !req.GetAutoExport().GetEnabled() {
							return fmt.Errorf("create must enable the export and request the backfill, got %+v", req)
						}
						if calls := f.callsTo("UpsertBackupAutoExport"); calls[0].workspace != ciWorkspace {
							return fmt.Errorf("upsert must be workspace-scoped: %+v", calls)
						}
						return nil
					},
				),
			},
			{
				Config:             exportConfig(f, selected),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
			{
				Config: exportConfig(f, `  backfill = false`),
				ConfigPlanChecks: tfresource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(exportAddr, plancheck.ResourceActionUpdate)},
				},
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckNoResourceAttr(exportAddr, "prefix"),
					tfresource.TestCheckResourceAttr(exportAddr, "which", "all"),
					tfresource.TestCheckResourceAttr(exportAddr, "scope.mode", "all"),
					tfresource.TestCheckNoResourceAttr(exportAddr, "scope.ids"),
					tfresource.TestCheckResourceAttr(exportAddr, "backfill", "false"),
					tfresource.TestCheckResourceAttr(exportAddr, "last_error", "AccessDenied"),
					tfresource.TestCheckResourceAttr(exportAddr, "last_exported_at", "2023-11-14T22:13:20Z"),
					func(*terraform.State) error {
						if req := f.lastAutoExport; req.GetBackfill() || req.GetAutoExport().GetScope().GetMode() != "all" || req.GetAutoExport().GetPrefix() != "" {
							return fmt.Errorf("an update must not backfill and must send the whole config, got %+v", req)
						}
						return nil
					},
				),
			},
			{
				Config:             exportConfig(f, `  backfill = true`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:             exportConfig(f, ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
			{
				Config:                  exportConfig(f, ""),
				ResourceName:            exportAddr,
				ImportState:             true,
				ImportStateId:           ciWorkspace,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"backfill"},
			},
		},
	})
	if n := len(f.callsTo("UpsertBackupAutoExport")); n != 2 {
		t.Errorf("create and update upsert once each, got %d", n)
	}
	if calls := f.callsTo("DeleteBackupAutoExport"); len(calls) != 1 || calls[0].workspace != ciWorkspace {
		t.Errorf("destroy must delete the export once, workspace-scoped, got %+v", calls)
	}
}

func TestBackupAutoExportResource_ImportSelected(t *testing.T) {
	f := exportFake(t)
	f.autoExport = &serverv1.BackupAutoExport{
		IntegrationId: exportIntegration, Bucket: "relay-backups", Prefix: "nightly", Which: "manual", Enabled: true,
		Scope: &serverv1.ClusterScope{Mode: "selected", Ids: []string{exportCluster1}},
	}
	cfg := exportConfig(f, fmt.Sprintf(`
  prefix = "nightly"
  which  = "manual"
  scope  = { mode = "selected", ids = [%q] }
`, exportCluster1))
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{
				Config:             cfg,
				ResourceName:       exportAddr,
				ImportState:        true,
				ImportStateId:      ciWorkspace,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if a := states[0].Attributes; a["prefix"] != "nightly" || a["scope.ids.0"] != exportCluster1 || a["backfill"] != "false" {
						return fmt.Errorf("import must read the whole export, got %v", a)
					}
					return nil
				},
			},
			{
				Config:   cfg,
				PlanOnly: true,
			},
		},
	})
}

func TestBackupAutoExportResource_Drift(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	f := exportFake(t)
	cfg := exportConfig(f, "")
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{
			{Config: cfg},
			{
				PreConfig:          func() { f.set(func() { f.autoExport.Enabled = false }) },
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: cfg},
			{
				PreConfig:          func() { f.set(func() { f.autoExport = nil }) },
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: cfg},
			{
				PreConfig:          func() { f.failWith("GetBackupAutoExport", status.Error(codes.NotFound, "gone")) },
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig:   func() { f.failWith("GetBackupAutoExport", unavailable) },
				Config:      cfg,
				ExpectError: regexp.MustCompile("Cannot read backup auto-export"),
			},
			{
				PreConfig: func() {
					f.failWith("GetBackupAutoExport", nil)
					f.failWith("UpsertBackupAutoExport", unavailable)
				},
				Config:      exportConfig(f, `  which = "manual"`),
				ExpectError: regexp.MustCompile("Cannot set backup auto-export"),
			},
			{
				PreConfig: func() {
					f.failWith("UpsertBackupAutoExport", nil)
					f.failWith("DeleteBackupAutoExport", unavailable)
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot delete backup auto-export"),
			},
			{
				PreConfig: func() { f.failWith("DeleteBackupAutoExport", status.Error(codes.NotFound, "gone")) },
				Config:    providerBlock(f),
			},
		},
	})
}

func TestBackupAutoExportResource_ServerRejects(t *testing.T) {
	f := exportFake(t)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
resource "relay_backup_auto_export" "this" {
  workspace_id   = %q
  integration_id = "99999999-9999-4999-8999-999999999999"
  bucket         = "relay-backups"
}
`, ciWorkspace),
			ExpectError: regexp.MustCompile(`(?s)Cannot set backup auto-export.*cloud integration not found`),
		}},
	})
}

func TestBackupAutoExportResource_ConfigGuards(t *testing.T) {
	f := exportFake(t)
	cases := map[string]struct{ body, want string }{
		"which":         {`  which = "daily"`, "value must be one of"},
		"mode":          {`  scope = { mode = "some" }`, "value must be one of"},
		"selected":      {`  scope = { mode = "selected" }`, "Missing cluster IDs"},
		"selected none": {`  scope = { mode = "selected", ids = [] }`, "Missing cluster IDs"},
		"all with ids":  {fmt.Sprintf(`  scope = { mode = "all", ids = [%q] }`, exportCluster1), "Cluster IDs not allowed"},
		"cluster uuid":  {`  scope = { mode = "selected", ids = ["DB-1"] }`, "must be a lowercase UUID"},
		"prefix":        {`  prefix = " relay"`, "must not start or end with whitespace"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tfresource.UnitTest(t, tfresource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []tfresource.TestStep{{
					Config:      exportConfig(f, tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
resource "relay_backup_auto_export" "this" {
  workspace_id   = %q
  integration_id = "not-a-uuid"
  bucket         = "Relay_Backups"
}
`, ciWorkspace),
			ExpectError: regexp.MustCompile(`(?s)must be a lowercase UUID.*must be a valid S3 bucket name|(?s)must be a valid S3 bucket name.*must be a lowercase UUID`),
		}},
	})
}

func TestBackupAutoExportResource_UnknownScopeIDs(t *testing.T) {
	f := exportFake(t)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []tfresource.TestStep{{
			Config: `resource "terraform_data" "later" {
  input = "` + exportCluster1 + `"
}
` + exportConfig(f, `  scope = { mode = "selected", ids = [terraform_data.later.output] }`),
			Check: tfresource.TestCheckResourceAttr(exportAddr, "scope.ids.0", exportCluster1),
		}},
	})
}

func TestBackupAutoExportValidateConfigSkipsUnknownScope(t *testing.T) {
	ctx := context.Background()
	r := newBackupAutoExportResource().(*backupAutoExportResource)
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	objType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		values[name] = tftypes.NewValue(typ, nil)
	}
	scopeType := objType.AttributeTypes["scope"].(tftypes.Object)
	values["scope"] = tftypes.NewValue(scopeType, map[string]tftypes.Value{
		"mode": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"ids":  tftypes.NewValue(scopeType.AttributeTypes["ids"], nil),
	})
	var resp resource.ValidateConfigResponse
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objType, values)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("an unknown mode cannot be judged yet: %v", resp.Diagnostics)
	}
}

func TestBackupAutoExportResource_DestroyWithIntegration(t *testing.T) {
	f := startFakeServer(t)
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy: func(*terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.autoExport != nil || len(f.cloudIntegrations) != 0 {
				return fmt.Errorf("one destroy must delete the export and then the integration, got %+v and %d integrations", f.autoExport, len(f.cloudIntegrations))
			}
			return nil
		},
		Steps: []tfresource.TestStep{{
			Config: integrationConfig(f, "backups", roleA) + fmt.Sprintf(`
resource "relay_backup_auto_export" "this" {
  workspace_id   = %q
  integration_id = relay_cloud_integration.this.id
  bucket         = "relay-backups"
}
`, ciWorkspace),
			Check: tfresource.TestCheckResourceAttrPair(exportAddr, "integration_id", integrationAddr, "id"),
		}},
	})
}
