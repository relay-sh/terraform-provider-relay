package provider

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const dbWorkspace = "ws-db"

func clusterConfig(f *fakeServer, version, storage, parameters, extra string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"
  version      = %q

  nodes = {
    writers = {
      role   = "electable"
      count  = 2
      cloud  = "aws"
      region = "us-east-1"
      tier   = "R10"
      resources = {
        storage = %q
        iops    = 3000
      }
    }

    readers = {
      role  = "read_only"
      count = 1
      tier  = "R20"
      resources = {
        storage = "20Gi"
      }
    }
  }

  parameters = %s

  replication = {
    mode                 = "synchronous"
    synchronous_replicas = 1
  }

  keep_backups_on_destroy = true
%s
}
`, dbWorkspace, version, storage, parameters, extra)
}

func noClustersLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.databases) != 0 {
			return fmt.Errorf("clusters left after destroy: %v", f.databases)
		}
		if !f.lastDBDelete.GetKeepBackups() {
			return errors.New("keep_backups_on_destroy must reach DeleteDatabase")
		}
		return nil
	}
}

func TestPostgreSQLClusterResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noClustersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: clusterConfig(f, "18", "10Gi", `{ work_mem = "64MB" }`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "phase", "running"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.storage_class", "standard"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.iops", "3000"),
					resource.TestCheckNoResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.throughput"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.tier", "R10"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.readers.tier", "R20"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.role", "electable"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.readers.role", "read_only"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.readers.count", "1"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.readers.cloud", ""),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "parameters.work_mem", "64MB"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "replication.synchronous_replicas", "1"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "endpoint_host", "billing.db.relay"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "public_endpoints.0.role", "primary"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "ready_nodes", "1"),
					func(*terraform.State) error {
						calls := f.callsTo("CreateDatabase")
						if len(calls) != 1 || calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						spec := f.lastDBCreate.GetSpec()
						switch {
						case spec.GetEngine() != "postgresql":
							return fmt.Errorf("engine = %q", spec.GetEngine())
						case !spec.GetBackupsEnabled():
							return errors.New("backups must always be requested on")
						case spec.GetNodes()[1].GetProvider() != "aws-use1":
							return fmt.Errorf("cloud and region must resolve to a provisioner, got %q", spec.GetNodes()[1].GetProvider())
						case spec.GetNodes()[0].GetProvider() != "":
							return fmt.Errorf("an unplaced group must not name a provisioner, got %q", spec.GetNodes()[0].GetProvider())
						case spec.GetNodes()[0].GetRole() != "read_only" || spec.GetNodes()[1].GetRole() != "electable":
							return fmt.Errorf("the group's role must be sent, not its label, got %q and %q", spec.GetNodes()[0].GetRole(), spec.GetNodes()[1].GetRole())
						case spec.GetNodes()[1].GetTier() != "R10":
							return fmt.Errorf("writers tier = %q, want R10", spec.GetNodes()[1].GetTier())
						case spec.GetNodes()[0].GetTier() != "R20":
							return fmt.Errorf("readers tier = %q, want R20", spec.GetNodes()[0].GetTier())
						case spec.GetNodes()[1].GetResources().GetCpu() != "" || spec.GetNodes()[1].GetResources().GetMemory() != "":
							return fmt.Errorf("cpu/memory must not be sent, got %q/%q", spec.GetNodes()[1].GetResources().GetCpu(), spec.GetNodes()[1].GetResources().GetMemory())
						}
						return nil
					},
				),
			},
			{
				Config: clusterConfig(f, "18", "20Gi", `{}`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.storage", "20Gi"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "parameters.%", "0"),
					func(*terraform.State) error {
						paths := f.lastDBUpdate.GetUpdateMask().GetPaths()
						if !slices.Equal(paths, []string{"nodes", "postgresql"}) {
							return fmt.Errorf("update mask = %v, want only the changed fields", paths)
						}
						if f.lastDBUpdate.GetSpec().GetPostgresql() == nil {
							return errors.New("an empty parameters map must still send postgresql to reset it")
						}
						return nil
					},
				),
			},
			{
				Config: clusterConfig(f, "18", "20Gi", `{}`, `
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
				Config:      clusterConfig(f, "17", "20Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Version cannot change here"),
			},
			{
				Config:      clusterConfig(f, "18", "5Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Storage cannot shrink"),
			},
			{
				ResourceName: "relay_postgresql_cluster.this",
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return dbWorkspace + "/" + s.RootModule().Resources["relay_postgresql_cluster.this"].Primary.ID, nil
				},
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"keep_backups_on_destroy", "timeouts", "nodes"},
			},
		},
	})
}

