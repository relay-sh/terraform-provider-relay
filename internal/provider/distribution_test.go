package provider

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func regionObject(cloud, region types.String, primary types.Bool) attr.Value {
	return types.ObjectValueMust(distributionRegionType.AttrTypes, map[string]attr.Value{"cloud": cloud, "region": region, "primary": primary})
}

func distributionObject(mode string, regions ...attr.Value) types.Object {
	return types.ObjectValueMust(distributionAttrTypes, map[string]attr.Value{
		"mode":   types.StringValue(mode),
		"region": types.SetValueMust(distributionRegionType, regions),
	})
}

func TestDistributionConversions(t *testing.T) {
	s := types.StringValue
	d := distributionObject("standby",
		regionObject(s("aws"), s("us-east-1"), types.BoolValue(true)),
		regionObject(s("gcp"), s("us-central1"), types.BoolValue(false)))

	proto := distributionToProto(d)
	if proto.GetMode() != "standby" || !proto.GetClouds()["aws"].GetRegions()["us-east-1"].GetPrimary() || proto.GetClouds()["gcp"].GetRegions()["us-central1"].GetPrimary() {
		t.Fatalf("unexpected proto %v", proto)
	}
	if back := distributionFromProto(proto, d); !back.Equal(d) {
		t.Fatalf("round trip lost the distribution: %v", back)
	}
	if got := distributionPrimary(d); got != "aws/us-east-1" {
		t.Fatalf("primary = %q", got)
	}
	if got := distributionPrimary(types.ObjectNull(distributionAttrTypes)); got != "" {
		t.Fatalf("a missing distribution has no primary, got %q", got)
	}
	for name, in := range map[string]*serverv1.Distribution{
		"nil":       nil,
		"local":     {Mode: "local"},
		"stretched": {Mode: "stretched", Clouds: map[string]*serverv1.CloudRegions{"aws": {Regions: map[string]*serverv1.RegionPlacement{"us-east-1": {Primary: true}, "eu-west-1": {}}}}},
	} {
		if got := distributionFromProto(in, types.ObjectNull(distributionAttrTypes)); !got.IsNull() {
			t.Fatalf("%s must read as no distribution, got %v", name, got)
		}
	}
	for name, in := range map[string]types.Object{
		"null":    types.ObjectNull(distributionAttrTypes),
		"unknown": types.ObjectUnknown(distributionAttrTypes),
		"unknown regions": types.ObjectValueMust(distributionAttrTypes, map[string]attr.Value{
			"mode": s("standby"), "region": types.SetUnknown(distributionRegionType),
		}),
	} {
		if got := distributionToProto(in); got != nil {
			t.Fatalf("%s must send no distribution, got %v", name, got)
		}
	}
}

func TestDistributionFromProtoKeepsTheConfiguredSpelling(t *testing.T) {
	s, yes, no := types.StringValue, types.BoolValue(true), types.BoolValue(false)
	server := &serverv1.Distribution{Mode: "standby", Clouds: map[string]*serverv1.CloudRegions{
		"aws": {Regions: map[string]*serverv1.RegionPlacement{"us-east-1": {Primary: true}, "eu-west-1": {}}},
	}}
	configured := distributionObject("standby",
		regionObject(s("AWS"), s("US-East-1"), yes),
		regionObject(types.StringUnknown(), s("eu-west-1"), no))
	want := distributionObject("standby",
		regionObject(s("AWS"), s("US-East-1"), yes),
		regionObject(s("aws"), s("eu-west-1"), no))
	if got := distributionFromProto(server, configured); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	canonical := distributionObject("standby",
		regionObject(s("aws"), s("us-east-1"), yes),
		regionObject(s("aws"), s("eu-west-1"), no))
	if got := distributionFromProto(server, types.ObjectNull(distributionAttrTypes)); !got.Equal(canonical) {
		t.Fatalf("with nothing configured the server spelling stands, got %v", got)
	}
}

