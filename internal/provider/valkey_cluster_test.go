package provider

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

func valkeyConfig(f *fakeServer, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_valkey_cluster" "this" {
  workspace_id = %q
  name         = "sessions"
%s
}
`, dbWorkspace, body)
}

func valkeyGroup(label string, count int, storage, extra string) string {
	return fmt.Sprintf(`
  nodes = {
    %s = {
      count  = %d
      cloud  = "aws"
      region = "us-east-1"
      tier   = "RC10"
      resources = {
        storage = %q%s
      }
    }
  }
`, label, count, storage, extra)
}

func replicationConfig(f *fakeServer, version, storage, parameters, extra string) string {
	return valkeyConfig(f, fmt.Sprintf(`
  version    = %q
  parameters = %s
  keep_backups_on_destroy = true
%s%s`, version, parameters, valkeyGroup("cache", 2, storage, ""), extra))
}

func TestValkeyClusterResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noClustersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: replicationConfig(f, "9.1", "8Gi", `{ "maxmemory-policy" = "allkeys-lru" }`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "phase", "running"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "mode", "replication"),
					resource.TestCheckNoResourceAttr("relay_valkey_cluster.this", "shards"),
					resource.TestCheckNoResourceAttr("relay_valkey_cluster.this", "replicas_per_shard"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "nodes.cache.count", "2"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "nodes.cache.tier", "RC10"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "parameters.maxmemory-policy", "allkeys-lru"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "endpoint_host", "sessions.public"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "endpoint_port", "6379"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "ready_nodes", "1"),
					func(*terraform.State) error {
						calls := f.callsTo("CreateDatabase")
						if len(calls) != 1 || calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						spec := f.lastDBCreate.GetSpec()
						n := spec.GetNodes()[0]
						switch {
						case spec.GetEngine() != "valkey":
							return fmt.Errorf("engine = %q", spec.GetEngine())
						case !spec.GetBackupsEnabled():
							return errors.New("backups must always be requested on")
						case spec.GetReplication() != nil || spec.GetPostgresql() != nil:
							return errors.New("PostgreSQL settings must not be sent for Valkey")
						case spec.GetValkey().GetMode() != "replication":
							return fmt.Errorf("mode must always be sent, got %q", spec.GetValkey().GetMode())
						case spec.GetPrimaryNodeGroup() != "cache" || n.GetName() != "cache" || n.GetRole() != "electable":
							return fmt.Errorf("the only group must be sent as the electable primary, got %q/%q/%q", spec.GetPrimaryNodeGroup(), n.GetName(), n.GetRole())
						case n.GetResources().GetCpu() != "" || n.GetResources().GetDiskType() != "":
							return fmt.Errorf("cpu and disk_type must not be sent, got %+v", n.GetResources())
						}
						return nil
					},
				),
			},
			{
				Config: replicationConfig(f, "9.1", "16Gi", `{}`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "nodes.cache.resources.storage", "16Gi"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "parameters.%", "0"),
					func(*terraform.State) error {
						paths := f.lastDBUpdate.GetUpdateMask().GetPaths()
						if !slices.Equal(paths, []string{"nodes", "valkey.parameters"}) {
							return fmt.Errorf("a parameters-only change must not send the topology, got mask %v", paths)
						}
						if v := f.lastDBUpdate.GetSpec().GetValkey(); v.GetMode() != "replication" || v.GetParameters() != nil {
							return fmt.Errorf("an emptied parameters map must clear them and keep the mode, got %+v", v)
						}
						return nil
					},
				),
			},
			{
				Config: replicationConfig(f, "9.1", "16Gi", `{}`, `
  timeouts {
    update = "5m"
  }
`),
				Check: func(*terraform.State) error {
					if n := len(f.callsTo("UpdateDatabase")); n != 1 {
						return fmt.Errorf("a timeouts-only change must not call UpdateDatabase, got %d calls", n)
					}
					return nil
				},
			},
			{
				Config:      replicationConfig(f, "9.0", "16Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Version cannot change here"),
			},
			{
				Config:      replicationConfig(f, "9.1", "4Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Storage cannot shrink"),
			},
			{
				ResourceName: "relay_valkey_cluster.this",
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return dbWorkspace + "/" + s.RootModule().Resources["relay_valkey_cluster.this"].Primary.ID, nil
				},
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"keep_backups_on_destroy", "timeouts"},
			},
		},
	})
}

func TestValkeyClusterResource_MinimalCluster(t *testing.T) {
	f := startFakeServer(t)
	config := valkeyConfig(f, valkeyGroup("cache", 1, "1Gi", ""))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "mode", "replication"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "parameters.%", "0"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "keep_backups_on_destroy", "false"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

func clusterModeConfig(f *fakeServer, shards, replicas string, count int) string {
	return valkeyConfig(f, fmt.Sprintf(`
  mode   = "cluster"
  shards = %s
