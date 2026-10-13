package provider

import (
	"slices"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func TestNodesToProtoOrdersElectableFirst(t *testing.T) {
	group := func(role, tier string) nodeGroupModel {
		return nodeGroupModel{
			Role: types.StringValue(role), Count: types.Int64Value(1),
			Cloud: types.StringValue("aws"), Region: types.StringValue("us-east-1"), Tier: types.StringValue(tier),
			Resources: resourcesModel{
				Storage: types.StringValue("1Gi"), IOPS: types.Int64Null(),
				Throughput: types.Int64Null(), DiskType: types.StringNull(),
			},
		}
	}
	out := nodesToProto(map[string]nodeGroupModel{
		"analytics": group(roleReadOnly, "R10"),
		"dr":        group(roleStandby, "R20"),
		"secondary": group(roleElectable, "R30"),
		"primary":   group(roleElectable, "R40"),
	})
	var got []string
	for _, n := range out {
		got = append(got, n.GetRole()+"/"+n.GetTier())
	}
	want := []string{"electable/R40", "electable/R30", "standby/R20", "read_only/R10"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v (electable first, then the label)", got, want)
	}
}

func TestNodesRoundTrip(t *testing.T) {
	enabledAutoscale := types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(true), "threshold_percent": types.Int64Value(85),
		"increase_percent": types.Int64Value(25), "max_storage": types.StringValue("50Gi"),
	})
	nodes := map[string]nodeGroupModel{
		"writers": {
			Role:  types.StringValue(roleElectable),
			Count: types.Int64Value(2), Cloud: types.StringValue("aws"), Region: types.StringValue("us-east-1"),
			Tier: types.StringValue("R10"),
			Resources: resourcesModel{
				CPU: types.StringValue("1"), Memory: types.StringValue("1Gi"), Storage: types.StringValue("1Gi"),
				IOPS: types.Int64Unknown(), Throughput: types.Int64Value(250),
				DiskType: types.StringUnknown(), StorageAutoscale: types.ObjectNull(storageAutoscaleAttrTypes),
			},
		},
		"readers": {
			Role:  types.StringValue(roleReadOnly),
			Count: types.Int64Value(1), Cloud: types.StringValue("aws"), Region: types.StringValue("us-east-1"),
			Tier: types.StringValue("R20"),
			Resources: resourcesModel{
				CPU: types.StringValue("2"), Memory: types.StringValue("4Gi"), Storage: types.StringValue("9Gi"),
				IOPS: types.Int64Value(3000), Throughput: types.Int64Null(),
				DiskType: types.StringValue("io1"), StorageAutoscale: enabledAutoscale,
			},
		},
	}
	out := nodesToProto(nodes)
	if out[0].GetRole() != roleElectable || out[1].GetRole() != roleReadOnly {
		t.Fatalf("the electable group must lead whatever the labels sort to, got %q then %q", out[0].GetRole(), out[1].GetRole())
	}
	if out[0].GetCloud() != "aws" || out[0].GetRegion() != "us-east-1" || out[0].GetProvider() != "" {
		t.Errorf("placement = %q/%q provider %q, want aws/us-east-1 and no provider", out[0].GetCloud(), out[0].GetRegion(), out[0].GetProvider())
	}
	res := out[0].GetResources()
	if res.GetCpu() != "" || res.GetMemory() != "" {
		t.Errorf("cpu/memory must not be sent, got %q/%q", res.GetCpu(), res.GetMemory())
	}
	if out[0].GetTier() != "R10" || out[1].GetTier() != "R20" {
		t.Errorf("tiers = %q, %q", out[0].GetTier(), out[1].GetTier())
	}
	if res.Iops != nil || res.GetThroughput() != 250 {
		t.Errorf("unknown limits must be omitted and known ones sent: %+v", res)
	}
	if res.GetDiskType() != "" {
		t.Errorf("unknown disk_type must be omitted, got %q", res.GetDiskType())
	}
	if out[1].GetResources().GetDiskType() != "io1" {
		t.Errorf("readers disk_type = %q, want io1", out[1].GetResources().GetDiskType())
	}
	if sa := res.GetStorageAutoscale(); sa == nil || sa.GetEnabled() {
		t.Errorf("an absent block must still send an explicit disabled StorageAutoscale, got %+v", sa)
	}
	if sa := out[1].GetResources().GetStorageAutoscale(); !sa.GetEnabled() || sa.GetThresholdPercent() != 85 || sa.GetIncreasePercent() != 25 || sa.GetMaxStorage() != "50Gi" {
		t.Errorf("readers storage_autoscale = %+v", sa)
	}

	// The platform answers cloud+region on a node and never echoes the provisioner.
	for _, n := range out {
		n.Cloud, n.Provider = "aws", ""
	}
	back := nodesFromProto(out, nodes)
	if g := back["readers"]; g.Role.ValueString() != roleReadOnly || g.Resources.IOPS.ValueInt64() != 3000 || g.Tier.ValueString() != "R20" || g.Resources.DiskType.ValueString() != "io1" {
		t.Errorf("readers round trip = %+v", g)
	}
	if g := back["readers"]; g.Resources.StorageAutoscale.IsNull() {
		t.Error("an enabled block must round trip visible")
	} else if attrs := g.Resources.StorageAutoscale.Attributes(); attrs["threshold_percent"].(types.Int64).ValueInt64() != 85 {
		t.Errorf("readers storage_autoscale round trip = %+v", attrs)
	}
	if g := back["writers"]; g.Role.ValueString() != roleElectable || !g.Resources.IOPS.IsNull() || g.Cloud.ValueString() != "aws" || g.Tier.ValueString() != "R10" {
		t.Errorf("writers round trip = %+v", g)
	}
	if g := back["writers"]; !g.Resources.StorageAutoscale.IsNull() {
		t.Errorf("an absent block must read back null, got %+v", g.Resources.StorageAutoscale)
	}
}

