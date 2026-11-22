package provider

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

const (
	distributionStandby = "standby"
	distributionLocal   = "local"
)

var distributionRegionType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"cloud":   types.StringType,
	"region":  types.StringType,
	"primary": types.BoolType,
}}

var distributionAttrTypes = map[string]attr.Type{
	"mode":   types.StringType,
	"region": types.SetType{ElemType: distributionRegionType},
}

type distributionRegionModel struct {
	Cloud   types.String `tfsdk:"cloud"`
	Region  types.String `tfsdk:"region"`
	Primary types.Bool   `tfsdk:"primary"`
}

func distributionBlock() schema.Block {
	return schema.SingleNestedBlock{
		MarkdownDescription: "Spreads the cluster across regions as a standby: one region serves reads and writes and the others replicate from it. " +
			"Only with `mode = \"replication\"`. Add the block when you create the cluster: moving a cluster between a single region and a standby spread is refused by the platform. " +
			"Move the primary by setting `primary = true` on another region and `false` on the current one, in an update that changes nothing else. " +
			"Add or remove one region per update. The platform refuses a promotion while the new primary has not synchronized; " +
			"forcing one can lose data and is not available from Terraform, so use the API, CLI or dashboard.",
		Attributes: map[string]schema.Attribute{
			"mode": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{stringvalidator.OneOf(distributionStandby)},
				MarkdownDescription: "Only `standby` is supported. Required when the block is present.",
			},
		},
		Blocks: map[string]schema.Block{
			"region": schema.SetNestedBlock{
				MarkdownDescription: "A region the cluster runs in; at least two, exactly one with `primary = true`. The node group's `cloud` and `region` must be one of them.",
				NestedObject: schema.NestedBlockObject{Attributes: map[string]schema.Attribute{
					"cloud": schema.StringAttribute{Required: true, MarkdownDescription: "Cloud of the region: `aws`, `gcp` or `azure`."},
					"region": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Region name, for example `us-east-1`.",
					},
					"primary": schema.BoolAttribute{
						Optional: true, Computed: true, Default: booldefault.StaticBool(false),
						MarkdownDescription: "Whether this region holds the primary. Exactly one region does.",
					},
				}},
			},
		},
	}
}

func distributionRegions(d types.Object) ([]distributionRegionModel, bool) {
	if !known(d) {
		return nil, false
	}
	set, ok := d.Attributes()["region"].(types.Set)
	if !ok || !known(set) {
		return nil, false
	}
	out := make([]distributionRegionModel, 0, len(set.Elements()))
	for _, element := range set.Elements() {
		attrs := element.(types.Object).Attributes()
		cloud, _ := attrs["cloud"].(types.String)
		region, _ := attrs["region"].(types.String)
		primary, _ := attrs["primary"].(types.Bool)
		out = append(out, distributionRegionModel{Cloud: cloud, Region: region, Primary: primary})
	}
	return out, true
}

func distributionPrimary(d types.Object) string {
	regions, _ := distributionRegions(d)
	for _, r := range regions {
		if r.Primary.ValueBool() {
			return r.Cloud.ValueString() + "/" + r.Region.ValueString()
		}
	}
	return ""
}

func distributionToProto(d types.Object) *serverv1.Distribution {
	regions, ok := distributionRegions(d)
	if !ok {
		return nil
	}
	mode, _ := d.Attributes()["mode"].(types.String)
	out := &serverv1.Distribution{Mode: mode.ValueString(), Clouds: map[string]*serverv1.CloudRegions{}}
	for _, r := range regions {
		cloud := r.Cloud.ValueString()
		if out.Clouds[cloud] == nil {
			out.Clouds[cloud] = &serverv1.CloudRegions{Regions: map[string]*serverv1.RegionPlacement{}}
		}
		out.Clouds[cloud].Regions[r.Region.ValueString()] = &serverv1.RegionPlacement{Primary: r.Primary.ValueBool()}
	}
	return out
}

