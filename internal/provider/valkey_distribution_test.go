package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"
)

func standbyBlock(mode string, regions ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  distribution {\n    mode = %q\n", mode)
	for _, r := range regions {
		b.WriteString(r)
	}
	b.WriteString("  }\n")
	return b.String()
}

func standbyRegion(cloud, region string, primary bool) string {
	return fmt.Sprintf("    region {\n      cloud   = %q\n      region  = %q\n      primary = %t\n    }\n", cloud, region, primary)
}

func standbyConfig(f *fakeServer, regions ...string) string {
	return valkeyConfig(f, valkeyGroup("cache", 2, "8Gi", "")+standbyBlock("standby", regions...))
}

func regionSet(t *testing.T, d *serverv1.Distribution) []string {
	t.Helper()
	var out []string
	for cloud, c := range d.GetClouds() {
		for region, p := range c.GetRegions() {
			out = append(out, fmt.Sprintf("%s/%s=%t", cloud, region, p.GetPrimary()))
		}
	}
	slices.Sort(out)
	return out
}

func TestValkeyClusterResource_StandbyDistribution(t *testing.T) {
	f := startFakeServer(t)
	east, eu := standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)
	movedEast, movedEu := "    region {\n      cloud  = \"aws\"\n      region = \"us-east-1\"\n    }\n", standbyRegion("aws", "eu-west-1", true)
	const addr = "relay_valkey_cluster.this"
	wantRegions := func(want ...string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, db := range f.databases {
				if got := regionSet(t, db.GetSpec().GetDistribution()); !slices.Equal(got, want) {
					return fmt.Errorf("stored regions %v, want %v", got, want)
				}
			}
			return nil
		}
	}
	wantMask := func(paths ...string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := f.lastDBUpdate.GetUpdateMask().GetPaths(); !slices.Equal(got, paths) {
				return fmt.Errorf("update mask %v, want %v", got, paths)
			}
			return nil
		}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(relay.Dial),
		Steps: []resource.TestStep{
			{
				Config: standbyConfig(f, east, eu),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "distribution.mode", "standby"),
					resource.TestCheckResourceAttr(addr, "distribution.region.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "distribution.region.*", map[string]string{"cloud": "aws", "region": "us-east-1", "primary": "true"}),
					resource.TestCheckTypeSetElemNestedAttrs(addr, "distribution.region.*", map[string]string{"cloud": "aws", "region": "eu-west-1", "primary": "false"}),
					func(*terraform.State) error {
						d := f.lastDBCreate.GetSpec().GetDistribution()
						if d.GetMode() != "standby" {
							return fmt.Errorf("mode = %q", d.GetMode())
						}
						return nil
					},
					wantRegions("aws/eu-west-1=false", "aws/us-east-1=true"),
				),
			},
			{Config: standbyConfig(f, eu, east), PlanOnly: true},
			{Config: standbyConfig(f, "    region {\n      cloud  = \"aws\"\n      region = \"eu-west-1\"\n    }\n", east), PlanOnly: true},
			{
				Config: standbyConfig(f, movedEast, movedEu),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckTypeSetElemNestedAttrs(addr, "distribution.region.*", map[string]string{"region": "eu-west-1", "primary": "true"}),
					wantMask("distribution"),
					wantRegions("aws/eu-west-1=true", "aws/us-east-1=false"),
				),
			},
			{
				PreConfig: func() { f.unsyncedStandby = true },
				Config:    standbyConfig(f, east, eu),
				ExpectError: regexp.MustCompile(`(?s)Cannot promote the standby region.*cannot be promoted yet.*` +
					`does\s+not\s+force\s+promotions.*API,\s+CLI\s+or\s+dashboard`),
			},
			{
				Config:      standbyConfig(f, east, eu, standbyRegion("aws", "sa-east-1", false)),
				ExpectError: regexp.MustCompile(`(?s)Cannot update cluster.*cannot be promoted yet`),
			},
			{
				PreConfig: func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					for _, db := range f.databases {
						db.Spec.Distribution = &serverv1.Distribution{Mode: "standby", Clouds: map[string]*serverv1.CloudRegions{
							"aws": {Regions: map[string]*serverv1.RegionPlacement{"us-east-1": {Primary: true}, "eu-west-1": {}}},
						}}
					}
				},
				Config:             standbyConfig(f, movedEast, movedEu),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { f.unsyncedStandby = false },
				Config:    standbyConfig(f, movedEast, movedEu),
				Check:     wantRegions("aws/eu-west-1=true", "aws/us-east-1=false"),
			},
			{
				Config: standbyConfig(f, movedEast, movedEu, standbyRegion("aws", "sa-east-1", false)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "distribution.region.#", "3"),
					wantMask("distribution"),
				),
			},
			{
				Config:      valkeyConfig(f, valkeyGroup("cache", 2, "8Gi", "")),
				ExpectError: regexp.MustCompile(`changing the distribution mode`),
			},
		},
	})
}

