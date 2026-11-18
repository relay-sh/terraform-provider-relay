package provider

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const documentDBAddr = "relay_documentdb_cluster.this"

func documentDBConfig(f *fakeServer, body string) string {
	return providerBlock(f) + fmt.Sprintf(`
resource "relay_documentdb_cluster" "this" {
  workspace_id = %q
  name         = "docs"
%s
}
`, dbWorkspace, body)
}

func documentDBGroup(label string, count int, tier string) string {
	return fmt.Sprintf(`
  nodes = {
    %s = {
      count     = %d
      cloud     = "aws"
      region    = "us-east-1"
      tier      = %q
      resources = {}
    }
  }
`, label, count, tier)
}

func documentDBCluster(f *fakeServer, version, size, extra string) string {
	storage := ""
	if size != "" {
		storage = fmt.Sprintf("  storage = { size = %q }\n", size)
	}
	return documentDBConfig(f, fmt.Sprintf("  version = %q\n%s%s%s", version, storage, extra, documentDBGroup("main", 2, "R10")))
}

func TestDocumentDBClusterResource_Lifecycle(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy: func(*terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.databases) != 0 || f.lastDBDelete.GetKeepBackups() {
				return fmt.Errorf("destroy must delete every cluster without keeping backups: %v %v", f.databases, f.lastDBDelete)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: documentDBCluster(f, "17", "30Gi", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBAddr, "phase", "running"),
					resource.TestCheckResourceAttr(documentDBAddr, "version", "17"),
					resource.TestCheckResourceAttr(documentDBAddr, "storage.size", "30Gi"),
					resource.TestCheckResourceAttr(documentDBAddr, "nodes.main.count", "2"),
					resource.TestCheckResourceAttr(documentDBAddr, "nodes.main.resources.cpu", "2"),
					resource.TestCheckResourceAttr(documentDBAddr, "endpoint_host", "docs.public"),
					resource.TestCheckResourceAttr(documentDBAddr, "endpoint_port", "27017"),
					resource.TestCheckResourceAttr(documentDBAddr, "public_endpoints.#", "2"),
					resource.TestCheckResourceAttr(documentDBAddr, "public_endpoints.1.role", "ro"),
					resource.TestCheckResourceAttr(documentDBAddr, "public_endpoints.1.host", "docs-ro.public"),
					resource.TestCheckNoResourceAttr(documentDBAddr, "default_connections.readwrite"),
					resource.TestCheckResourceAttr(documentDBAddr, "default_connections.readonly", "true"),
					resource.TestCheckResourceAttr(documentDBAddr, "default_connections.admin", "true"),
					func(*terraform.State) error {
						calls := f.callsTo("CreateDatabase")
						if len(calls) != 1 || calls[0].workspace != dbWorkspace {
							return fmt.Errorf("create must be workspace-scoped: %+v", calls)
						}
						req := f.lastDBCreate
						spec, n := req.GetSpec(), req.GetSpec().GetNodes()[0]
						switch {
						case spec.GetEngine() != "documentdb" || spec.GetVersion() != "17":
							return fmt.Errorf("engine and version = %q %q", spec.GetEngine(), spec.GetVersion())
						case spec.GetBackupsEnabled():
							return errors.New("DocumentDB refuses backups, so they must not be requested")
						case spec.GetValkey() != nil || spec.GetPostgresql() != nil || spec.GetReplication() != nil:
							return errors.New("no engine settings may be sent for DocumentDB")
						case spec.GetPrimaryNodeGroup() != "main" || n.GetRole() != "data" || n.GetAutoscaling() != nil:
							return fmt.Errorf("the only group must be sent as the data primary without autoscaling, got %q/%q/%v", spec.GetPrimaryNodeGroup(), n.GetRole(), n.GetAutoscaling())
						case spec.GetStorage().GetSize() != "30Gi" || spec.GetStorage().GetAutoscale() != nil:
							return fmt.Errorf("storage = %+v", spec.GetStorage())
						case !req.GetDefaultConnections().GetReadonly() || req.GetDefaultConnections().GetReadwrite() || !req.GetDefaultConnections().GetAdmin():
							return fmt.Errorf("default connections = %+v, want readonly and admin", req.GetDefaultConnections())
						}
						return nil
					},
				),
			},
			{
				Config: documentDBConfig(f, "  version = \"17\"\n  storage = { size = \"40Gi\" }\n"+documentDBGroup("main", 3, "R20")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(documentDBAddr, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBAddr, "storage.size", "40Gi"),
					resource.TestCheckResourceAttr(documentDBAddr, "nodes.main.count", "3"),
					resource.TestCheckResourceAttr(documentDBAddr, "nodes.main.resources.cpu", "4"),
					func(*terraform.State) error {
						if paths := f.lastDBUpdate.GetUpdateMask().GetPaths(); !slices.Equal(paths, []string{"nodes", "storage"}) {
							return fmt.Errorf("mask = %v", paths)
						}
						return nil
					},
				),
			},
			{
				Config:      documentDBConfig(f, "  version = \"17\"\n  storage = { size = \"20Gi\" }\n"+documentDBGroup("main", 3, "R20")),
				ExpectError: regexp.MustCompile(`(?s)Storage cannot shrink.*at 40Gi`),
			},
			{
				Config:           documentDBConfig(f, documentDBGroup("main", 3, "R20")),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{
				Config: documentDBConfig(f, "  version = \"17\"\n  storage = { size = \"40Gi\" }\n"+documentDBGroup("main", 3, "R20")+`
  timeouts {
    update = "5m"
  }
`),
				Check: callCount(f, "UpdateDatabase", 1),
			},
			{
				ResourceName: documentDBAddr,
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return dbWorkspace + "/" + s.RootModule().Resources[documentDBAddr].Primary.ID, nil
				},
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{
				ResourceName:  documentDBAddr,
				ImportState:   true,
				ImportStateId: "missing-slash",
				ExpectError:   regexp.MustCompile("Invalid import ID"),
			},
		},
	})
}

