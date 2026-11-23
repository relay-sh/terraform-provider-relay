package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

const pgAddr = "relay_postgresql_cluster.this"

func pgStandbyConfig(f *fakeServer, parameters string, regions ...string) string {
	return clusterConfig(f, "18", "10Gi", parameters, standbyBlock("standby", regions...))
}

func moveGroupsTo(config, region string) string {
	config = strings.Replace(config, "region = \"us-east-1\"\n      tier   = \"R10\"", "region = \""+region+"\"\n      tier   = \"R10\"", 1)
	return strings.Replace(config, "region = \"us-east-1\"\n      count = 1", "region = \""+region+"\"\n      count = 1", 1)
}

func storedPlacement(f *fakeServer, region string) *serverv1.RegionPlacement {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, db := range f.databases {
		return db.GetSpec().GetDistribution().GetClouds()["aws"].GetRegions()[region]
	}
	return nil
}

func TestPostgreSQLClusterResource_StandbyDistribution(t *testing.T) {
	f := startFakeServer(t)
	east, eu := standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)
	movedEast, movedEu := "    region {\n      cloud  = \"aws\"\n      region = \"us-east-1\"\n    }\n", standbyRegion("aws", "eu-west-1", true)
	const noParameters = `{}`
	const workMem = `{ work_mem = "64MB" }`
	sa := standbyRegion("aws", "sa-east-1", false)
	wantMask := func(paths ...string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := f.lastDBUpdate.GetUpdateMask().GetPaths(); !slices.Equal(got, paths) {
				return fmt.Errorf("update mask %v, want %v", got, paths)
			}
			return nil
		}
	}
	wantMembers := func(region string, want int32) resource.TestCheckFunc {
		return func(*terraform.State) error {
			sent := f.lastDBUpdate.GetSpec().GetDistribution().GetClouds()["aws"].GetRegions()[region]
			if sent.GetMembers() != want || storedPlacement(f, region).GetMembers() != want {
				return fmt.Errorf("members of %s sent %d, stored %d, want %d", region, sent.GetMembers(), storedPlacement(f, region).GetMembers(), want)
			}
			return nil
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noClustersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: pgStandbyConfig(f, noParameters, east, eu),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pgAddr, "distribution.mode", "standby"),
					resource.TestCheckResourceAttr(pgAddr, "distribution.region.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(pgAddr, "distribution.region.*", map[string]string{"cloud": "aws", "region": "us-east-1", "primary": "true"}),
					resource.TestCheckTypeSetElemNestedAttrs(pgAddr, "distribution.region.*", map[string]string{"cloud": "aws", "region": "eu-west-1", "primary": "false"}),
					resource.TestCheckResourceAttr(pgAddr, "regions.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(pgAddr, "regions.*", map[string]string{"region": "us-east-1", "role": "primary", "replication_link": ""}),
					resource.TestCheckTypeSetElemNestedAttrs(pgAddr, "regions.*", map[string]string{"region": "eu-west-1", "role": "standby", "replication_link": "Up"}),
					func(*terraform.State) error {
						spec := f.lastDBCreate.GetSpec()
						d := spec.GetDistribution()
						if d.GetMode() != "standby" || !slices.Equal(regionSet(t, d), []string{"aws/eu-west-1=false", "aws/us-east-1=true"}) {
							return fmt.Errorf("distribution sent: %v", d)
						}
						for _, p := range d.GetClouds()["aws"].GetRegions() {
							if p.Members != nil || p.Priority != nil {
								return fmt.Errorf("members and priority are not modelled and must not be sent: %v", p)
							}
						}
						if spec.GetDcs() != nil {
							return fmt.Errorf("no dcs is sent for a standby: %v", spec.GetDcs())
						}
						return nil
					},
				),
			},
			{Config: pgStandbyConfig(f, noParameters, eu, east), PlanOnly: true},
			{Config: pgStandbyConfig(f, noParameters, "    region {\n      cloud  = \"aws\"\n      region = \"eu-west-1\"\n    }\n", east), PlanOnly: true},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for _, db := range f.databases {
						two := int32(2)
						db.Spec.Distribution.Clouds["aws"].Regions["eu-west-1"].Members = &two
					}
				},
				Config: pgStandbyConfig(f, noParameters, movedEast, movedEu),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(pgAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(pgAddr, "distribution.region.*", map[string]string{"region": "eu-west-1", "primary": "true"}),
					wantMask("distribution"),
					wantMembers("eu-west-1", 2),
				),
			},
			{
				PreConfig: func() { f.set(func() { f.unsyncedStandby = true }) },
				Config:    pgStandbyConfig(f, noParameters, east, eu),
				ExpectError: regexp.MustCompile(`(?s)Cannot promote the standby region.*cannot be promoted yet.*` +
					`does\s+not\s+force\s+promotions.*API,\s+CLI\s+or\s+dashboard`),
			},
			{
				PreConfig: func() { f.set(func() { f.unsyncedStandby = false }) },
				Config:    pgStandbyConfig(f, `{ work_mem = "64MB" }`, movedEast, movedEu),
				Check:     wantMask("postgresql"),
			},
			{
				Config: pgStandbyConfig(f, workMem, movedEast, movedEu, sa),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(pgAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pgAddr, "distribution.region.#", "3"),
					resource.TestCheckResourceAttr(pgAddr, "regions.#", "3"),
					resource.TestCheckTypeSetElemNestedAttrs(pgAddr, "regions.*", map[string]string{"region": "sa-east-1", "role": "standby"}),
					wantMask("distribution"),
					wantMembers("eu-west-1", 2),
				),
			},
			{Config: pgStandbyConfig(f, workMem, movedEast, movedEu, sa), PlanOnly: true},
			{
				Config:      pgStandbyConfig(f, workMem, movedEast, movedEu, sa, standbyRegion("aws", "ap-south-1", false), standbyRegion("aws", "eu-central-1", false)),
				ExpectError: regexp.MustCompile(`(?s)Cannot update cluster.*one\s+region\s+of\s+a\s+standby\s+database\s+per\s+update,\s+this\s+update\s+changes\s+2`),
			},
			{
				Config:      pgStandbyConfig(f, workMem, movedEast, movedEu, sa, standbyRegion("aws", "ap-south-1", false)),
				PreConfig:   func() { f.set(func() { f.noTopologyOps = true }) },
				ExpectError: regexp.MustCompile(`(?s)Cannot update cluster.*topology\s+operations\s+are\s+not\s+enabled`),
			},
			{
				Config:      pgStandbyConfig(f, noParameters, movedEast, movedEu),
				PreConfig:   func() { f.set(func() { f.noTopologyOps = false }) },
				ExpectError: regexp.MustCompile(`(?s)Cannot update cluster.*region\s+change\s+alone\s+and\s+the\s+other\s+fields`),
			},
			{
				Config:      pgStandbyConfig(f, workMem, east, eu, sa, standbyRegion("aws", "ap-south-1", false)),
				ExpectError: regexp.MustCompile(`(?s)Cannot update cluster.*primary\s+marker\s+together\s+with\s+a\s+region\s+change`),
			},
			{
				Config:      pgStandbyConfig(f, workMem, east, sa),
				ExpectError: regexp.MustCompile(`(?s)Cannot update cluster.*region\s+aws/eu-west-1\s+holds\s+the\s+primary\s+marker,\s+so\s+it\s+cannot\s+be\s+removed`),
			},
			{
				Config:      pgStandbyConfig(f, workMem, movedEu, sa),
				ExpectError: regexp.MustCompile(`(?s)Node group outside the distribution.*aws/us-east-1.*aws/eu-west-1,\s+aws/sa-east-1`),
			},
			{
				Config: pgStandbyConfig(f, workMem, movedEast, movedEu),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pgAddr, "distribution.region.#", "2"),
					resource.TestCheckResourceAttr(pgAddr, "regions.#", "2"),
					wantMask("distribution"),
					wantMembers("eu-west-1", 2),
				),
			},
			{
				Config:      pgStandbyConfig(f, workMem, movedEu),
				ExpectError: regexp.MustCompile(`Too few regions`),
			},
			{
				Config:      clusterConfig(f, "18", "10Gi", noParameters, ""),
				ExpectError: regexp.MustCompile(`Distribution cannot be removed`),
			},
			{
				Config:      moveGroupsTo(pgStandbyConfig(f, noParameters, movedEast, movedEu), "eu-west-1"),
				ExpectError: regexp.MustCompile(`(?s)Node group cannot move.*primary.*distribution`),
			},
			{
				PreConfig:   func() { f.failAfter("GetDatabase", 1, status.Error(codes.PermissionDenied, "denied")) },
				Config:      pgStandbyConfig(f, noParameters, east, eu),
				ExpectError: regexp.MustCompile(`Cannot read cluster`),
			},
			{
				PreConfig: func() { f.set(func() { delete(f.errs, "GetDatabase"); delete(f.skip, "GetDatabase") }) },
				Config:    pgStandbyConfig(f, `{ work_mem = "64MB" }`, movedEast, movedEu),
				PlanOnly:  true,
			},
		},
	})
}

