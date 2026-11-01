package provider

import (
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const (
	valkeyAddr   = "relay_valkey_cluster.this"
	postgresAddr = "relay_postgresql_cluster.this"
	pastInstant  = "2026-10-31T12:00:00Z"
)

func valkeySource(id, version, mode, label string, backups bool) *serverv1.Database {
	count, shards := int32(1), int32(0)
	if mode == valkeyModeCluster {
		count, shards = 3, 3
	}
	return &serverv1.Database{
		Id: id, Name: "source-" + id, WorkspaceId: dbWorkspace,
		Spec: &serverv1.DatabaseSpec{
			Engine: engineValkey, Version: version, BackupsEnabled: backups, PrimaryNodeGroup: label,
			Valkey: &serverv1.ValkeyConfig{Mode: mode, Shards: shards, Parameters: map[string]string{"maxmemory-policy": "noeviction"}},
			Nodes: []*serverv1.NodeSpec{{
				Name: label, Role: roleElectable, Count: count, Cloud: "aws", Region: "us-east-1", Tier: "RC10",
				Resources: &serverv1.ResourceRequirements{Storage: "8Gi"},
			}},
		},
		Status: &serverv1.DatabaseStatus{Phase: phaseRunning},
	}
}

func postgresSource(id, version string) *serverv1.Database {
	return &serverv1.Database{
		Id: id, Name: "source-" + id, WorkspaceId: dbWorkspace,
		Spec: &serverv1.DatabaseSpec{
			Engine: enginePostgreSQL, Version: version, BackupsEnabled: true, PrimaryNodeGroup: "writers",
			Replication: &serverv1.Replication{Mode: "async"},
			Postgresql:  &serverv1.PostgreSQLConfig{Parameters: map[string]string{"work_mem": "4MB"}},
			Nodes: []*serverv1.NodeSpec{{
				Name: "writers", Role: roleElectable, Count: 1, Cloud: "aws", Region: "us-east-1", Tier: "R10",
				Resources: &serverv1.ResourceRequirements{Cpu: "2", Memory: "4Gi", Storage: "10Gi", DiskType: "gp3", StorageClass: "standard"},
			}},
		},
		Status: &serverv1.DatabaseStatus{Phase: phaseRunning},
	}
}

func seedSources(f *fakeServer, dbs ...*serverv1.Database) {
	f.set(func() {
		for _, db := range dbs {
			f.databases[db.Id] = db
		}
	})
}

func restoreBlock(source, target string) string {
	if target == "" {
		return fmt.Sprintf("  restore = { source_database_id = %q }\n", source)
	}
	return fmt.Sprintf("  restore = {\n    source_database_id = %q\n    target_time        = %q\n  }\n", source, target)
}

func postgresRestoreConfig(f *fakeServer, restore, extra string, count int) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing-restored"
%s%s
  nodes = {
    writers = {
      role   = "electable"
      count  = %d
      cloud  = "aws"
      region = "us-east-1"
      tier   = "R10"
      resources = { storage = "10Gi" }
    }
  }
}
`, dbWorkspace, restore, extra, count)
}

func TestValkeyClusterResource_Restore(t *testing.T) {
	f := startFakeServer(t)
	seedSources(f, valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: valkeyConfig(f, restoreBlock("db-src", pastInstant)+valkeyGroup("cache", 2, "8Gi", "")),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(valkeyAddr, "restore.source_database_id", "db-src"),
					resource.TestCheckResourceAttr(valkeyAddr, "restore.target_time", pastInstant),
					resource.TestCheckResourceAttr(valkeyAddr, "nodes.cache.count", "2"),
					resource.TestCheckResourceAttr(valkeyAddr, "version", "9.1"),
					resource.TestCheckResourceAttr(valkeyAddr, "parameters.maxmemory-policy", "noeviction"),
					callCount(f, "CreateDatabase", 0),
					func(*terraform.State) error {
						want, _ := time.Parse(time.RFC3339, pastInstant)
						switch r := f.lastRestore; {
						case r.GetName() != "sessions" || r.GetSourceDatabaseId() != "db-src" || !r.GetTargetTime().AsTime().Equal(want):
							return fmt.Errorf("restore request = %v", r)
						case !slices.Equal(f.lastDBUpdate.GetUpdateMask().GetPaths(), []string{"nodes"}):
							return fmt.Errorf("only the count drifted from the source, got mask %v", f.lastDBUpdate.GetUpdateMask().GetPaths())
						case f.lastDBUpdate.GetSpec().GetValkey().GetParameters()["maxmemory-policy"] != "noeviction" || f.lastDBUpdate.GetSpec().GetVersion() != "9.1":
							return fmt.Errorf("unconfigured settings must keep the source's values, got %v", f.lastDBUpdate.GetSpec())
						}
						return nil
					},
				),
			},
			{
				Config: valkeyConfig(f, restoreBlock("db-src", "2026-10-30T12:00:00Z")+valkeyGroup("cache", 2, "8Gi", "")),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(valkeyAddr, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: callCount(f, "RestoreDatabase", 2),
			},
			{
				Config: valkeyConfig(f, valkeyGroup("cache", 2, "8Gi", "")),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(valkeyAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(valkeyAddr, "restore.source_database_id"),
					callCount(f, "RestoreDatabase", 2),
					callCount(f, "UpdateDatabase", 2),
				),
			},
		},
	})
}

func TestValkeyClusterResource_RestoreConverges(t *testing.T) {
	cases := map[string]struct {
		source *serverv1.Database
		body   string
		mask   []string
	}{
		"matching config": {
			source: valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true),
			body:   restoreBlock("db-src", "") + valkeyGroup("cache", 1, "4Gi", ""),
		},
		"larger storage": {
			source: valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true),
			body:   restoreBlock("db-src", "") + valkeyGroup("cache", 1, "16Gi", ""),
			mask:   []string{"nodes"},
		},
		"reshard and parameters": {
			source: valkeySource("db-src", "9.1", valkeyModeCluster, "cache", true),
			body: restoreBlock("db-src", "") + "  mode = \"cluster\"\n  shards = 4\n  parameters = { \"maxmemory-policy\" = \"allkeys-lru\" }\n" +
				valkeyGroup("cache", 4, "", ""),
			mask: []string{"nodes", "valkey"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			seedSources(f, tc.source)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config: valkeyConfig(f, tc.body),
					Check: func(*terraform.State) error {
						var mask []string
						if f.lastDBUpdate != nil {
							mask = f.lastDBUpdate.GetUpdateMask().GetPaths()
						}
						if !slices.Equal(mask, tc.mask) {
							return fmt.Errorf("mask %v, want %v", mask, tc.mask)
						}
						return nil
					},
				}},
			})
		})
	}
}

func TestValkeyClusterResource_RestorePlanChecks(t *testing.T) {
	f := startFakeServer(t)
	seedSources(f,
		valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true),
		valkeySource("db-cl", "9.1", valkeyModeCluster, "cache", true),
		valkeySource("db-nobackup", "9.1", valkeyModeReplication, "cache", false),
		valkeySource("db-nomode", "9.1", "", "cache", true),
		postgresSource("db-pg", "18"),
	)
	group := valkeyGroup("cache", 1, "", "")
	cases := map[string]struct {
		body string
		want string
	}{
		"invalid target":       {restoreBlock("db-src", "yesterday") + group, "Invalid target_time"},
		"future target":        {restoreBlock("db-src", "2999-01-01T00:00:00Z") + group, "target_time is in the future"},
		"cluster mode pitr":    {restoreBlock("db-cl", pastInstant) + "  mode = \"cluster\"\n  shards = 3\n" + valkeyGroup("cache", 3, "", ""), "No point-in-time restore in Valkey cluster mode"},
		"other engine":         {restoreBlock("db-pg", "") + group, "Source is another engine"},
		"no backups":           {restoreBlock("db-nobackup", "") + group, "Source has no backups"},
		"other mode":           {restoreBlock("db-src", "") + "  mode = \"cluster\"\n  shards = 3\n" + valkeyGroup("cache", 3, "", ""), "Mode differs from the source"},
		"other version":        {restoreBlock("db-src", "") + "  version = \"9.0\"\n" + group, "Version differs from the source"},
		"other label":          {restoreBlock("db-src", "") + valkeyGroup("other", 1, "", ""), "Missing the source's node group"},
		"unknown restore":      {"  restore = terraform_data.later.output == \"x\" ? null : { source_database_id = \"db-src\" }\n" + group, ""},
		"unknown source id":    {"  restore = { source_database_id = terraform_data.later.output }\n" + group, ""},
		"unreadable source":    {restoreBlock("db-gone", "") + group, ""},
		"workspace unknown":    {restoreBlock("db-src", "") + group, ""},
		"unknown nodes":        {restoreBlock("db-src", "") + "  nodes = terraform_data.later.output == \"x\" ? {} : {\n    cache = {\n      cloud     = \"aws\"\n      region    = \"us-east-1\"\n      tier      = \"RC10\"\n      resources = {}\n    }\n  }\n", ""},
		"source without mode":  {restoreBlock("db-nomode", "") + "  mode = \"cluster\"\n  shards = 3\n" + valkeyGroup("cache", 3, "", ""), `(?s)Mode differs from the source.*replication mode`},
		"cluster mode no pitr": {restoreBlock("db-cl", "") + "  mode = \"cluster\"\n  shards = 3\n" + valkeyGroup("cache", 3, "", ""), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			config := "resource \"terraform_data\" \"later\" {\n  input = \"y\"\n}\n" + valkeyConfig(f, tc.body)
			if name == "workspace unknown" {
				config = regexp.MustCompile(`workspace_id = "ws-db"`).ReplaceAllString(config, "workspace_id = terraform_data.later.output")
			}
			step := resource.TestStep{Config: config, PlanOnly: true, ExpectNonEmptyPlan: true}
			if tc.want != "" {
				step.ExpectError = regexp.MustCompile(tc.want)
			}
			resource.UnitTest(t, resource.TestCase{ProtoV6ProviderFactories: factories(relay.Dial), Steps: []resource.TestStep{step}})
		})
	}
}

func TestValkeyClusterResource_RestoreFailures(t *testing.T) {
	cases := map[string]struct {
		setup func(*fakeServer)
		body  string
		want  string
	}{
		"source not found": {
			body: restoreBlock("db-missing", "") + valkeyGroup("cache", 1, "", ""),
			want: "Cannot restore cluster",
		},
		"kept source on another version": {
			setup: func(f *fakeServer) {
				f.set(func() {
					f.keptSources["db-gone"] = valkeySource("db-gone", "9.0", valkeyModeReplication, "cache", true)
				})
			},
			body: restoreBlock("db-gone", "") + "  version = \"9.1\"\n" + valkeyGroup("other", 1, "", ""),
			want: "Restored cluster does not match the configuration",
		},
		"update rejected": {
			setup: func(f *fakeServer) { f.failWith("UpdateDatabase", status.Error(codes.InvalidArgument, "nope")) },
			body:  restoreBlock("db-src", "") + valkeyGroup("cache", 2, "", ""),
			want:  "Cannot apply the configuration to the restored cluster",
		},
		"update fails to settle": {
			setup: func(f *fakeServer) {
				f.set(func() { f.afterUpdate = func(*serverv1.Database) { f.failProvision = "boom" } })
			},
			body: restoreBlock("db-src", "") + valkeyGroup("cache", 2, "", ""),
			want: "Restored cluster did not settle after update",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			seedSources(f, valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true))
			if tc.setup != nil {
				tc.setup(f)
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: valkeyConfig(f, tc.body), ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestPostgreSQLClusterResource_Restore(t *testing.T) {
	f := startFakeServer(t)
	seedSources(f, postgresSource("db-src", "18"))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: postgresRestoreConfig(f, restoreBlock("db-src", ""), "", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(postgresAddr, "version", "18"),
					resource.TestCheckResourceAttr(postgresAddr, "parameters.work_mem", "4MB"),
					resource.TestCheckNoResourceAttr(postgresAddr, "restore.target_time"),
					callCount(f, "UpdateDatabase", 0),
					func(*terraform.State) error {
						if f.lastRestore.GetTargetTime() != nil {
							return fmt.Errorf("an omitted target_time must not be sent, got %v", f.lastRestore.GetTargetTime())
						}
						return nil
					},
				),
			},
			{
				Config: postgresRestoreConfig(f, restoreBlock("db-src", pastInstant), `
  parameters  = { work_mem = "8MB" }
  replication = {
    mode                 = "synchronous"
    synchronous_replicas = 1
  }
