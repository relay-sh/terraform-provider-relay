package provider

import (
	"fmt"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

func regionStatusAttrs(region, role, link, held string) map[string]string {
	return map[string]string{"cloud": "aws", "region": region, "role": role, "replication_link": link, "held": held, "total_nodes": "2"}
}

func TestValkeyClusterResource_RegionsStatus(t *testing.T) {
	f := startFakeServer(t)
	east, eu := standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)
	const addr = "relay_valkey_cluster.this"
	standby := standbyConfig(f, east, eu)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: standby,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "regions.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", regionStatusAttrs("us-east-1", "primary", "", "")),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", regionStatusAttrs("eu-west-1", "standby", "Up", "")),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{"region": "eu-west-1", "phase": "running", "ready_nodes": "1", "message": ""}),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{"region": "us-east-1", "endpoint": ""}),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{"region": "eu-west-1", "endpoint": ""}),
				),
			},
			{Config: standby, PlanOnly: true},
			{
				PreConfig: func() {
					f.set(func() {
						f.regionEndpoint["aws/us-east-1"] = "cache.seg.cell-a.example"
						f.regionEndpoint["aws/eu-west-1"] = "cache.seg.cell-b.example"
					})
				},
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{"region": "us-east-1", "role": "primary", "endpoint": "cache.seg.cell-a.example"}),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{"region": "eu-west-1", "role": "standby", "endpoint": "cache.seg.cell-b.example"}),
				),
			},
			{Config: standby, PlanOnly: true},
			{
				PreConfig:    func() { f.set(func() { clear(f.regionEndpoint) }) },
				RefreshState: true,
				Check:        resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{"region": "eu-west-1", "endpoint": ""}),
			},
			{
				PreConfig: func() {
					f.set(func() {
						f.regionLink["aws/eu-west-1"] = "Down"
						f.regionHeld["aws/eu-west-1"] = "the standby has not synchronized"
						f.regionMessage["aws/eu-west-1"] = "replication link is down"
					})
				},
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "regions.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", regionStatusAttrs("us-east-1", "primary", "", "")),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{
						"region": "eu-west-1", "role": "standby", "replication_link": "Down", "held": "the standby has not synchronized", "message": "replication link is down",
					}),
				),
			},
			{Config: standby, PlanOnly: true},
			{
				Config: valkeyConfig(f, valkeyGroup("cache", 2, "8Gi", "")+standbyBlock("standby", east, eu)+"  parameters = { maxmemory-policy = \"allkeys-lru\" }\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
					plancheck.ExpectUnknownValue(addr, tfjsonpath.New("regions")),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "regions.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", map[string]string{"region": "eu-west-1", "replication_link": "Down"}),
				),
			},
			{
				PreConfig: func() {
					f.set(func() {
						delete(f.regionLink, "aws/eu-west-1")
						delete(f.regionHeld, "aws/eu-west-1")
						delete(f.regionMessage, "aws/eu-west-1")
					})
				},
				RefreshState: true,
				Check:        resource.TestCheckTypeSetElemNestedAttrs(addr, "regions.*", regionStatusAttrs("eu-west-1", "standby", "Up", "")),
			},
		},
	})
}

func TestValkeyClusterResource_RegionsEmptyWithoutStandby(t *testing.T) {
	f := startFakeServer(t)
	const addr = "relay_valkey_cluster.this"
	config := valkeyConfig(f, valkeyGroup("cache", 1, "1Gi", ""))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{Config: config, Check: resource.TestCheckResourceAttr(addr, "regions.#", "0")},
			{Config: config, PlanOnly: true},
			{RefreshState: true, Check: resource.TestCheckResourceAttr(addr, "regions.#", "0")},
		},
	})
}