%s%s`, shards, replicas, valkeyGroup("shards", count, "4Gi", "")))
}

func TestValkeyClusterResource_ClusterMode(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: clusterModeConfig(f, "3", "  replicas_per_shard = 1", 6),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "mode", "cluster"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "shards", "3"),
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "replicas_per_shard", "1"),
					func(*terraform.State) error {
						if v := f.lastDBCreate.GetSpec().GetValkey(); v.GetMode() != "cluster" || v.GetShards() != 3 || v.GetReplicasPerShard() != 1 {
							return fmt.Errorf("valkey config = %+v", v)
						}
						return nil
					},
				),
			},
			{
				Config: clusterModeConfig(f, "4", "  replicas_per_shard = 1", 8),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_cluster.this", plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if paths := f.lastDBUpdate.GetUpdateMask().GetPaths(); !slices.Equal(paths, []string{"nodes", "valkey"}) {
						return fmt.Errorf("a reshard must send nodes and valkey together, got %v", paths)
					}
					return nil
				},
			},
			{
				Config: clusterModeConfig(f, "4", "  replicas_per_shard = 0", 4),
				Check:  resource.TestCheckResourceAttr("relay_valkey_cluster.this", "replicas_per_shard", "0"),
			},
			{
				Config: clusterModeConfig(f, "4", "", 4),
				Check:  resource.TestCheckNoResourceAttr("relay_valkey_cluster.this", "replicas_per_shard"),
			},
			{Config: clusterModeConfig(f, "4", "", 4), PlanOnly: true},
		},
	})
}

func TestValkeyClusterResource_Replacements(t *testing.T) {
	f := startFakeServer(t)
	replaced := func(*terraform.State) error {
		if n := len(f.callsTo("CreateDatabase")); n != 2 {
			return fmt.Errorf("a replacement must create a new cluster, got %d creates", n)
		}
		return nil
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: valkeyConfig(f, valkeyGroup("cache", 3, "1Gi", ""))},
			{
				Config: valkeyConfig(f, valkeyGroup("store", 3, "1Gi", "")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_cluster.this", plancheck.ResourceActionDestroyBeforeCreate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "nodes.store.count", "3"),
					replaced,
				),
			},
			{
				Config: clusterModeConfig(f, "3", "", 3),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("relay_valkey_cluster.this", plancheck.ResourceActionDestroyBeforeCreate)},
				},
			},
		},
	})
}

func TestValkeyClusterResource_StorageAutoscale(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: valkeyConfig(f, valkeyGroup("cache", 1, "4Gi", ""))},
			{
				Config: valkeyConfig(f, valkeyGroup("cache", 1, "4Gi", `
        storage_autoscale = {
          enabled     = true
          max_storage = "40Gi"
        }`)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_valkey_cluster.this", "nodes.cache.resources.storage_autoscale.enabled", "true"),
					func(*terraform.State) error {
						if sa := f.lastDBUpdate.GetSpec().GetNodes()[0].GetResources().GetStorageAutoscale(); !sa.GetEnabled() || sa.GetMaxStorage() != "40Gi" {
							return fmt.Errorf("storage_autoscale must reach the platform enabled, got %+v", sa)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for _, db := range f.databases {
						db.Status.Storage = []*serverv1.NodeGroupStorage{{NodeGroup: "cache", Size: "6Gi"}}
					}
				},
				Config: valkeyConfig(f, valkeyGroup("cache", 1, "4Gi", `
        storage_autoscale = {
          enabled     = true
          max_storage = "40Gi"
        }`)),
				Check: resource.TestCheckResourceAttr("relay_valkey_cluster.this", "storage_status.0.size", "6Gi"),
			},
		},
	})
}

func TestValkeyClusterResource_ConfigValidation(t *testing.T) {
	f := startFakeServer(t)
	two := `
  nodes = {
    a = {
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "RC10"
      resources = { storage = "1Gi" }
    }
    b = {
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "RC10"
      resources = { storage = "1Gi" }
    }
  }