func TestPostgreSQLClusterResource_VolumeAttrsInPlace(t *testing.T) {
	f := startFakeServer(t)
	base := func(iops int, diskType, storageClass string) string {
		disk := ""
		if diskType != "" {
			disk = fmt.Sprintf("\n        disk_type = %q", diskType)
		}
		class := ""
		if storageClass != "" {
			class = fmt.Sprintf("\n        storage_class = %q", storageClass)
		}
		return providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"
  version      = "18"

  nodes = {
    writers = {
      role   = "electable"
      count  = 1
      cloud  = "aws"
      region = "us-east-1"
      tier   = "R10"
      resources = {
        storage = "10Gi"
        iops    = %d%s%s
      }
    }
  }
}
`, dbWorkspace, iops, disk, class)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: base(3000, "gp3", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.iops", "3000"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.disk_type", "gp3"),
				),
			},
			{
				Config: base(6000, "io1", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.iops", "6000"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.this", "nodes.writers.resources.disk_type", "io1"),
					func(*terraform.State) error {
						res := f.lastDBUpdate.GetSpec().GetNodes()[0].GetResources()
						if res.GetIops() != 6000 || res.GetDiskType() != "io1" {
							return fmt.Errorf("update must send iops=6000 disk_type=io1, got iops=%d disk_type=%q", res.GetIops(), res.GetDiskType())
						}
						return nil
					},
				),
			},
			{
				Config:      base(6000, "io1", "io2"),
				ExpectError: regexp.MustCompile("Attribute cannot change after creation"),
			},
		},
	})
}

func TestPostgreSQLClusterResource_MinimalCluster(t *testing.T) {
	f := startFakeServer(t)
	config := providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "dev" {
  workspace_id = %q
  name         = "orders"

  nodes = {
    only = {
      role = "electable"
      tier = "R5"
      resources = {
        storage = "5Gi"
      }
    }
  }
}
`, dbWorkspace)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_postgresql_cluster.dev", "nodes.only.count", "1"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.dev", "replication.mode", "async"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.dev", "keep_backups_on_destroy", "false"),
					resource.TestCheckResourceAttr("relay_postgresql_cluster.dev", "created_at", ""),
					func(*terraform.State) error {
						if f.lastDBCreate.GetSpec().GetReplication().GetMode() != "async" {
							return errors.New("replication must always be sent, because omitting it does not clear it")
						}
						return nil
					},
				),
			},
			{
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
		},
	})
}

func TestPostgreSQLClusterResource_PlanGuards(t *testing.T) {
	f := startFakeServer(t)
	f.set(func() { f.providers = nil })
	cases := map[string]struct {
		body string
		want string
	}{
		"synchronous without a replica count": {`
  replication = { mode = "synchronous" }`, "Missing synchronous_replicas"},
		"unknown replication mode": {`
  replication = { mode = "semi" }`, "Attribute replication.mode value must be one of"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			config := providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"

  nodes = {
    writers = {
      role = "electable"
      tier = "R10"
      resources = { storage = "5Gi" }
    }
  }
%s
}
`, dbWorkspace, tc.body)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: config, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestPostgreSQLClusterResource_RequiresANodeGroup(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"
  nodes        = {}
}
`, dbWorkspace),
			ExpectError: regexp.MustCompile("must contain at least 1"),
		}},
	})
}

