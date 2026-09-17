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

func databaseConfig(f *fakeServer, version, storage, pgParams, extra string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_database" "this" {
  workspace_id = %q
  name         = "billing"
  engine       = "postgresql"
  version      = %q

  nodes = [{
    role     = "data"
    count    = 2
    provider = "aws"
    region   = "us-east-1"
    tier     = "R10"
    resources = {
      storage = %q
      iops    = 3000
    }
    tags = { env = "gold" }
  }]

  params                = { max_connections = "200" }
  postgresql_parameters = %s
  backups_enabled       = true

  replication = {
    mode                 = "synchronous"
    synchronous_replicas = 1
  }

  default_connections = {
    readonly = true
  }

  keep_backups_on_destroy = true
%s
}
`, dbWorkspace, version, storage, pgParams, extra)
}

func noDatabasesLeft(f *fakeServer) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.databases) != 0 {
			return fmt.Errorf("databases left after destroy: %v", f.databases)
		}
		if !f.lastDBDelete.GetKeepBackups() {
			return errors.New("keep_backups_on_destroy must reach DeleteDatabase")
		}
		return nil
	}
}

func TestDatabaseResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noDatabasesLeft(f),
		Steps: []resource.TestStep{
			{
				Config: databaseConfig(f, "17", "10Gi", `{ work_mem = "64MB" }`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_database.this", "phase", "running"),
					resource.TestCheckResourceAttr("relay_database.this", "nodes.0.resources.storage_class", "standard"),
					resource.TestCheckResourceAttr("relay_database.this", "nodes.0.resources.iops", "3000"),
					resource.TestCheckNoResourceAttr("relay_database.this", "nodes.0.resources.throughput"),
					resource.TestCheckResourceAttr("relay_database.this", "nodes.0.tier", "R10"),
					resource.TestCheckResourceAttr("relay_database.this", "nodes.0.tags.env", "gold"),
					resource.TestCheckResourceAttr("relay_database.this", "postgresql_parameters.work_mem", "64MB"),
					resource.TestCheckResourceAttr("relay_database.this", "replication.synchronous_replicas", "1"),
					resource.TestCheckResourceAttr("relay_database.this", "endpoint_host", "billing.db.relay"),
					resource.TestCheckResourceAttr("relay_database.this", "public_endpoints.0.role", "primary"),
					resource.TestCheckResourceAttr("relay_database.this", "ready_nodes", "1"),
					func(*terraform.State) error {
						calls := f.callsTo("CreateDatabase")
						if len(calls) != 1 || calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						if !f.lastDBCreate.GetDefaultConnections().GetReadonly() || f.lastDBCreate.GetDefaultConnections().GetAdmin() {
							return errors.New("default_connections must reach CreateDatabase")
						}
						n := f.lastDBCreate.GetSpec().GetNodes()[0]
						if n.GetTier() != "R10" {
							return fmt.Errorf("tier = %q, want R10", n.GetTier())
						}
						if n.GetResources().GetCpu() != "" || n.GetResources().GetMemory() != "" {
							return fmt.Errorf("cpu/memory must not be sent, got %q/%q", n.GetResources().GetCpu(), n.GetResources().GetMemory())
						}
						return nil
					},
				),
			},
			{
				Config: databaseConfig(f, "17", "20Gi", `{}`, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("relay_database.this", "nodes.0.resources.storage", "20Gi"),
					resource.TestCheckResourceAttr("relay_database.this", "postgresql_parameters.%", "0"),
					func(*terraform.State) error {
						paths := f.lastDBUpdate.GetUpdateMask().GetPaths()
						if !slices.Equal(paths, []string{"nodes", "postgresql"}) {
							return fmt.Errorf("update mask = %v, want only the changed fields", paths)
						}
						if f.lastDBUpdate.GetSpec().GetPostgresql() == nil {
							return errors.New("an empty postgresql_parameters map must still send postgresql to reset it")
						}
						return nil
					},
				),
			},
			{
				Config: databaseConfig(f, "17", "20Gi", `{}`, `
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
				Config:      databaseConfig(f, "18", "20Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Version cannot change here"),
			},
			{
				ResourceName: "relay_database.this",
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return dbWorkspace + "/" + s.RootModule().Resources["relay_database.this"].Primary.ID, nil
				},
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"default_connections", "keep_backups_on_destroy", "timeouts"},
			},
		},
	})
}