func TestPostgreSQLClusterResource_DistributionCannotBeAddedLater(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		CheckDestroy:             noClustersLeft(f),
		Steps: []resource.TestStep{
			{
				Config: clusterConfig(f, "18", "10Gi", `{}`, ""),
				Check:  resource.TestCheckResourceAttr(pgAddr, "regions.#", "0"),
			},
			{
				Config:      pgStandbyConfig(f, `{}`, standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)),
				ExpectError: regexp.MustCompile(`Distribution cannot be added`),
			},
			{
				PreConfig: func() {
					f.set(func() {
						for _, db := range f.databases {
							db.Spec.Distribution = &serverv1.Distribution{Mode: "stretched", Clouds: map[string]*serverv1.CloudRegions{
								"aws": {Regions: map[string]*serverv1.RegionPlacement{"us-east-1": {Primary: true}, "eu-west-1": {}}},
							}}
						}
					})
				},
				Config:   clusterConfig(f, "18", "10Gi", `{}`, ""),
				PlanOnly: true,
			},
		},
	})
}

func TestPostgreSQLClusterResource_StandbyAcrossClustersRefused(t *testing.T) {
	f := startFakeServer(t)
	f.set(func() { f.noStandbyMesh = true })
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config:      pgStandbyConfig(f, `{}`, standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)),
			ExpectError: regexp.MustCompile(`(?s)Cannot create cluster.*standby\s+across\s+clusters\s+is\s+not\s+supported\s+yet`),
		}},
	})
}