func TestDocumentDBClusterResource_PlatformPicksVersionAndStorage(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: documentDBConfig(f, documentDBGroup("main", 1, "R20")),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBAddr, "storage.size", "40Gi"),
					resource.TestCheckResourceAttr(documentDBAddr, "version", ""),
					func(*terraform.State) error {
						if spec := f.lastDBCreate.GetSpec(); spec.GetStorage() != nil || spec.GetVersion() != "" {
							return fmt.Errorf("omitted storage and version must not be sent, got %+v", spec)
						}
						return nil
					},
				),
			},
			{
				Config: documentDBConfig(f, "  storage = {}\n"+documentDBGroup("main", 1, "R20")),
				Check:  callCount(f, "UpdateDatabase", 0),
			},
		},
	})
}

func TestDocumentDBClusterResource_Replacements(t *testing.T) {
	f := startFakeServer(t)
	replaceAction := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(documentDBAddr, plancheck.ResourceActionDestroyBeforeCreate)},
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: documentDBCluster(f, "17", "", "")},
			{
				Config:           documentDBCluster(f, "18", "", ""),
				ConfigPlanChecks: replaceAction,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBAddr, "version", "18"),
					callCount(f, "CreateDatabase", 2),
					callCount(f, "UpdateDatabase", 0),
				),
			},
			{
				Config:           documentDBConfig(f, "  version = \"18\"\n"+documentDBGroup("store", 2, "R10")),
				ConfigPlanChecks: replaceAction,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBAddr, "nodes.store.resources.memory", "4Gi"),
					callCount(f, "CreateDatabase", 3),
				),
			},
		},
	})
}

