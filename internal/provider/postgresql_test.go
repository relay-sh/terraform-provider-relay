package provider

import (
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

var testCatalog = []*serverv1.DatabaseProvider{
	{Name: "aws-use1", Cloud: "aws", Region: "us-east-1", Engines: []string{"postgresql"}},
	{Name: "mongo-euw1", Cloud: "aws", Region: "eu-west-1", Engines: []string{"mongodb"}},
	{Name: "kubernetes"},
}

func TestProvisionerFor(t *testing.T) {
	if got, err := provisionerFor(testCatalog, "", ""); got != "" || err != nil {
		t.Errorf("an unplaced group must leave the choice to the platform, got %q %v", got, err)
	}
	if got, err := provisionerFor(testCatalog, "aws", "us-east-1"); got != "aws-use1" || err != nil {
		t.Errorf("provisioner = %q, %v", got, err)
	}
	if got, err := provisionerFor(testCatalog, "", "us-east-1"); got != "aws-use1" || err != nil {
		t.Errorf("a region alone must be enough, got %q %v", got, err)
	}
	_, err := provisionerFor(testCatalog, "aws", "eu-west-1")
	if err == nil {
		t.Fatal("a region that only serves MongoDB must not be accepted")
	}
	if want := `no PostgreSQL provisioner serves cloud "aws" in region "eu-west-1"; the platform offers [aws/us-east-1 /]`; err.Error() != want {
		t.Errorf("error = %q", err)
	}
}

func TestPlacementOf(t *testing.T) {
	if cloud, region := placementOf(testCatalog, &serverv1.NodeSpec{Provider: "aws-use1"}); cloud != "aws" || region != "us-east-1" {
		t.Errorf("placement = %q/%q", cloud, region)
	}
	if cloud, region := placementOf(testCatalog, &serverv1.NodeSpec{Provider: "gone", Region: "us-west-2"}); cloud != "" || region != "us-west-2" {
		t.Errorf("a provisioner missing from the catalog must fall back to the node's own region, got %q/%q", cloud, region)
	}
}

func TestNodesRoundTrip(t *testing.T) {
	nodes := map[string]nodeGroupModel{
		"primary": {
			Count: types.Int64Value(2), Cloud: types.StringValue("aws"), Region: types.StringValue("us-east-1"),
			Tier: types.StringValue("R10"),
			Resources: resourcesModel{
				CPU: types.StringValue("1"), Memory: types.StringValue("1Gi"), Storage: types.StringValue("1Gi"),
				StorageClass: types.StringUnknown(), IOPS: types.Int64Unknown(), Throughput: types.Int64Value(250),
			},
			Synchronous: types.BoolValue(true), Failover: types.BoolValue(true),
		},
		"analytics": {
			Count: types.Int64Value(1), Cloud: types.StringNull(), Region: types.StringNull(),
			Tier: types.StringValue("R20"),
			Resources: resourcesModel{
				CPU: types.StringValue("2"), Memory: types.StringValue("4Gi"), Storage: types.StringValue("9Gi"),
				StorageClass: types.StringValue("gp3"), IOPS: types.Int64Value(3000), Throughput: types.Int64Null(),
			},
			Synchronous: types.BoolValue(false), Failover: types.BoolValue(false),
		},
	}
	out, err := nodesToProto(nodes, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].GetRole() != "analytics" || out[1].GetRole() != "primary" {
		t.Fatalf("groups must be sent in a stable order, got %q then %q", out[0].GetRole(), out[1].GetRole())
	}
	if tags := out[0].GetTags(); tags["nosync"] != "true" || tags["nofailover"] != "true" {
		t.Errorf("tags = %v", tags)
	}
	if out[1].GetTags() != nil {
		t.Errorf("a group that stays in the commit path and can be promoted carries no tags, got %v", out[1].GetTags())
	}
	if out[1].GetProvider() != "aws-use1" || out[1].GetRegion() != "us-east-1" {
		t.Errorf("placement = %q/%q", out[1].GetProvider(), out[1].GetRegion())
	}
	res := out[1].GetResources()
	if res.GetCpu() != "" || res.GetMemory() != "" {
		t.Errorf("cpu/memory must not be sent, got %q/%q", res.GetCpu(), res.GetMemory())
	}
	if out[0].GetTier() != "R20" || out[1].GetTier() != "R10" {
		t.Errorf("tiers = %q, %q", out[0].GetTier(), out[1].GetTier())
	}
	if res.Iops != nil || res.GetThroughput() != 250 {
		t.Errorf("unknown limits must be omitted and known ones sent: %+v", res)
	}

	back := nodesFromProto(out, testCatalog)
	if g := back["analytics"]; g.Synchronous.ValueBool() || g.Failover.ValueBool() || g.Resources.IOPS.ValueInt64() != 3000 || g.Tier.ValueString() != "R20" {
		t.Errorf("analytics round trip = %+v", g)
	}
	if g := back["primary"]; !g.Synchronous.ValueBool() || !g.Failover.ValueBool() || !g.Resources.IOPS.IsNull() || g.Cloud.ValueString() != "aws" || g.Tier.ValueString() != "R10" {
		t.Errorf("primary round trip = %+v", g)
	}
}

func TestNodesToProtoSendsUnknownTierUnchanged(t *testing.T) {
	out, err := nodesToProto(map[string]nodeGroupModel{"primary": {
		Count: types.Int64Value(1), Cloud: types.StringValue("aws"), Region: types.StringValue("us-east-1"),
		Tier:        types.StringValue("R99"),
		Resources:   resourcesModel{Storage: types.StringValue("1Gi")},
		Synchronous: types.BoolValue(true), Failover: types.BoolValue(true),
	}}, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].GetTier() != "R99" {
		t.Fatalf("unknown names must be sent unchanged, got %q", out[0].GetTier())
	}
}

func TestNodesToProtoRejectsAnUnservedRegion(t *testing.T) {
	_, err := nodesToProto(map[string]nodeGroupModel{"primary": {
		Cloud: types.StringValue("fly"), Region: types.StringValue("gru"),
		Resources:   resourcesModel{},
		Synchronous: types.BoolValue(true), Failover: types.BoolValue(true),
	}}, testCatalog)
	if err == nil {
		t.Fatal("an unserved placement must fail before the cluster is created")
	}
}

func TestReplicationConversions(t *testing.T) {
	if replicationToProto(types.ObjectNull(replicationAttrTypes)) != nil || replicationToProto(types.ObjectUnknown(replicationAttrTypes)) != nil {
		t.Error("null or unknown replication must not be sent")
	}
	async := types.ObjectValueMust(replicationAttrTypes, map[string]attr.Value{"mode": types.StringValue("async"), "synchronous_replicas": types.Int64Null()})
	if got := replicationToProto(async); got.GetMode() != "async" || got.SynchronousReplicas != nil {
		t.Errorf("async replication = %+v", got)
	}
	sync := types.ObjectValueMust(replicationAttrTypes, map[string]attr.Value{"mode": types.StringValue("synchronous"), "synchronous_replicas": types.Int64Value(2)})
	if got := replicationToProto(sync); got.GetSynchronousReplicas() != 2 {
		t.Errorf("synchronous replication = %+v", got)
	}
	if !replicationFromProto(nil).IsNull() {
		t.Error("absent replication must read as null")
	}
	if got := replicationFromProto(&serverv1.Replication{Mode: "async"}); got.Attributes()["mode"].(types.String).ValueString() != "async" {
		t.Errorf("replication = %v", got)
	}
}

func TestStringMapToGo(t *testing.T) {
	if stringMapToGo(types.MapNull(types.StringType)) != nil || stringMapToGo(types.MapUnknown(types.StringType)) != nil {
		t.Error("null or unknown maps must not be sent")
	}
}

func TestTimestampValue(t *testing.T) {
	if got := timestampValue(nil); got.ValueString() != "" {
		t.Errorf("a missing timestamp reads as empty, got %q", got.ValueString())
	}
	if got := timestampValue(timestamppb.New(time.Time{})); got.ValueString() != "" {
		t.Errorf("a zero timestamp reads as empty, got %q", got.ValueString())
	}
	if got := timestampValue(timestamppb.New(time.Unix(1_700_000_000, 0))); got.ValueString() != "2023-11-14T22:13:20Z" {
		t.Errorf("timestamp = %q", got.ValueString())
	}
}

func TestParseQuantity(t *testing.T) {
	cases := map[string]struct {
		want float64
		ok   bool
	}{
		"100":    {100, true},
		"1.5":    {1.5, true},
		"500m":   {0.5, true},
		"10Gi":   {10 << 30, true},
		"2G":     {2e9, true},
		"5Zi":    {0, false},
		"Gi":     {0, false},
		"1.2.3G": {0, false},
	}
	for in, tc := range cases {
		got, ok := parseQuantity(in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseQuantity(%q) = %v, %v; want %v, %v", in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestShrinks(t *testing.T) {
	if shrinks(types.StringUnknown(), types.StringValue("10Gi")) || shrinks(types.StringValue("10Gi"), types.StringUnknown()) {
		t.Error("an unknown size cannot be compared")
	}
	if shrinks(types.StringValue("5Zi"), types.StringValue("10Gi")) {
		t.Error("a size the provider cannot parse must be left to the platform")
	}
	if !shrinks(types.StringValue("5Gi"), types.StringValue("10Gi")) {
		t.Error("5Gi is smaller than 10Gi")
	}
	if shrinks(types.StringValue("10240Mi"), types.StringValue("10Gi")) {
		t.Error("the same size in another unit is not a shrink")
	}
}