func TestPostgreSQLClusterResource_DistributionConfigValidation(t *testing.T) {
	f := startFakeServer(t)
	east, eu := standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)
	cases := map[string]struct {
		extra string
		want  string
	}{
		"one region":   {standbyBlock("standby", east), "Too few regions"},
		"no primary":   {standbyBlock("standby", standbyRegion("aws", "us-east-1", false), eu), "Exactly one primary region"},
		"two primary":  {standbyBlock("standby", east, standbyRegion("aws", "eu-west-1", true)), "Exactly one primary region"},
		"duplicate":    {standbyBlock("standby", east, eu, standbyRegion("aws", "us-east-1", false)), "Duplicate region"},
		"stretched":    {standbyBlock("stretched", east, eu), `(?s)Stretched distribution is not available.*member count per region.*API, CLI or dashboard`},
		"unknown mode": {standbyBlock("mirror", east, eu), "Unsupported distribution mode"},
		"node outside": {standbyBlock("standby", standbyRegion("aws", "eu-west-1", true), standbyRegion("gcp", "us-central1", false)),
			`(?s)Node group outside the distribution.*aws/eu-west-1, gcp/us-central1`},
		"restore": {standbyBlock("standby", east, eu) + "  restore = { source_database_id = \"db-src\" }\n", "No restore into a standby"},
		"no mode": {"\n  distribution {\n" + east + eu + "  }\n", "Missing mode"},
		"no cloud": {"\n  distribution {\n    mode = \"standby\"\n    region {\n      region = \"us-east-1\"\n    }\n  }\n",
			`"cloud" is required|Missing Configuration for Required Attribute`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: clusterConfig(f, "18", "10Gi", `{}`, tc.extra), PlanOnly: true, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
	t.Run("groups in two regions", func(t *testing.T) {
		config := strings.Replace(pgStandbyConfig(f, `{}`, east, eu), "region = \"us-east-1\"\n      count = 1", "region = \"eu-west-1\"\n      count = 1", 1)
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: factories(relay.Dial),
			Steps:                    []resource.TestStep{{Config: config, PlanOnly: true, ExpectError: regexp.MustCompile(`(?s)One cluster, one region.*copies\s+of\s+the\s+whole\s+cluster`)}},
		})
	})
}