func TestDocumentDBClusterResource_ConfigValidation(t *testing.T) {
	f := startFakeServer(t)
	two := `
  nodes = {
    a = {
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "R10"
      resources = {}
    }
    b = {
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "R10"
      resources = {}
    }
  }
`
	cases := map[string]struct {
		body string
		want string
	}{
		"unknown version": {"  version = \"16\"\n" + documentDBGroup("main", 1, "R10"), "value must be one of"},
		"cache tier":      {documentDBGroup("main", 1, "RC10"), `(?s)RC\s+cache\s+tiers\s+are\s+for\s+Valkey`},
		"zero nodes":      {documentDBGroup("main", 0, "R10"), "must be at least 1"},
		"two groups":      {two, "must contain at least 1 elements and at most 1"},
		"group name":      {documentDBGroup("Main_1", 1, "R10"), "lowercase letters, digits and hyphens"},
		"backups":         {"  keep_backups_on_destroy = true\n" + documentDBGroup("main", 1, "R10"), `An argument named "keep_backups_on_destroy" is not expected here`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: documentDBConfig(f, tc.body), ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestDocumentDBClusterResource_UnknownTierPlans(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: documentDBCluster(f, "17", "", "")},
			{
				Config: "resource \"terraform_data\" \"later\" {\n  input = \"R20\"\n}\n" +
					documentDBConfig(f, "  version = \"17\"\n"+`
  nodes = {
    main = {
      count     = 2
      cloud     = "aws"
      region    = "us-east-1"
      tier      = terraform_data.later.output
      resources = {}
    }
  }
`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestDocumentDBClusterResource_DefaultConnections(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: documentDBCluster(f, "17", "", "  default_connections = { admin = false }\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(documentDBAddr, "default_connections.readonly", "true"),
					resource.TestCheckResourceAttr(documentDBAddr, "default_connections.admin", "false"),
					sentDefaultConnections(f, &serverv1.DefaultConnectionsIntent{Readonly: true}),
				),
			},
			{
				Config: documentDBCluster(f, "17", "", "  default_connections = { admin = true }\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					setDefaultConnectionsTo(f, &serverv1.DefaultConnectionsIntent{Readonly: true, Admin: true}),
					callCount(f, "UpdateDatabase", 0),
				),
			},
			{
				PreConfig:   func() { f.failWith("SetDefaultConnections", status.Error(codes.FailedPrecondition, "not available")) },
				Config:      documentDBCluster(f, "17", "", "  default_connections = { readonly = false }\n"),
				ExpectError: regexp.MustCompile("Cannot change default connections"),
			},
		},
	})
}

func TestDocumentDBClusterResource_ProvisioningOutcomes(t *testing.T) {
	cases := map[string]struct {
		prep func(f *fakeServer)
		want string
	}{
		"create rejected": {func(f *fakeServer) {
			f.failWith("CreateDatabase", status.Error(codes.FailedPrecondition, "The Free plan does not include DocumentDB"))
		}, `(?s)Cannot create cluster.*Free plan`},
		"provisioning failed": {func(f *fakeServer) { f.failProvision = "no capacity" }, `(?s)did not become ready.*no capacity`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			tc.prep(f)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      documentDBCluster(f, "17", "", ""),
					ExpectError: regexp.MustCompile(tc.want),
				}, {
					PreConfig: func() {
						f.mu.Lock()
						defer f.mu.Unlock()
						f.failProvision, f.errs = "", map[string]error{}
					},
					Config: providerBlock(f),
				}},
			})
		})
	}
}