func TestRegionsFromProtoSortsByCloudAndRegion(t *testing.T) {
	in := []*serverv1.RegionStatus{
		{Cloud: "gcp", Region: "europe-west1"},
		{Cloud: "aws", Region: "us-east-1"},
		{Cloud: "aws", Region: "eu-west-1", Role: "standby", Phase: "running", ReadyNodes: 1, TotalNodes: 2, ReplicationLink: "Down", Held: "why", Message: "m"},
	}
	var order []string
	for _, e := range regionsFromProto(in).Elements() {
		attrs := e.(types.Object).Attributes()
		order = append(order, attrs["cloud"].(types.String).ValueString()+"/"+attrs["region"].(types.String).ValueString())
	}
	if want := []string{"aws/eu-west-1", "aws/us-east-1", "gcp/europe-west1"}; !slices.Equal(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
	if in[0].GetCloud() != "gcp" {
		t.Error("the input slice was reordered")
	}
	if got := regionsFromProto(nil); got.IsNull() || len(got.Elements()) != 0 {
		t.Errorf("no regions reads as an empty set, got %v", got)
	}
}

func TestValkeyClusterDataSource_StandbyRegions(t *testing.T) {
	f := startFakeServer(t)
	f.set(func() {
		f.regionLink["aws/eu-west-1"] = "Down"
		f.regionHeld["aws/eu-west-1"] = "the standby has not synchronized"
		f.regionMessage["aws/eu-west-1"] = "replication link is down"
		f.regionEndpoint["aws/us-east-1"] = "sessions.seg.cell-a.example"
		f.regionEndpoint["aws/eu-west-1"] = "sessions.seg.cell-b.example"
		f.databases["db-standby"] = &serverv1.Database{
			Id: "db-standby", Name: "sessions", WorkspaceId: dbWorkspace,
			Spec: &serverv1.DatabaseSpec{
				Engine: "valkey", Version: "9.1", Valkey: &serverv1.ValkeyConfig{Mode: "replication"},
				Nodes: []*serverv1.NodeSpec{{Name: "main", Count: 2, Cloud: "aws", Region: "us-east-1", Tier: "RC10"}},
				Distribution: &serverv1.Distribution{Mode: "standby", Clouds: map[string]*serverv1.CloudRegions{
					"aws": {Regions: map[string]*serverv1.RegionPlacement{"us-east-1": {Primary: true}, "eu-west-1": {}}},
				}},
			},
			Status: &serverv1.DatabaseStatus{Phase: "running", TotalNodes: 2, ReadyNodes: 2},
		}
		f.databases["db-plain"] = &serverv1.Database{
			Id: "db-plain", Name: "plain", WorkspaceId: dbWorkspace,
			Spec:   &serverv1.DatabaseSpec{Engine: "valkey", Version: "9.1", Nodes: []*serverv1.NodeSpec{{Name: "main", Count: 1}}},
			Status: &serverv1.DatabaseStatus{Phase: "running"},
		}
	})
	const standby, plain = "data.relay_valkey_cluster.standby", "data.relay_valkey_cluster.plain"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{{
			Config: providerBlock(f) + fmt.Sprintf(`
data "relay_valkey_cluster" "standby" {
  workspace_id = %[1]q
  name         = "sessions"
}

data "relay_valkey_cluster" "plain" {
  workspace_id = %[1]q
  id           = "db-plain"
}
`, dbWorkspace),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(standby, "distribution.mode", "standby"),
				resource.TestCheckResourceAttr(standby, "distribution.region.#", "2"),
				resource.TestCheckTypeSetElemNestedAttrs(standby, "distribution.region.*", map[string]string{"cloud": "aws", "region": "us-east-1", "primary": "true"}),
				resource.TestCheckTypeSetElemNestedAttrs(standby, "distribution.region.*", map[string]string{"cloud": "aws", "region": "eu-west-1", "primary": "false"}),
				resource.TestCheckResourceAttr(standby, "regions.#", "2"),
				resource.TestCheckTypeSetElemNestedAttrs(standby, "regions.*", map[string]string{
					"cloud": "aws", "region": "us-east-1", "role": "primary", "phase": "running", "ready_nodes": "2", "total_nodes": "2", "replication_link": "", "held": "", "message": "",
					"endpoint": "sessions.seg.cell-a.example",
				}),
				resource.TestCheckTypeSetElemNestedAttrs(standby, "regions.*", map[string]string{
					"cloud": "aws", "region": "eu-west-1", "role": "standby", "replication_link": "Down", "held": "the standby has not synchronized", "message": "replication link is down",
					"endpoint": "sessions.seg.cell-b.example",
				}),
				resource.TestCheckNoResourceAttr(plain, "distribution"),
				resource.TestCheckResourceAttr(plain, "regions.#", "0"),
			),
		}},
	})
}