`, 2),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(postgresAddr, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(postgresAddr, "parameters.work_mem", "8MB"),
					resource.TestCheckResourceAttr(postgresAddr, "replication.mode", "synchronous"),
					resource.TestCheckResourceAttr(postgresAddr, "nodes.writers.count", "2"),
					func(*terraform.State) error {
						if paths := f.lastDBUpdate.GetUpdateMask().GetPaths(); !slices.Equal(paths, []string{"nodes", "replication", "postgresql"}) {
							return fmt.Errorf("mask %v", paths)
						}
						if v := f.lastDBUpdate.GetSpec().GetVersion(); v != "18" {
							return fmt.Errorf("the update must keep the source's version, got %q", v)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestPostgreSQLClusterResource_RestoreChecksAndFailures(t *testing.T) {
	cases := map[string]struct {
		setup func(*fakeServer)
		body  string
		want  string
	}{
		"other label": {
			setup: func(f *fakeServer) { f.set(func() { f.databases["db-src"].Spec.PrimaryNodeGroup = "primary" }) },
			body:  restoreBlock("db-src", ""),
			want:  "Missing the source's node group",
		},
		"invalid target": {
			body: restoreBlock("db-src", "nope"),
			want: "Invalid target_time",
		},
		"kept source on another version": {
			setup: func(f *fakeServer) { f.set(func() { f.keptSources["db-gone"] = postgresSource("db-gone", "17") }) },
			body:  restoreBlock("db-gone", "") + "  version = \"18\"\n",
			want:  "Restored cluster does not match the configuration",
		},
		"update rejected": {
			setup: func(f *fakeServer) { f.failWith("UpdateDatabase", status.Error(codes.InvalidArgument, "nope")) },
			body:  restoreBlock("db-src", "") + "  parameters = {}\n",
			want:  "Cannot apply the configuration to the restored cluster",
		},
		"update fails to settle": {
			setup: func(f *fakeServer) {
				f.set(func() { f.afterUpdate = func(*serverv1.Database) { f.failProvision = "boom" } })
			},
			body: restoreBlock("db-src", "") + "  parameters = {}\n",
			want: "Restored cluster did not settle after update",
		},
		"source not found": {
			body: restoreBlock("db-missing", ""),
			want: "Cannot restore cluster",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			seedSources(f, postgresSource("db-src", "18"))
			if tc.setup != nil {
				tc.setup(f)
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: postgresRestoreConfig(f, tc.body, "", 1), ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestValkeyClusterResource_RestoreFromAnotherWorkspace(t *testing.T) {
	f := startFakeServer(t)
	other := valkeySource("db-other", "9.1", valkeyModeReplication, "cache", true)
	other.WorkspaceId = "ws-elsewhere"
	seedSources(f, other)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:      valkeyConfig(f, restoreBlock("db-other", "")+valkeyGroup("cache", 1, "", "")),
			ExpectError: regexp.MustCompile(`(?s)source database is in a different\s+workspace`),
		}},
	})
}

func TestSatisfies(t *testing.T) {
	str := func(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
	strs := func(m map[string]string) tftypes.Value {
		out := map[string]tftypes.Value{}
		for k, v := range m {
			out[k] = str(v)
		}
		return tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, out)
	}
	num := tftypes.NewValue(tftypes.Number, big.NewFloat(1))
	cases := map[string]struct {
		planned, got tftypes.Value
		want         bool
	}{
		"unknown":        {tftypes.NewValue(tftypes.String, tftypes.UnknownValue), str("a"), true},
		"null vs value":  {tftypes.NewValue(tftypes.String, nil), str("a"), false},
		"equal":          {num, num, true},
		"different":      {str("a"), str("b"), false},
		"fewer keys":     {strs(map[string]string{"a": "1"}), strs(map[string]string{"a": "1", "b": "2"}), false},
		"other key":      {strs(map[string]string{"a": "1"}), strs(map[string]string{"b": "1"}), false},
		"nested differs": {strs(map[string]string{"a": "1"}), strs(map[string]string{"a": "2"}), false},
		"map vs null":    {strs(map[string]string{"a": "1"}), tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil), false},
		"nested equal":   {strs(map[string]string{"a": "1"}), strs(map[string]string{"a": "1"}), true},
	}
	for name, tc := range cases {
		if got := satisfies(tc.planned, tc.got); got != tc.want {
			t.Errorf("%s: satisfies = %v, want %v", name, got, tc.want)
		}
	}
}