func TestDocumentDBClusterResource_DriftAndFailures(t *testing.T) {
	f := startFakeServer(t)
	unavailable := status.Error(codes.Unavailable, "down")
	clearAll := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.errs, f.failProvision = map[string]error{}, ""
	}
	cfg := documentDBCluster(f, "17", "30Gi", "")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					f.set(func() {
						for _, db := range f.databases {
							db.Status.Phase = "deleting"
						}
					})
				},
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() {
					f.set(func() {
						for id := range f.databases {
							delete(f.databases, id)
						}
					})
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
				Config:      documentDBCluster(f, "17", "31Gi", ""),
				ExpectError: regexp.MustCompile("Cannot update cluster"),
			},
			{
				PreConfig: func() {
					clearAll()
					f.set(func() { f.failProvision = "resize failed" })
				},
				Config:      documentDBCluster(f, "17", "32Gi", ""),
				ExpectError: regexp.MustCompile(`(?s)did not settle after update.*resize failed`),
			},
			{
				PreConfig: func() {
					clearAll()
					f.set(func() {
						for _, db := range f.databases {
							db.Status.Phase = "running"
						}
					})
				},
				Config: providerBlock(f),
			},
		},
	})
}

func TestDocumentDBClusterResource_ImportRejectsOtherEngine(t *testing.T) {
	f := startFakeServer(t)
	f.databases["db-other"] = &serverv1.Database{
		Id: "db-other", Name: "other", WorkspaceId: dbWorkspace,
		Spec:   &serverv1.DatabaseSpec{Engine: "valkey"},
		Status: &serverv1.DatabaseStatus{Phase: "running"},
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:        documentDBCluster(f, "17", "", ""),
			ResourceName:  documentDBAddr,
			ImportState:   true,
			ImportStateId: dbWorkspace + "/db-other",
			ExpectError:   regexp.MustCompile(`(?s)is a valkey cluster.*relay_valkey_cluster`),
		}},
	})
}

func TestFakeServerRejectsWhatThePlatformRejectsForDocumentDB(t *testing.T) {
	group := func(role string) []*serverv1.NodeSpec {
		return []*serverv1.NodeSpec{{Name: "main", Role: role, Count: 1, Cloud: "aws", Region: "us-east-1", Tier: "R10"}}
	}
	cases := map[string]*serverv1.DatabaseSpec{
		"version":           {Engine: "documentdb", Version: "16", PrimaryNodeGroup: "main", Nodes: group("data")},
		"backups":           {Engine: "documentdb", BackupsEnabled: true, PrimaryNodeGroup: "main", Nodes: group("data")},
		"valkey settings":   {Engine: "documentdb", PrimaryNodeGroup: "main", Nodes: group("data"), Valkey: &serverv1.ValkeyConfig{}},
		"two groups":        {Engine: "documentdb", PrimaryNodeGroup: "a", Nodes: []*serverv1.NodeSpec{{Name: "a", Role: "data"}, {Name: "b", Role: "data"}}},
		"storage autoscale": {Engine: "documentdb", PrimaryNodeGroup: "main", Nodes: group("data"), Storage: &serverv1.ClusterStorage{Autoscale: &serverv1.StorageAutoscale{Enabled: true}}},
		"primary":           {Engine: "documentdb", PrimaryNodeGroup: "other", Nodes: group("data")},
		"electable role":    {Engine: "documentdb", PrimaryNodeGroup: "main", Nodes: group("electable")},
		"compute autoscale": {Engine: "documentdb", PrimaryNodeGroup: "main", Nodes: []*serverv1.NodeSpec{{Name: "main", Role: "data", Autoscaling: &serverv1.ComputeAutoscaling{Enabled: true}}}},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			if _, err := f.CreateDatabase(t.Context(), &serverv1.CreateDatabaseRequest{Name: "docs", Spec: spec}); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
		})
	}
}

