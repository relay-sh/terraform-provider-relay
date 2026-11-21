package provider

import (
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/relay-sh/relay-go/relay"
)

type managedCluster struct {
	addr         string
	config       func(f *fakeServer, extra string) string
	restore      func(f *fakeServer, extra string) string
	importIgnore []string
}

var managedClusters = map[string]managedCluster{
	"postgresql": {
		addr:   postgresAddr,
		config: func(f *fakeServer, extra string) string { return postgresRestoreConfig(f, "", extra, 1) },
		restore: func(f *fakeServer, extra string) string {
			return postgresRestoreConfig(f, restoreBlock("db-src", ""), extra, 1)
		},
		importIgnore: []string{"keep_backups_on_destroy", "timeouts", "nodes"},
	},
	"valkey": {
		addr: valkeyAddr,
		config: func(f *fakeServer, extra string) string {
			return valkeyConfig(f, extra+valkeyGroup("cache", 1, "8Gi", ""))
		},
		restore: func(f *fakeServer, extra string) string {
			return valkeyConfig(f, restoreBlock("db-src", "")+extra+valkeyGroup("cache", 1, "8Gi", ""))
		},
		importIgnore: []string{"keep_backups_on_destroy", "timeouts"},
	},
	"documentdb": {
		addr:   documentDBAddr,
		config: func(f *fakeServer, extra string) string { return documentDBCluster(f, "17", "", extra) },
		restore: func(f *fakeServer, extra string) string {
			return documentDBRestoreConfig(f, restoreBlock("db-src", ""), extra, 1)
		},
		importIgnore: []string{"timeouts"},
	},
}

func seedManagedSource(f *fakeServer, engine string) {
	switch engine {
	case "postgresql":
		seedSources(f, postgresSource("db-src", "18"))
	case "valkey":
		seedSources(f, valkeySource("db-src", "9.1", valkeyModeReplication, "cache", true))
	default:
		seedSources(f, documentDBSource("db-src", "18"))
	}
}

func clearStoredManagedBy(f *fakeServer) {
	f.set(func() {
		for _, db := range f.databases {
			db.ManagedBy = ""
		}
	})
}

func lastUpdateOnlyMarks(f *fakeServer, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		req := f.lastDBMark
		if req == nil || req.GetSpec() != nil || req.GetUpdateMask() != nil || req.ManagedBy == nil || req.GetManagedBy() != want {
			return fmt.Errorf("UpdateDatabase must carry only id and managed_by = %q, got %v", want, req)
		}
		return nil
	}
}

func TestClusterManagedBy(t *testing.T) {
	for engine, c := range managedClusters {
		t.Run(engine, func(t *testing.T) {
			f := startFakeServer(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{
					{
						Config: c.config(f, ""),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(c.addr, "managed_by", "terraform"),
							func(*terraform.State) error {
								if got := f.lastDBCreate.GetManagedBy(); got != "terraform" {
									return fmt.Errorf("CreateDatabase managed_by = %q", got)
								}
								return nil
							},
							callCount(f, "UpdateDatabase", 0),
						),
					},
					{
						PreConfig: func() { clearStoredManagedBy(f) },
						Config:    c.config(f, ""),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(c.addr, "managed_by", "terraform"),
							callCount(f, "UpdateDatabase", 1),
							lastUpdateOnlyMarks(f, "terraform"),
						),
					},
					{
						Config: c.config(f, ""),
						Check:  callCount(f, "UpdateDatabase", 1),
					},
					{
						Config: c.config(f, "  managed_by = \"pulumi\"\n"),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(c.addr, "managed_by", "pulumi"),
							callCount(f, "UpdateDatabase", 2),
							lastUpdateOnlyMarks(f, "pulumi"),
						),
					},
					{
						ResourceName: c.addr,
						ImportState:  true,
						ImportStateIdFunc: func(s *terraform.State) (string, error) {
							return dbWorkspace + "/" + s.RootModule().Resources[c.addr].Primary.ID, nil
						},
						ImportStateVerify:       true,
						ImportStateVerifyIgnore: c.importIgnore,
					},
					{
						Config: c.config(f, "  managed_by = \"\"\n"),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(c.addr, "managed_by", ""),
							callCount(f, "UpdateDatabase", 3),
							lastUpdateOnlyMarks(f, ""),
						),
					},
				},
			})
		})
	}
}

func TestClusterManagedByUpdateFails(t *testing.T) {
	for engine, c := range managedClusters {
		t.Run(engine, func(t *testing.T) {
			f := startFakeServer(t)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{
					{Config: c.config(f, "")},
					{
						PreConfig: func() {
							clearStoredManagedBy(f)
							f.failWith("UpdateDatabase", status.Error(codes.InvalidArgument, "spec is required"))
						},
						Config:      c.config(f, ""),
						ExpectError: regexp.MustCompile(`(?s)Cannot change managed_by.*spec is required`),
					},
					{
						PreConfig: func() { f.failWith("UpdateDatabase", nil) },
						Config:    c.config(f, ""),
						Check:     resource.TestCheckResourceAttr(c.addr, "managed_by", "terraform"),
					},
				},
			})
		})
	}
}

func TestClusterManagedByOnRestore(t *testing.T) {
	for engine, c := range managedClusters {
		t.Run(engine, func(t *testing.T) {
			f := startFakeServer(t)
			seedManagedSource(f, engine)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config: c.restore(f, ""),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(c.addr, "managed_by", "terraform"),
						callCount(f, "UpdateDatabase", 1),
						lastUpdateOnlyMarks(f, "terraform"),
					),
				}},
			})
		})
	}
}

func TestClusterManagedByFailsOnRestore(t *testing.T) {
	for engine, c := range managedClusters {
		t.Run(engine, func(t *testing.T) {
			f := startFakeServer(t)
			seedManagedSource(f, engine)
			f.failWith("UpdateDatabase", errors.New("older server"))
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      c.restore(f, ""),
					ExpectError: regexp.MustCompile(`Cannot change managed_by`),
				}},
			})
		})
	}
}

func TestClusterDataSourceManagedBy(t *testing.T) {
	for engine, c := range managedClusters {
		t.Run(engine, func(t *testing.T) {
			f := startFakeServer(t)
			data := fmt.Sprintf(`
data "relay_%s_cluster" "by_id" {
  workspace_id = %q
  id           = %s.id
}
`, engine, dbWorkspace, c.addr)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config: c.config(f, "") + data,
					Check:  resource.TestCheckResourceAttr("data.relay_"+engine+"_cluster.by_id", "managed_by", "terraform"),
				}},
			})
		})
	}
}