func TestValkeyClusterResource_DistributionConfigValidation(t *testing.T) {
	f := startFakeServer(t)
	east, eu := standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)
	cases := map[string]struct {
		body string
		want string
	}{
		"one region":   {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby", east), "Too few regions"},
		"no primary":   {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby", standbyRegion("aws", "us-east-1", false), eu), "Exactly one primary region"},
		"two primary":  {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby", east, standbyRegion("aws", "eu-west-1", true)), "Exactly one primary region"},
		"duplicate":    {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby", east, eu, standbyRegion("aws", "us-east-1", false)), "Duplicate region"},
		"stretched":    {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("stretched", east, eu), "Stretched distribution is not available"},
		"unknown mode": {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("mirror", east, eu), "Unsupported distribution mode"},
		"node outside": {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby", standbyRegion("aws", "eu-west-1", true), standbyRegion("gcp", "us-central1", false)),
			`(?s)Node group outside the distribution.*cache runs in aws/us-east-1.*aws/eu-west-1, gcp/us-central1`},
		"cluster mode": {"  mode = \"cluster\"\n  shards = 3\n" + valkeyGroup("cache", 3, "1Gi", "") + standbyBlock("standby", east, eu), "Standby needs replication mode"},
		"restore":      {valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby", east, eu) + "  restore = { source_database_id = \"db-src\" }\n", "No restore into a standby"},
		"no mode":      {valkeyGroup("cache", 1, "1Gi", "") + "\n  distribution {\n" + east + eu + "  }\n", "Missing mode"},
		"no cloud":     {valkeyGroup("cache", 1, "1Gi", "") + "\n  distribution {\n    mode = \"standby\"\n    region {\n      region = \"us-east-1\"\n    }\n  }\n", `"cloud" is required|Missing Configuration for Required Attribute`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps:                    []resource.TestStep{{Config: valkeyConfig(f, tc.body), PlanOnly: true, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestValkeyClusterResource_DistributionUnknownValuesPlan(t *testing.T) {
	f := startFakeServer(t)
	later := "resource \"terraform_data\" \"later\" {\n  input = \"y\"\n}\n"
	cases := map[string]string{
		"region": valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby",
			standbyRegion("aws", "us-east-1", true),
			"    region {\n      cloud   = \"aws\"\n      region  = terraform_data.later.output\n    }\n"),
		"primary": valkeyGroup("cache", 1, "1Gi", "") + standbyBlock("standby",
			"    region {\n      cloud   = \"aws\"\n      region  = \"us-east-1\"\n      primary = terraform_data.later.output == \"x\"\n    }\n",
			standbyRegion("aws", "eu-west-1", false)),
		"node region": `
  nodes = {
    cache = {
      cloud     = "aws"
      region    = terraform_data.later.output
      tier      = "RC10"
      resources = {}
    }
  }
` + standbyBlock("standby", standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)),
		"mode": "  mode = terraform_data.later.output == \"x\" ? \"cluster\" : \"replication\"\n" + valkeyGroup("cache", 1, "1Gi", "") +
			standbyBlock("standby", standbyRegion("aws", "us-east-1", true), standbyRegion("aws", "eu-west-1", false)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: factories(relay.Dial),
				Steps: []resource.TestStep{{
					Config:             later + valkeyConfig(f, body),
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				}},
			})
		})
	}
}

func TestFakeServerRejectsWhatThePlatformRejectsForValkeyDistribution(t *testing.T) {
	region := func(primary bool) *serverv1.RegionPlacement { return &serverv1.RegionPlacement{Primary: primary} }
	dist := func(mode string, regions map[string]*serverv1.RegionPlacement) *serverv1.Distribution {
		return &serverv1.Distribution{Mode: mode, Clouds: map[string]*serverv1.CloudRegions{"aws": {Regions: regions}}}
	}
	two := map[string]*serverv1.RegionPlacement{"us-east-1": region(true), "eu-west-1": region(false)}
	spec := func(mode string, d *serverv1.Distribution) *serverv1.DatabaseSpec {
		return &serverv1.DatabaseSpec{
			Engine: "valkey", PrimaryNodeGroup: "cache", Distribution: d, Valkey: &serverv1.ValkeyConfig{Mode: mode},
			Nodes: []*serverv1.NodeSpec{{Name: "cache", Role: "electable", Count: 1, Cloud: "aws", Region: "us-east-1"}},
		}
	}
	reject := map[string]*serverv1.DatabaseSpec{
		"stretched":    spec("replication", dist("stretched", two)),
		"cluster mode": spec("cluster", dist("standby", two)),
		"one region":   spec("replication", dist("standby", map[string]*serverv1.RegionPlacement{"us-east-1": region(true)})),
		"no primary":   spec("replication", dist("standby", map[string]*serverv1.RegionPlacement{"us-east-1": region(false), "eu-west-1": region(false)})),
		"node outside": spec("replication", dist("standby", map[string]*serverv1.RegionPlacement{"eu-west-1": region(true), "ap-south-1": region(false)})),
	}
	for name, s := range reject {
		t.Run(name, func(t *testing.T) {
			if err := validateValkeyDistribution(s); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
		})
	}
	for name, d := range map[string]*serverv1.Distribution{"none": nil, "local": dist("local", nil), "standby": dist("standby", two)} {
		if err := validateValkeyDistribution(spec("replication", d)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := primaryRegionOf(nil); got != "" {
		t.Fatalf("no distribution has no primary, got %q", got)
	}
}