func TestDocumentDBClusterDataSource(t *testing.T) {
	f := startFakeServer(t)
	f.databases["db-vk"] = &serverv1.Database{
		Id: "db-vk", Name: "docs", WorkspaceId: dbWorkspace,
		Spec:   &serverv1.DatabaseSpec{Engine: "valkey"},
		Status: &serverv1.DatabaseStatus{Phase: "running"},
	}
	f.databases["db-docs"] = &serverv1.Database{
		Id: "db-docs", Name: "docs", WorkspaceId: dbWorkspace,
		Spec: &serverv1.DatabaseSpec{
			Engine: "documentdb", Version: "18",
			Nodes: []*serverv1.NodeSpec{{
				Name: "main", Role: "data", Count: 3, Cloud: "aws", Region: "us-east-1", Tier: "R20",
				Resources: &serverv1.ResourceRequirements{Cpu: "4", Memory: "8Gi"},
			}},
			Storage: &serverv1.ClusterStorage{Size: "40Gi"},
		},
		Status: &serverv1.DatabaseStatus{
			Phase: "running", TotalNodes: 3, ReadyNodes: 3, Endpoint: &serverv1.Endpoint{Host: "docs.db", Port: 27017},
			PublicEndpoints: []*serverv1.PublicEndpoint{{Role: "rw", Host: "docs.db", Port: 27017}, {Role: "ro", Host: "docs-ro.db", Port: 27017}},
		},
		DefaultConnections: &serverv1.DefaultConnectionsIntent{Readonly: true},
		DeletionProtection: true,
	}
	f.databases["db-plain"] = &serverv1.Database{
		Id: "db-plain", Name: "plain", WorkspaceId: dbWorkspace,
		Spec:   &serverv1.DatabaseSpec{Engine: "documentdb", Version: "17", Nodes: []*serverv1.NodeSpec{{Name: "main", Count: 1}}},
		Status: &serverv1.DatabaseStatus{Phase: "provisioning"},
	}
	const byName, byID = "data.relay_documentdb_cluster.by_name", "data.relay_documentdb_cluster.by_id"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
data "relay_documentdb_cluster" "by_name" {
  workspace_id = %[1]q
  name         = "docs"
}

data "relay_documentdb_cluster" "by_id" {
  workspace_id = %[1]q
  id           = "db-plain"
}
`, dbWorkspace),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(byName, "id", "db-docs"),
				resource.TestCheckResourceAttr(byName, "version", "18"),
				resource.TestCheckResourceAttr(byName, "nodes.main.tier", "R20"),
				resource.TestCheckResourceAttr(byName, "nodes.main.resources.memory", "8Gi"),
				resource.TestCheckResourceAttr(byName, "storage.size", "40Gi"),
				resource.TestCheckResourceAttr(byName, "deletion_protection", "true"),
				resource.TestCheckResourceAttr(byName, "endpoint_port", "27017"),
				resource.TestCheckResourceAttr(byName, "public_endpoints.1.host", "docs-ro.db"),
				resource.TestCheckResourceAttr(byName, "ready_nodes", "3"),
				resource.TestCheckResourceAttr(byID, "name", "plain"),
				resource.TestCheckResourceAttr(byID, "phase", "provisioning"),
				resource.TestCheckNoResourceAttr(byID, "storage"),
			),
		}},
	})
}

func TestDocumentDBClusterDataSource_Errors(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "down")
	cases := map[string]struct {
		body   string
		method string
		want   string
	}{
		"missing by name": {`name = "nope"`, "", "Cluster not found"},
		"missing by id":   {`id = "nope"`, "", "Cluster not found"},
		"other engine":    {`id = "db-pg"`, "", "Cluster not found"},
		"list failure":    {`name = "x"`, "ListDatabases", "Cannot list clusters"},
		"get failure":     {`id = "x"`, "GetDatabase", "Cannot read cluster"},
		"both keys":       {"id = \"a\"\n  name = \"b\"", "", "Invalid Attribute Combination"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeServer(t)
			f.databases["db-pg"] = &serverv1.Database{Id: "db-pg", Name: "pg", WorkspaceId: dbWorkspace, Spec: &serverv1.DatabaseSpec{Engine: "postgresql"}, Status: &serverv1.DatabaseStatus{Phase: "running"}}
			if tc.method != "" {
				f.failWith(tc.method, unavailable)
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:      providerBlock(f) + fmt.Sprintf("data \"relay_documentdb_cluster\" \"x\" {\n  workspace_id = %q\n  %s\n}\n", dbWorkspace, tc.body),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}