func TestDatabaseResource_ProvisioningOutcomes(t *testing.T) {
	cases := map[string]struct {
		prep  func(f *fakeServer)
		extra string
		want  string
	}{
		"create rejected": {func(f *fakeServer) {
			f.failWith("CreateDatabase", status.Error(codes.FailedPrecondition, "plan limit"))
		}, "", "Cannot create database"},
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
					Config:      databaseConfig(f, "17", "10Gi", `{}`, tc.extra),
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

func TestDatabaseResource_DriftAndFailures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	clearAll := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.errs, f.failProvision, f.failDeletion, f.stuck = map[string]error{}, "", "", false
	}
	cfg := databaseConfig(f, "17", "10Gi", `{}`, "")
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
				ExpectError: regexp.MustCompile("Cannot read database"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.failWith("UpdateDatabase", unavailable)
				},
				Config:      databaseConfig(f, "17", "11Gi", `{}`, ""),
				ExpectError: regexp.MustCompile("Cannot update database"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.set(func() { f.failProvision = "resize failed" })
				},
				Config:      databaseConfig(f, "17", "12Gi", `{}`, ""),
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
				ExpectError: regexp.MustCompile("Cannot delete database"),
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

func TestDatabaseResource_DeleteTimeoutAndImportErrors(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: databaseConfig(f, "17", "10Gi", `{}`, `
  timeouts {
    delete = "30ms"
  }
`)},
			{
				ResourceName:  "relay_database.this",
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

func TestDatabaseDataSource(t *testing.T) {
	f := startFakeServer(t)
	for _, name := range []string{"alpha", "billing", "zeta"} {
		id := "db-" + name
		f.databases[id] = &serverv1.Database{
			Id: id, Name: name, WorkspaceId: dbWorkspace,
			Spec:   &serverv1.DatabaseSpec{Engine: "postgresql", Version: "17", BackupsEnabled: true},
			Status: &serverv1.DatabaseStatus{Phase: "running", Endpoint: &serverv1.Endpoint{Host: name + ".db", Port: 5432}, PrivateEndpoints: []*serverv1.PublicEndpoint{{Role: "primary", Host: name + ".pl", Port: 5432}}},
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
data "relay_database" "by_name" {
  workspace_id = %[1]q
  name         = "zeta"
}

data "relay_database" "by_id" {
  workspace_id = %[1]q
  id           = "db-billing"
}
`, dbWorkspace),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("data.relay_database.by_name", "id", "db-zeta"),
				resource.TestCheckResourceAttr("data.relay_database.by_name", "private_endpoints.0.host", "zeta.pl"),
				resource.TestCheckResourceAttr("data.relay_database.by_id", "name", "billing"),
				resource.TestCheckResourceAttr("data.relay_database.by_id", "endpoint_port", "5432"),
				resource.TestCheckResourceAttr("data.relay_database.by_id", "backups_enabled", "true"),
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

func TestDatabaseDataSource_Errors(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	cases := map[string]struct {
		body   string
		method string
		err    error
		want   string
	}{
		"missing by name": {`name = "nope"`, "", nil, "Database not found"},
		"missing by id":   {`id = "nope"`, "", nil, "Database not found"},
		"list failure":    {`name = "x"`, "ListDatabases", unavailable, "Cannot list databases"},
		"get failure":     {`id = "x"`, "GetDatabase", unavailable, "Cannot read database"},
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
					Config:      providerBlock(f) + fmt.Sprintf("data \"relay_database\" \"x\" {\n  workspace_id = %q\n  %s\n}\n", dbWorkspace, tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}