func TestKeepStoredPlacementsIgnoresSpelling(t *testing.T) {
	members := int32(2)
	stored := &serverv1.Distribution{Clouds: map[string]*serverv1.CloudRegions{
		"aws": {Regions: map[string]*serverv1.RegionPlacement{"eu-west-1": {Members: &members}}},
	}}
	next := &serverv1.Distribution{Clouds: map[string]*serverv1.CloudRegions{
		"AWS": {Regions: map[string]*serverv1.RegionPlacement{"EU-West-1": {}}},
	}}
	keepStoredPlacements(next, stored)
	if got := next.GetClouds()["AWS"].GetRegions()["EU-West-1"].GetMembers(); got != 2 {
		t.Fatalf("members = %d, want the stored 2", got)
	}
}

func TestKeepStoredPlacements(t *testing.T) {
	members, priority := int32(2), int32(3)
	region := func(p *serverv1.RegionPlacement) *serverv1.CloudRegions {
		return &serverv1.CloudRegions{Regions: map[string]*serverv1.RegionPlacement{"us-east-1": p}}
	}
	stored := &serverv1.Distribution{Mode: "standby", Clouds: map[string]*serverv1.CloudRegions{
		"aws": region(&serverv1.RegionPlacement{Primary: true, Members: &members, Priority: &priority}),
	}}
	next := &serverv1.Distribution{Mode: "standby", Clouds: map[string]*serverv1.CloudRegions{
		"aws": {Regions: map[string]*serverv1.RegionPlacement{"us-east-1": {Primary: true}, "eu-west-1": {}}},
		"gcp": region(&serverv1.RegionPlacement{}),
	}}
	keepStoredPlacements(next, stored)
	kept := next.GetClouds()["aws"].GetRegions()["us-east-1"]
	if kept.GetMembers() != 2 || kept.GetPriority() != 3 || !kept.GetPrimary() {
		t.Errorf("a kept region must carry its stored members and priority, got %v", kept)
	}
	for _, other := range []*serverv1.RegionPlacement{next.GetClouds()["aws"].GetRegions()["eu-west-1"], next.GetClouds()["gcp"].GetRegions()["us-east-1"]} {
		if other.Members != nil || other.Priority != nil {
			t.Errorf("a new region has nothing stored to carry, got %v", other)
		}
	}
	keepStoredPlacements(next, nil)
	keepStoredPlacements(nil, stored)
}

func TestPromotesStandby(t *testing.T) {
	s, yes, no := types.StringValue, types.BoolValue(true), types.BoolValue(false)
	east := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), yes), regionObject(s("aws"), s("eu-west-1"), no))
	eu := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), no), regionObject(s("aws"), s("eu-west-1"), yes))
	null := types.ObjectNull(distributionAttrTypes)
	for name, tc := range map[string]struct {
		plan, prior types.Object
		want        bool
	}{
		"marker moves":  {eu, east, true},
		"unchanged":     {east, east, false},
		"added":         {east, null, false},
		"with a region": {distributionObject("standby", regionObject(s("aws"), s("us-east-1"), no), regionObject(s("aws"), s("eu-west-1"), yes), regionObject(s("aws"), s("sa-east-1"), no)), east, false},
		"removed":       {null, east, false},
	} {
		if got := promotesStandby(tc.plan, tc.prior); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestClusterUpdateError(t *testing.T) {
	refused := status.Error(codes.FailedPrecondition, "cannot be promoted yet")
	cases := map[string]struct {
		err       error
		promoting bool
		summary   string
		forced    bool
	}{
		"refused promotion":     {refused, true, "Cannot promote the standby region", true},
		"refused other update":  {refused, false, "Cannot update cluster", false},
		"other promotion error": {status.Error(codes.Internal, "boom"), true, "Cannot update cluster", false},
		"plain error":           {errors.New("boom"), true, "Cannot update cluster", false},
	}
	for name, tc := range cases {
		summary, detail := clusterUpdateError(tc.err, tc.promoting)
		if summary != tc.summary || strings.Contains(detail, "API, CLI or dashboard") != tc.forced {
			t.Errorf("%s: got %q / %q", name, summary, detail)
		}
	}
}

func TestDistributionModeValidator(t *testing.T) {
	v := distributionModeValidator{stretched: "use standby"}
	if v.Description(t.Context()) == "" || v.MarkdownDescription(t.Context()) != v.Description(t.Context()) {
		t.Error("the validator must describe itself")
	}
	for name, tc := range map[string]struct {
		value  types.String
		detail string
	}{
		"standby":   {types.StringValue("standby"), ""},
		"null":      {types.StringNull(), ""},
		"unknown":   {types.StringUnknown(), ""},
		"stretched": {types.StringValue("stretched"), "use standby"},
		"other":     {types.StringValue("mirror"), `got "mirror"`},
	} {
		resp := &validator.StringResponse{}
		v.ValidateString(t.Context(), validator.StringRequest{Path: path.Root("distribution").AtName("mode"), ConfigValue: tc.value}, resp)
		if tc.detail == "" && resp.Diagnostics.HasError() || tc.detail != "" && (!resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), tc.detail)) {
			t.Errorf("%s: %v", name, resp.Diagnostics)
		}
	}
}