`
	cases := map[string]struct {
		body string
		want string
	}{
		"shards in replication mode":   {"  shards = 3\n" + valkeyGroup("cache", 1, "1Gi", ""), `(?s)Only in cluster mode.*shards`},
		"replicas in replication mode": {"  replicas_per_shard = 1\n" + valkeyGroup("cache", 1, "1Gi", ""), `(?s)Only in cluster mode.*replicas_per_shard`},
		"cluster without shards":       {"  mode = \"cluster\"\n" + valkeyGroup("cache", 3, "1Gi", ""), "Missing shards"},
		"count off the shards":         {"  mode = \"cluster\"\n  shards = 3\n  replicas_per_shard = 1\n" + valkeyGroup("cache", 3, "1Gi", ""), `(?s)Count does not match the shards.*= 6 nodes.*asks for 3`},
		"default count off the shards": {`
  mode   = "cluster"
  shards = 3
  nodes = {
    cache = {
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "RC10"
      resources = { storage = "1Gi" }
    }
  }
`, `(?s)Count does not match the shards.*asks for 1`},
		"unknown mode":          {"  mode = \"sentinel\"\n" + valkeyGroup("cache", 1, "1Gi", ""), "value must be one of"},
		"too few shards":        {"  mode = \"cluster\"\n  shards = 2\n" + valkeyGroup("cache", 2, "1Gi", ""), "between 3 and 100"},
		"two groups":            {two, "must contain at least 1 elements and at most 1"},
		"group name":            {valkeyGroup("Cache_1", 1, "1Gi", ""), "lowercase letters, digits and hyphens"},
		"autoscale without max": {valkeyGroup("cache", 1, "1Gi", "\n        storage_autoscale = { enabled = true }"), "Missing max_storage"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: valkeyConfig(f, tc.body), ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestValkeyClusterResource_UnknownValuesPlan(t *testing.T) {
	f := startFakeServer(t)
	cases := map[string]string{
		"mode": `
  mode = terraform_data.later.output == "x" ? "cluster" : "replication"
` + valkeyGroup("cache", 1, "1Gi", ""),
		"shards": `
  mode   = "cluster"
  shards = terraform_data.later.output == "x" ? 4 : 3
` + valkeyGroup("cache", 3, "1Gi", ""),
		"nodes": `
  nodes = terraform_data.later.output == "x" ? {} : {
    cache = {
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "RC10"
      resources = { storage = "1Gi" }
    }
  }
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:             "resource \"terraform_data\" \"later\" {\n  input = \"y\"\n}\n" + valkeyConfig(f, body),
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				}},
			})
		})
	}
}

func TestValkeyClusterResource_ProvisioningOutcomes(t *testing.T) {
	cases := map[string]struct {
		prep  func(f *fakeServer)
		extra string
		want  string
	}{
		"create rejected": {func(f *fakeServer) {
			f.failWith("CreateDatabase", status.Error(codes.PermissionDenied, "Valkey is not enabled"))
		}, "", "Cannot create cluster"},
		"provisioning failed": {func(f *fakeServer) { f.failProvision = "no capacity" }, "", `(?s)did not become ready.*no capacity`},
		"timeout": {func(f *fakeServer) { f.stuck = true }, `
  timeouts {
    create = "30ms"
  }
`, `(?s)did not become ready.*timed out`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			tc.prep(f)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      replicationConfig(f, "9.1", "8Gi", `{}`, tc.extra),
					ExpectError: regexp.MustCompile(tc.want),
				}, {
					PreConfig: func() {
						f.mu.Lock()
						defer f.mu.Unlock()
						f.stuck, f.failProvision, f.errs = false, "", map[string]error{}
					},
					Config: providerBlock(f),
				}},
			})
		})
	}
}