func TestStorageAutoscaleConversions(t *testing.T) {
	if got := storageAutoscaleToProto(types.ObjectNull(storageAutoscaleAttrTypes)); got == nil || got.GetEnabled() {
		t.Errorf("a null block must still send an explicit disabled StorageAutoscale, got %+v", got)
	}
	if got := storageAutoscaleToProto(types.ObjectUnknown(storageAutoscaleAttrTypes)); got == nil || got.GetEnabled() {
		t.Errorf("an unknown block must not be sent as enabled, got %+v", got)
	}
	enabled := types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(true), "threshold_percent": types.Int64Value(85),
		"increase_percent": types.Int64Value(25), "max_storage": types.StringValue("50Gi"),
	})
	if got := storageAutoscaleToProto(enabled); !got.GetEnabled() || got.GetThresholdPercent() != 85 || got.GetIncreasePercent() != 25 || got.GetMaxStorage() != "50Gi" {
		t.Errorf("enabled block = %+v", got)
	}
	unknownFields := types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(true), "threshold_percent": types.Int64Unknown(),
		"increase_percent": types.Int64Unknown(), "max_storage": types.StringUnknown(),
	})
	if got := storageAutoscaleToProto(unknownFields); !got.GetEnabled() || got.ThresholdPercent != 0 || got.IncreasePercent != 0 || got.MaxStorage != "" {
		t.Errorf("unknown limits must be omitted: %+v", got)
	}

	if got := storageAutoscaleFromProto(nil, types.ObjectNull(storageAutoscaleAttrTypes)); !got.IsNull() {
		t.Error("no StorageAutoscale on the wire must read as null")
	}
	disabled := &serverv1.StorageAutoscale{Enabled: false}
	if got := storageAutoscaleFromProto(disabled, types.ObjectNull(storageAutoscaleAttrTypes)); !got.IsNull() {
		t.Error("a disabled response with no prior block must stay null, so an absent config never drifts")
	}
	priorDisabled := types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(false), "threshold_percent": types.Int64Value(0),
		"increase_percent": types.Int64Value(0), "max_storage": types.StringNull(),
	})
	if got := storageAutoscaleFromProto(disabled, priorDisabled); got.IsNull() {
		t.Error("a block the config explicitly disabled must stay visible")
	}
	on := &serverv1.StorageAutoscale{Enabled: true, ThresholdPercent: 80, IncreasePercent: 20, MaxStorage: "50Gi"}
	got := storageAutoscaleFromProto(on, types.ObjectNull(storageAutoscaleAttrTypes))
	if got.IsNull() {
		t.Fatal("an enabled response must always surface")
	}
	attrs := got.Attributes()
	if !attrs["enabled"].(types.Bool).ValueBool() || attrs["threshold_percent"].(types.Int64).ValueInt64() != 80 ||
		attrs["increase_percent"].(types.Int64).ValueInt64() != 20 || attrs["max_storage"].(types.String).ValueString() != "50Gi" {
		t.Errorf("enabled round trip = %+v", got)
	}
}

func TestStorageStatusFromProto(t *testing.T) {
	if got := storageStatusFromProto(nil); len(got.Elements()) != 0 {
		t.Errorf("no entries must read as an empty list, got %v", got)
	}
	got := storageStatusFromProto([]*serverv1.NodeGroupStorage{{NodeGroup: "writers", Size: "15Gi"}})
	elems := got.Elements()
	if len(elems) != 1 {
		t.Fatalf("entries = %v", elems)
	}
	attrs := elems[0].(types.Object).Attributes()
	if attrs["node_group"].(types.String).ValueString() != "writers" || attrs["size"].(types.String).ValueString() != "15Gi" {
		t.Errorf("entry = %+v", attrs)
	}
}

func TestOptionalString(t *testing.T) {
	if got := optionalString(""); !got.IsNull() {
		t.Errorf("an empty string must read as null, got %v", got)
	}
	if got := optionalString("50Gi"); got.ValueString() != "50Gi" {
		t.Errorf("optionalString(%q) = %v", "50Gi", got)
	}
}

func TestPrimaryNodeGroup(t *testing.T) {
	nodes := map[string]nodeGroupModel{
		"analytics": {Role: types.StringValue(roleReadOnly)},
		"dr":        {Role: types.StringValue(roleStandby)},
		"writers":   {Role: types.StringValue(roleElectable)},
		"backup":    {Role: types.StringValue(roleElectable)},
	}
	if got := primaryNodeGroup(nodes, types.StringNull()); got != "backup" {
		t.Errorf("with nothing chosen yet the first electable label wins, got %q", got)
	}
	if got := primaryNodeGroup(nodes, types.StringValue("writers")); got != "writers" {
		t.Errorf("an electable group the platform already chose must be kept, got %q", got)
	}
	if got := primaryNodeGroup(nodes, types.StringValue("analytics")); got != "backup" {
		t.Errorf("a group that is no longer electable must be replaced, got %q", got)
	}
	if got := primaryNodeGroup(nodes, types.StringValue("gone")); got != "backup" {
		t.Errorf("a group that no longer exists must be replaced, got %q", got)
	}
	if got := primaryNodeGroup(nodes, types.StringUnknown()); got != "backup" {
		t.Errorf("an unknown value must not be sent, got %q", got)
	}
	readOnly := map[string]nodeGroupModel{"analytics": {Role: types.StringValue(roleReadOnly)}}
	if got := primaryNodeGroup(readOnly, types.StringNull()); got != "" {
		t.Errorf("no electable group means nothing to name, got %q", got)
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