func TestPostgreSQLClusterResource_DistributionUnknownValuesPlan(t *testing.T) {
	f := startFakeServer(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: "resource \"terraform_data\" \"later\" {\n  input = \"y\"\n}\n" + clusterConfig(f, "18", "10Gi", `{}`, standbyBlock("standby",
				standbyRegion("aws", "us-east-1", true),
				"    region {\n      cloud   = \"aws\"\n      region  = terraform_data.later.output\n    }\n")),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		}},
	})
}

func TestPostgreSQLClusterDataSource_StandbyAndStretched(t *testing.T) {
	f := startFakeServer(t)
	f.set(func() {
		f.regionLink["aws/eu-west-1"] = "Down"
		spec := func(d *serverv1.Distribution) *serverv1.DatabaseSpec {
			return &serverv1.DatabaseSpec{
				Engine: "postgresql", Version: "18", Distribution: d,
				Nodes: []*serverv1.NodeSpec{{Name: "main", Count: 2, Cloud: "aws", Region: "us-east-1", Tier: "R10"}},
			}
		}
		regions := map[string]*serverv1.CloudRegions{"aws": {Regions: map[string]*serverv1.RegionPlacement{"us-east-1": {Primary: true}, "eu-west-1": {}}}}
		f.databases["db-standby"] = &serverv1.Database{Id: "db-standby", Name: "ledger", WorkspaceId: dbWorkspace,
			Spec: spec(&serverv1.Distribution{Mode: "standby", Clouds: regions}), Status: &serverv1.DatabaseStatus{Phase: "running", TotalNodes: 2, ReadyNodes: 2}}
		f.databases["db-stretched"] = &serverv1.Database{Id: "db-stretched", Name: "wide", WorkspaceId: dbWorkspace,
			Spec: spec(&serverv1.Distribution{Mode: "stretched", Clouds: regions}), Status: &serverv1.DatabaseStatus{Phase: "running"}}
		f.databases["db-plain"] = &serverv1.Database{Id: "db-plain", Name: "plain", WorkspaceId: dbWorkspace,
			Spec: spec(nil), Status: &serverv1.DatabaseStatus{Phase: "running"}}
	})
	const standby, stretched, plain = "data.relay_postgresql_cluster.standby", "data.relay_postgresql_cluster.stretched", "data.relay_postgresql_cluster.plain"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
data "relay_postgresql_cluster" "standby" {
  workspace_id = %[1]q
  name         = "ledger"
}

data "relay_postgresql_cluster" "stretched" {
  workspace_id = %[1]q
  id           = "db-stretched"
}

data "relay_postgresql_cluster" "plain" {
  workspace_id = %[1]q
  id           = "db-plain"
}
`, dbWorkspace),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(standby, "distribution.mode", "standby"),
				resource.TestCheckTypeSetElemNestedAttrs(standby, "distribution.region.*", map[string]string{"cloud": "aws", "region": "us-east-1", "primary": "true"}),
				resource.TestCheckTypeSetElemNestedAttrs(standby, "distribution.region.*", map[string]string{"cloud": "aws", "region": "eu-west-1", "primary": "false"}),
				resource.TestCheckResourceAttr(standby, "regions.#", "2"),
				resource.TestCheckTypeSetElemNestedAttrs(standby, "regions.*", map[string]string{"region": "eu-west-1", "role": "standby", "replication_link": "Down"}),
				resource.TestCheckNoResourceAttr(stretched, "distribution"),
				resource.TestCheckNoResourceAttr(plain, "distribution"),
				resource.TestCheckResourceAttr(plain, "regions.#", "0"),
			),
		}},
	})
}

func TestFakeServerRejectsWhatThePlatformRejectsForPostgreSQLDistribution(t *testing.T) {
	one, two, three := int32(1), int32(2), int32(3)
	zero := int32(0)
	dist := func(mode string, regions map[string]*serverv1.RegionPlacement) *serverv1.Distribution {
		return &serverv1.Distribution{Mode: mode, Clouds: map[string]*serverv1.CloudRegions{"aws": {Regions: regions}}}
	}
	pair := func(a, b *serverv1.RegionPlacement) map[string]*serverv1.RegionPlacement {
		return map[string]*serverv1.RegionPlacement{"us-east-1": a, "eu-west-1": b}
	}
	spec := func(d *serverv1.Distribution, dcs string) *serverv1.DatabaseSpec {
		s := &serverv1.DatabaseSpec{
			Engine: "postgresql", Distribution: d,
			Nodes: []*serverv1.NodeSpec{{Name: "main", Role: "electable", Count: 3, Cloud: "aws", Region: "us-east-1"}},
		}
		if dcs != "" {
			s.Dcs = &serverv1.DCS{Type: dcs}
		}
		return s
	}
	primary, standby := &serverv1.RegionPlacement{Primary: true}, &serverv1.RegionPlacement{}
	reject := map[string]*serverv1.DatabaseSpec{
		"unknown mode":            spec(dist("mirror", pair(primary, standby)), ""),
		"one region":              spec(dist("standby", map[string]*serverv1.RegionPlacement{"us-east-1": primary}), ""),
		"no primary":              spec(dist("standby", pair(standby, standby)), ""),
		"zero members":            spec(dist("standby", pair(&serverv1.RegionPlacement{Primary: true, Members: &zero}, standby)), ""),
		"priority on standby":     spec(dist("standby", pair(&serverv1.RegionPlacement{Primary: true, Priority: &one}, standby)), ""),
		"priority below one":      spec(dist("stretched", pair(&serverv1.RegionPlacement{Primary: true, Priority: &zero, Members: &three}, standby)), "etcd"),
		"shared priority":         spec(dist("stretched", pair(&serverv1.RegionPlacement{Primary: true, Priority: &one, Members: &two}, &serverv1.RegionPlacement{Priority: &one, Members: &one})), "etcd"),
		"stretched without dcs":   spec(dist("stretched", pair(&serverv1.RegionPlacement{Primary: true, Members: &two}, &serverv1.RegionPlacement{Members: &one})), ""),
		"stretched no members":    spec(dist("stretched", pair(primary, standby)), "etcd"),
		"stretched wrong sum":     spec(dist("stretched", pair(&serverv1.RegionPlacement{Primary: true, Members: &two}, &serverv1.RegionPlacement{Members: &two})), "etcd"),
		"node outside the spread": spec(dist("standby", map[string]*serverv1.RegionPlacement{"eu-west-1": primary, "ap-south-1": standby}), ""),
	}
	for name, s := range reject {
		t.Run(name, func(t *testing.T) {
			if err := validatePostgreSQLDistribution(s); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
		})
	}
	accept := map[string]*serverv1.DatabaseSpec{
		"none":                 spec(nil, ""),
		"local":                spec(dist("local", nil), ""),
		"standby":              spec(dist("standby", pair(primary, standby)), ""),
		"standby with members": spec(dist("standby", pair(&serverv1.RegionPlacement{Primary: true, Members: &one}, standby)), ""),
		"stretched":            spec(dist("stretched", pair(&serverv1.RegionPlacement{Primary: true, Members: &two, Priority: &one}, &serverv1.RegionPlacement{Members: &one, Priority: &two})), "etcd"),
		"another engine":       {Engine: "valkey", Distribution: dist("mirror", nil)},
	}
	for name, s := range accept {
		if err := validatePostgreSQLDistribution(s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRejectDistributionChange(t *testing.T) {
	s, yes, no := types.StringValue, types.BoolValue(true), types.BoolValue(false)
	east := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), yes), regionObject(s("aws"), s("eu-west-1"), no))
	moved := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), no), regionObject(s("aws"), s("eu-west-1"), yes))
	added := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), yes), regionObject(s("aws"), s("eu-west-1"), no), regionObject(s("aws"), s("sa-east-1"), no))
	partial := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), yes), regionObject(s("aws"), types.StringUnknown(), no))
	null, unknown := types.ObjectNull(distributionAttrTypes), types.ObjectUnknown(distributionAttrTypes)
	for name, tc := range map[string]struct {
		plan, prior types.Object
		want        string
	}{
		"both absent":      {null, null, ""},
		"unknown plan":     {unknown, east, ""},
		"added":            {east, null, "Distribution cannot be added"},
		"removed":          {null, east, "Distribution cannot be removed"},
		"marker moves":     {moved, east, ""},
		"unchanged":        {east, east, ""},
		"region added":     {added, east, ""},
		"region removed":   {east, added, ""},
		"region not known": {partial, east, ""},
		"regions unknown": {types.ObjectValueMust(distributionAttrTypes, map[string]attr.Value{
			"mode": s("standby"), "region": types.SetUnknown(distributionRegionType),
		}), east, ""},
	} {
		var diags diag.Diagnostics
		rejectDistributionChange(tc.plan, tc.prior, &diags)
		if tc.want == "" && diags.HasError() || tc.want != "" && (len(diags) != 1 || diags[0].Summary() != tc.want) {
			t.Errorf("%s: %v", name, diags)
		}
	}
}

func TestRejectGroupMove(t *testing.T) {
	group := func(cloud, region types.String) nodeGroupModel { return nodeGroupModel{Cloud: cloud, Region: region} }
	s := types.StringValue
	prior := map[string]nodeGroupModel{"main": group(s("aws"), s("us-east-1"))}
	spread := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), types.BoolValue(true)), regionObject(s("aws"), s("eu-west-1"), types.BoolValue(false)))
	for name, tc := range map[string]struct {
		plan         map[string]nodeGroupModel
		distribution types.Object
		moves        bool
	}{
		"unchanged":       {map[string]nodeGroupModel{"main": group(s("aws"), s("US-EAST-1"))}, spread, false},
		"region moves":    {map[string]nodeGroupModel{"main": group(s("aws"), s("eu-west-1"))}, spread, true},
		"cloud moves":     {map[string]nodeGroupModel{"main": group(s("gcp"), s("us-east-1"))}, spread, true},
		"no distribution": {map[string]nodeGroupModel{"main": group(s("aws"), s("eu-west-1"))}, types.ObjectNull(distributionAttrTypes), false},
		"new group":       {map[string]nodeGroupModel{"extra": group(s("aws"), s("eu-west-1"))}, spread, false},
		"unknown region":  {map[string]nodeGroupModel{"main": group(s("aws"), types.StringUnknown())}, spread, false},
		"unknown cloud":   {map[string]nodeGroupModel{"main": group(types.StringUnknown(), s("eu-west-1"))}, spread, false},
	} {
		var diags diag.Diagnostics
		rejectGroupMove(tc.plan, prior, tc.distribution, &diags)
		if diags.HasError() != tc.moves {
			t.Errorf("%s: %v", name, diags)
		}
	}
}

func TestStandbyUpdatesKeepStoredMembersForValkey(t *testing.T) {
	f := startFakeServer(t)
	east, eu := standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)
	movedEast, movedEu := "    region {\n      cloud  = \"aws\"\n      region = \"us-east-1\"\n    }\n", standbyRegion("aws", "eu-west-1", true)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: standbyConfig(f, east, eu)},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for _, db := range f.databases {
						two := int32(2)
						db.Spec.Distribution.Clouds["aws"].Regions["eu-west-1"].Members = &two
					}
				},
				Config: standbyConfig(f, movedEast, movedEu),
				Check: func(*terraform.State) error {
					if got := storedPlacement(f, "eu-west-1").GetMembers(); got != 2 {
						return fmt.Errorf("stored members %d, want 2", got)
					}
					return nil
				},
			},
			{
				PreConfig:   func() { f.failAfter("GetDatabase", 1, status.Error(codes.PermissionDenied, "denied")) },
				Config:      standbyConfig(f, east, eu),
				ExpectError: regexp.MustCompile(`Cannot read cluster`),
			},
			{
				PreConfig: func() { f.set(func() { delete(f.errs, "GetDatabase"); delete(f.skip, "GetDatabase") }) },
				Config:    standbyConfig(f, movedEast, movedEu),
				PlanOnly:  true,
			},
		},
	})
}