func TestPostgreSQLClusterResource_RejectsAnUnknownRole(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"

  nodes = {
    writers = {
      role = "data"
      tier = "R10"
      resources = { storage = "5Gi" }
    }
  }
}
`, dbWorkspace),
			ExpectError: regexp.MustCompile(`(?s)nodes\["writers"\].role value must be one of.*electable.*read_only.*got: "data"`),
		}},
	})
}

func TestFakeServerRejectsWhatThePlatformRejects(t *testing.T) {
	cases := map[string]*serverv1.NodeSpec{
		"an unknown role":      {Role: "primary"},
		"tags from the client": {Role: roleReadOnly, Tags: map[string]string{"nosync": "true"}},
	}
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			node.Resources = &serverv1.ResourceRequirements{Storage: "5Gi"}
			req := &serverv1.CreateDatabaseRequest{Name: "billing", Spec: &serverv1.DatabaseSpec{
				Engine: "postgresql",
				Nodes:  []*serverv1.NodeSpec{node},
			}}
			if _, err := f.CreateDatabase(t.Context(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
		})
	}
}

func TestPostgreSQLClusterResource_UnservedPlacement(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"

  nodes = {
    writers = {
      role   = "electable"
      cloud  = "fly"
      region = "gru"
      tier = "R10"
      resources = { storage = "5Gi" }
    }
  }
}
`, dbWorkspace),
			ExpectError: regexp.MustCompile("Cannot place the cluster"),
		}},
	})
}