func distributionFromProto(d *serverv1.Distribution) types.Object {
	if d.GetMode() == "" || d.GetMode() == distributionLocal {
		return types.ObjectNull(distributionAttrTypes)
	}
	var elements []attr.Value
	for cloud, regions := range d.GetClouds() {
		for region, placement := range regions.GetRegions() {
			elements = append(elements, types.ObjectValueMust(distributionRegionType.AttrTypes, map[string]attr.Value{
				"cloud":   types.StringValue(cloud),
				"region":  types.StringValue(region),
				"primary": types.BoolValue(placement.GetPrimary()),
			}))
		}
	}
	return types.ObjectValueMust(distributionAttrTypes, map[string]attr.Value{
		"mode":   types.StringValue(d.GetMode()),
		"region": types.SetValueMust(distributionRegionType, elements),
	})
}

func validateDistribution(d types.Object, mode types.String, nodes types.Map, restore types.Object, diags *diag.Diagnostics) {
	if !known(d) {
		return
	}
	root := path.Root("distribution")
	if distMode, _ := d.Attributes()["mode"].(types.String); distMode.IsNull() {
		diags.AddAttributeError(root.AtName("mode"), "Missing mode", "A distribution block needs mode = \"standby\".")
	}
	if mode.ValueString() == valkeyModeCluster {
		diags.AddAttributeError(root, "Standby needs replication mode",
			"A standby distribution replicates one primary to the other regions, which only mode \"replication\" does. Remove the distribution block or use mode = \"replication\".")
	}
	if !restore.IsNull() {
		diags.AddAttributeError(root, "No restore into a standby",
			"A backup cannot be restored into a standby distribution. Remove the distribution block to restore the backup into a single-region cluster.")
	}
	regions, ok := distributionRegions(d)
	if !ok {
		return
	}
	complete := true
	seen := map[string]bool{}
	primaries := 0
	for _, r := range regions {
		if r.Cloud.IsUnknown() || r.Region.IsUnknown() || r.Primary.IsUnknown() {
			complete = false
			continue
		}
		key := r.Cloud.ValueString() + "/" + r.Region.ValueString()
		if seen[key] {
			diags.AddAttributeError(root.AtName("region"), "Duplicate region", "Region "+key+" appears more than once; list each region once.")
		}
		seen[key] = true
		if r.Primary.ValueBool() {
			primaries++
		}
	}
	if !complete {
		return
	}
	if len(regions) < 2 {
		diags.AddAttributeError(root.AtName("region"), "Too few regions",
			fmt.Sprintf("A standby distribution needs at least 2 regions, got %d.", len(regions)))
	}
	if primaries != 1 {
		diags.AddAttributeError(root.AtName("region"), "Exactly one primary region",
			fmt.Sprintf("Exactly one region must set primary = true, got %d.", primaries))
	}
	if nodes.IsNull() || nodes.IsUnknown() {
		return
	}
	for label, element := range nodes.Elements() {
		attrs := element.(types.Object).Attributes()
		cloud, _ := attrs["cloud"].(types.String)
		region, _ := attrs["region"].(types.String)
		if !known(cloud) || !known(region) {
			continue
		}
		if key := cloud.ValueString() + "/" + region.ValueString(); !seen[key] {
			diags.AddAttributeError(path.Root("nodes").AtMapKey(label), "Node group outside the distribution",
				fmt.Sprintf("Node group %s runs in %s, which is not one of the distribution's regions (%s).", label, key, strings.Join(sortedKeys(seen), ", ")))
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func clusterUpdateError(err error, promoting bool) (string, string) {
	if promoting && status.Code(err) == codes.FailedPrecondition {
		return "Cannot promote the standby region", err.Error() +
			"\n\nTerraform does not force promotions: a forced promotion can lose whatever the new primary has not received. " +
			"If you accept that, force it through the API, CLI or dashboard, then set primary in the configuration to match."
	}
	return "Cannot update cluster", err.Error()
}

func promotesStandby(plan, prior types.Object) bool {
	from, to := distributionPrimary(prior), distributionPrimary(plan)
	return from != "" && to != "" && from != to && slices.Equal(distributionKeys(plan), distributionKeys(prior))
}

func distributionKeys(d types.Object) []string {
	regions, _ := distributionRegions(d)
	keys := make([]string, 0, len(regions))
	for _, r := range regions {
		keys = append(keys, r.Cloud.ValueString()+"/"+r.Region.ValueString())
	}
	slices.Sort(keys)
	return keys
}