func TestValidateDistributionSkipsWhatIsNotKnown(t *testing.T) {
	noRestore := types.ObjectNull(map[string]attr.Type{})
	s := types.StringValue
	node := func(cloud, region types.String) types.Map {
		return types.MapValueMust(types.ObjectType{AttrTypes: map[string]attr.Type{"cloud": types.StringType, "region": types.StringType}}, map[string]attr.Value{
			"cache": types.ObjectValueMust(map[string]attr.Type{"cloud": types.StringType, "region": types.StringType}, map[string]attr.Value{"cloud": cloud, "region": region}),
		})
	}
	two := distributionObject("standby", regionObject(s("aws"), s("us-east-1"), types.BoolValue(true)), regionObject(s("aws"), s("eu-west-1"), types.BoolValue(false)))
	for name, tc := range map[string]struct {
		d     types.Object
		mode  types.String
		nodes types.Map
	}{
		"no distribution":   {types.ObjectNull(distributionAttrTypes), s("replication"), types.MapNull(types.StringType)},
		"unknown":           {types.ObjectUnknown(distributionAttrTypes), s("replication"), types.MapNull(types.StringType)},
		"unknown regions":   {types.ObjectValueMust(distributionAttrTypes, map[string]attr.Value{"mode": s("standby"), "region": types.SetUnknown(distributionRegionType)}), s("replication"), types.MapNull(types.StringType)},
		"unknown nodes":     {two, s("replication"), types.MapUnknown(types.StringType)},
		"no nodes":          {two, s("replication"), types.MapNull(types.StringType)},
		"unknown node cell": {two, s("replication"), node(types.StringUnknown(), s("us-east-1"))},
		"unknown node reg":  {two, s("replication"), node(s("aws"), types.StringUnknown())},
		"unknown mode":      {two, types.StringUnknown(), node(s("aws"), s("us-east-1"))},
		"null mode":         {two, types.StringNull(), node(s("aws"), s("us-east-1"))},
	} {
		var diags diag.Diagnostics
		validateValkeyDistributionConfig(tc.d, tc.mode, tc.nodes, noRestore, &diags)
		if diags.HasError() {
			t.Errorf("%s: %v", name, diags)
		}
		diags = nil
		validatePostgreSQLDistributionConfig(tc.d, tc.nodes, noRestore, &diags)
		if diags.HasError() {
			t.Errorf("postgresql %s: %v", name, diags)
		}
	}
	var diags diag.Diagnostics
	validateValkeyDistributionConfig(two, s("replication"), node(s("aws"), s("ap-south-1")), noRestore, &diags)
	if !diags.HasError() {
		t.Fatal("a node group outside the regions must be rejected")
	}
	diags = nil
	validatePostgreSQLDistributionConfig(two, node(s("aws"), s("ap-south-1")), noRestore, &diags)
	if !diags.HasError() {
		t.Fatal("a node group outside the regions must be rejected for PostgreSQL too")
	}
}