func TestPostgreSQLClusterResource_NodeGroupChanges(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	writers := func(count int, region string) string {
		return providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"

  nodes = {
    writers = {
      role   = "electable"
      count  = %d
      cloud  = "aws"
      region = %q
      tier = "R10"
      resources = { storage = "5Gi" }
    }
  }
}
`, dbWorkspace, count, region)
	}
	withReaders := providerBlock(f) + fmt.Sprintf(`
resource "relay_postgresql_cluster" "this" {
  workspace_id = %q
  name         = "billing"

  nodes = {
    writers = {
      role   = "electable"
      count  = 1
      cloud  = "aws"
      region = "us-east-1"
      tier = "R10"
      resources = { storage = "5Gi" }
    }

    readers = {
      role   = "read_only"
      cloud  = "fly"
      region = "gru"
      tier = "R10"
      resources = { storage = "5Gi" }
    }
  }
}
`, dbWorkspace)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: writers(3, "us-east-1")},
			{Config: writers(1, "us-east-1")},
			{Config: writers(1, "eu-west-1")},
			{
				Config:      withReaders,
				ExpectError: regexp.MustCompile("Cannot place the cluster"),
			},
			{
				PreConfig:   func() { f.failAfter("ListDatabaseProviders", 1, unavailable) },
				Config:      writers(2, "us-east-1"),
				ExpectError: regexp.MustCompile("Cannot list the platform's provisioners"),
			},
			{
				PreConfig: func() { f.failWith("ListDatabaseProviders", nil) },
				Config:    writers(2, "us-east-1"),
			},
		},
	})
}

func TestPostgreSQLClusterResource_ProvisioningOutcomes(t *testing.T) {
	cases := map[string]struct {
		prep  func(f *fakeServer)
		extra string
		want  string
	}{
		"placement catalog unavailable": {func(f *fakeServer) {
			f.failWith("ListDatabaseProviders", status.Error(codes.Unavailable, "down"))
		}, "", "Cannot list the platform's provisioners"},
		"create rejected": {func(f *fakeServer) {
			f.failWith("CreateDatabase", status.Error(codes.FailedPrecondition, "plan limit"))
		}, "", "Cannot create cluster"},
		"provisioning failed": {func(f *fakeServer) { f.failProvision = "no capacity" }, "", `(?s)did not become ready.*no capacity`},
		"timeout": {func(f *fakeServer) { f.stuck = true }, `
  timeouts {
    create = "30ms"
  }
`, `(?s)did not become ready.*timed out`},
		"status read failure": {func(f *fakeServer) {
			f.readyAfter = 5
			f.failWith("GetDatabase", status.Error(codes.Unavailable, "down"))
		}, "", `(?s)did not become ready.*down`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			tc.prep(f)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      clusterConfig(f, "18", "10Gi", `{}`, tc.extra),
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

func TestPostgreSQLClusterResource_DriftAndFailures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	clearAll := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.errs, f.failProvision, f.failDeletion, f.stuck = map[string]error{}, "", "", false
	}
	cfg := clusterConfig(f, "18", "10Gi", `{}`, "")
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
					f.failWith("ListDatabaseProviders", unavailable)
				},
				Config:      cfg,
				ExpectError: regexp.MustCompile("Cannot list the platform's provisioners"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.failWith("UpdateDatabase", unavailable)
				},
				Config:      clusterConfig(f, "18", "11Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Cannot update cluster"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.set(func() { f.failProvision = "resize failed" })
				},
				Config:      clusterConfig(f, "18", "12Gi", `{}`, ""),
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

func TestPostgreSQLClusterResource_DeleteTimeoutAndImportErrors(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: clusterConfig(f, "18", "10Gi", `{}`, `
  timeouts {
    delete = "30ms"
  }
`)},
			{
				ResourceName:  "relay_postgresql_cluster.this",
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
					f.failWhileDeleting = status.Error(codes.Unavailable, "down")
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

func TestPostgreSQLClusterDataSource(t *testing.T) {
	f := startFakeServer(t)
	for _, name := range []string{"alpha", "billing", "zeta"} {
		id := "db-" + name
		f.databases[id] = &serverv1.Database{
			Id: id, Name: name, WorkspaceId: dbWorkspace,
			Spec:   &serverv1.DatabaseSpec{Engine: "postgresql", Version: "18"},
			Status: &serverv1.DatabaseStatus{Phase: "running", TotalNodes: 3, Endpoint: &serverv1.Endpoint{Host: name + ".db", Port: 5432}, PrivateEndpoints: []*serverv1.PublicEndpoint{{Role: "primary", Host: name + ".pl", Port: 5432}}},
		}
	}
	f.databases["db-mongo"] = &serverv1.Database{
		Id: "db-mongo", Name: "zeta", WorkspaceId: dbWorkspace,
		Spec:   &serverv1.DatabaseSpec{Engine: "mongodb"},
		Status: &serverv1.DatabaseStatus{Phase: "running"},
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
data "relay_postgresql_cluster" "by_name" {
  workspace_id = %[1]q
  name         = "zeta"
}

data "relay_postgresql_cluster" "by_id" {
  workspace_id = %[1]q
  id           = "db-billing"
}
`, dbWorkspace),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.relay_postgresql_cluster.by_name", "id", "db-zeta"),
				resource.TestCheckResourceAttr("data.relay_postgresql_cluster.by_name", "private_endpoints.0.host", "zeta.pl"),
				resource.TestCheckResourceAttr("data.relay_postgresql_cluster.by_id", "name", "billing"),
				resource.TestCheckResourceAttr("data.relay_postgresql_cluster.by_id", "endpoint_port", "5432"),
				resource.TestCheckResourceAttr("data.relay_postgresql_cluster.by_id", "total_nodes", "3"),
				func(*terraform.State) error {
					if n := len(f.callsTo("ListDatabases")); n < 3 {
						return fmt.Errorf("lookup by name must page through the list, got %d calls", n)
					}
					return nil
				},
			),
		}},
	})
}

func TestPostgreSQLClusterDataSource_Errors(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	cases := map[string]struct {
		body   string
		method string
		err    error
		want   string
	}{
		"missing by name": {`name = "nope"`, "", nil, "Cluster not found"},
		"missing by id":   {`id = "nope"`, "", nil, "Cluster not found"},
		"list failure":    {`name = "x"`, "ListDatabases", unavailable, "Cannot list clusters"},
		"get failure":     {`id = "x"`, "GetDatabase", unavailable, "Cannot read cluster"},
		"both keys":       {"id = \"a\"\n  name = \"b\"", "", nil, "Invalid Attribute Combination"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			if tc.method != "" {
				f.failWith(tc.method, tc.err)
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      providerBlock(f) + fmt.Sprintf("data \"relay_postgresql_cluster\" \"x\" {\n  workspace_id = %q\n  %s\n}\n", dbWorkspace, tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}