func TestValkeyClusterResource_DriftAndFailures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	clearAll := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.errs, f.failProvision, f.failDeletion, f.stuck = map[string]error{}, "", "", false
	}
	cfg := replicationConfig(f, "9.1", "8Gi", `{}`, "")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for _, db := range f.databases {
						db.Status.Phase = "deleting"
					}
				},
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for id := range f.databases {
						delete(f.databases, id)
					}
				},
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: cfg},
			{
				PreConfig:   func() { f.failWith("GetDatabase", unavailable) },
				Config:      cfg,
				ExpectError: regexp.MustCompile("Cannot read cluster"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.failWith("UpdateDatabase", unavailable)
				},
				Config:      replicationConfig(f, "9.1", "9Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Cannot update cluster"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.set(func() { f.failProvision = "resize failed" })
				},
				Config:      replicationConfig(f, "9.1", "10Gi", `{}`, ""),
				ExpectError: regexp.MustCompile(`(?s)did not settle after update.*resize failed`),
			},
			{
				PreConfig: func() {
					clearAll()
					f.mu.Lock()
					defer f.mu.Unlock()
					for _, db := range f.databases {
						db.Status.Phase = "running"
					}
					f.errs["DeleteDatabase"] = unavailable
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile("Cannot delete cluster"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.set(func() { f.failDeletion = "finalizer stuck" })
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile(`(?s)was not deleted.*finalizer stuck`),
			},
			{
				PreConfig: func() {
					clearAll()
					f.failWith("DeleteDatabase", status.Error(codes.NotFound, "gone"))
				},
				Config: providerBlock(f),
			},
		},
	})
}

func TestValkeyClusterResource_DeleteTimeoutAndImportErrors(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: replicationConfig(f, "9.1", "8Gi", `{}`, `
  timeouts {
    delete = "30ms"
  }
`)},
			{
				ResourceName:  "relay_valkey_cluster.this",
				ImportState:   true,
				ImportStateId: "missing-slash",
				ExpectError:   regexp.MustCompile("Invalid import ID"),
			},
			{
				PreConfig:   func() { f.set(func() { f.stuck = true }) },
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile(`(?s)was not deleted.*timed out`),
			},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					f.stuck = false
					f.failWhileDeleting = status.Error(codes.Internal, "down")
					for _, db := range f.databases {
						db.Status.Phase = "running"
					}
				},
				Config:      providerBlock(f),
				ExpectError: regexp.MustCompile(`(?s)was not deleted.*down`),
			},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					f.failWhileDeleting = nil
					for id := range f.databases {
						delete(f.databases, id)
					}
				},
				Config: providerBlock(f),
			},
		},
	})
}

func TestValkeyClusterFromProtoDefaultsTheMode(t *testing.T) {
	db := &serverv1.Database{Id: "db-1", Spec: &serverv1.DatabaseSpec{Engine: "valkey"}, Status: &serverv1.DatabaseStatus{}}
	got := valkeyClusterFromProto(db, valkeyClusterModel{ReplicasPerShard: types.Int64Null()})
	if got.Mode.ValueString() != "replication" || !got.Shards.IsNull() {
		t.Errorf("a spec without a valkey config reads as replication mode, got %v shards %v", got.Mode, got.Shards)
	}
}

func TestFakeServerRejectsWhatThePlatformRejectsForValkey(t *testing.T) {
	group := func(count int32) []*serverv1.NodeSpec {
		return []*serverv1.NodeSpec{{Name: "cache", Role: "electable", Count: count, Cloud: "aws", Region: "us-east-1"}}
	}
	cases := map[string]*serverv1.DatabaseSpec{
		"replication tuning": {Engine: "valkey", PrimaryNodeGroup: "cache", Nodes: group(1), Replication: &serverv1.Replication{Mode: "async"}},
		"two groups": {Engine: "valkey", PrimaryNodeGroup: "a", Nodes: []*serverv1.NodeSpec{
			{Name: "a", Role: "electable", Count: 1}, {Name: "b", Role: "electable", Count: 1},
		}},
		"shards in replication mode": {Engine: "valkey", PrimaryNodeGroup: "cache", Nodes: group(1), Valkey: &serverv1.ValkeyConfig{Mode: "replication", Shards: 3}},
		"count off the shards":       {Engine: "valkey", PrimaryNodeGroup: "cache", Nodes: group(4), Valkey: &serverv1.ValkeyConfig{Mode: "cluster", Shards: 3}},
		"no mode":                    {Engine: "valkey", PrimaryNodeGroup: "cache", Nodes: group(1)},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			if _, err := f.CreateDatabase(t.Context(), &serverv1.CreateDatabaseRequest{Name: "sessions", Spec: spec}); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
		})
	}
}
