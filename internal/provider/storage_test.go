package provider

import (
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func autoscaleObject(enabled bool, threshold, increase int64, limit types.String) types.Object {
	return types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(enabled), "threshold_percent": types.Int64Value(threshold),
		"increase_percent": types.Int64Value(increase), "max_storage": limit,
	})
}

func TestStorageAutoscaleConversions(t *testing.T) {
	if got := storageAutoscaleToProto(types.ObjectNull(storageAutoscaleAttrTypes)); got == nil || got.GetEnabled() {
		t.Errorf("a null block must still send an explicit disabled StorageAutoscale, got %+v", got)
	}
	if got := storageAutoscaleToProto(types.ObjectUnknown(storageAutoscaleAttrTypes)); got == nil || got.GetEnabled() {
		t.Errorf("an unknown block must not enable anything, got %+v", got)
	}
	enabled := autoscaleObject(true, 85, 25, types.StringValue("50Gi"))
	if got := storageAutoscaleToProto(enabled); !got.GetEnabled() || got.GetThresholdPercent() != 85 || got.GetIncreasePercent() != 25 || got.GetMaxStorage() != "50Gi" {
		t.Errorf("enabled block = %+v", got)
	}
	unknownFields := types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(true), "threshold_percent": types.Int64Unknown(),
		"increase_percent": types.Int64Unknown(), "max_storage": types.StringUnknown(),
	})
	if got := storageAutoscaleToProto(unknownFields); !got.GetEnabled() || got.ThresholdPercent != 0 || got.IncreasePercent != 0 || got.MaxStorage != "" {
		t.Errorf("unknown fields must be omitted, got %+v", got)
	}

	if got := storageAutoscaleFromProto(nil, types.ObjectNull(storageAutoscaleAttrTypes)); !got.IsNull() {
		t.Error("no StorageAutoscale on the wire must read as null")
	}
	disabled := &serverv1.StorageAutoscale{}
	if got := storageAutoscaleFromProto(disabled, types.ObjectNull(storageAutoscaleAttrTypes)); !got.IsNull() {
		t.Error("a disabled autoscale with nothing configured must read as null")
	}
	if got := storageAutoscaleFromProto(disabled, autoscaleObject(false, 0, 0, types.StringNull())); got.IsNull() {
		t.Error("a disabled autoscale the config spelled out must stay visible")
	}
	on := &serverv1.StorageAutoscale{Enabled: true, ThresholdPercent: 80, IncreasePercent: 20, MaxStorage: "50Gi"}
	attrs := storageAutoscaleFromProto(on, types.ObjectNull(storageAutoscaleAttrTypes)).Attributes()
	if !attrs["enabled"].(types.Bool).ValueBool() || attrs["threshold_percent"].(types.Int64).ValueInt64() != 80 ||
		attrs["increase_percent"].(types.Int64).ValueInt64() != 20 || attrs["max_storage"].(types.String).ValueString() != "50Gi" {
		t.Errorf("enabled round trip = %+v", attrs)
	}
}

func TestStorageConversions(t *testing.T) {
	if storageToProto(types.ObjectNull(storageAttrTypes)) != nil || storageToProto(types.ObjectUnknown(storageAttrTypes)) != nil {
		t.Error("null or unknown storage must not be sent")
	}
	null := types.ObjectNull(storageAutoscaleAttrTypes)
	if got := storageToProto(clusterStorage(types.StringUnknown(), null)); got.GetSize() != "" || got.GetAutoscale() == nil || got.GetAutoscale().GetEnabled() {
		t.Errorf("an unknown size is sent empty with autoscale off, got %+v", got)
	}
	if got := storageToProto(clusterStorage(types.StringNull(), null)); got.GetSize() != "" {
		t.Errorf("a null size is sent empty, got %+v", got)
	}
	want := clusterStorage(types.StringValue("20Gi"), autoscaleObject(true, 80, 20, types.StringValue("50Gi")))
	wire := storageToProto(want)
	if wire.GetSize() != "20Gi" || !wire.GetAutoscale().GetEnabled() || wire.GetAutoscale().GetMaxStorage() != "50Gi" {
		t.Fatalf("storage = %+v", wire)
	}
	if got := storageFromProto(wire, types.ObjectNull(storageAttrTypes)); !got.Equal(want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
	if got := storageFromProto(wire, types.ObjectUnknown(storageAttrTypes)); !got.Equal(want) {
		t.Errorf("round trip over an unknown prior = %v, want %v", got, want)
	}
	if got := storageFromProto(nil, want); !got.IsNull() {
		t.Errorf("no storage on the wire must read as null, got %v", got)
	}
	off := &serverv1.ClusterStorage{Size: "20Gi", Autoscale: &serverv1.StorageAutoscale{}}
	if got := storageFromProto(off, types.ObjectUnknown(storageAttrTypes)); !storageAutoscaleOf(got).IsNull() {
		t.Errorf("autoscale the platform reports off must read as null when nothing was configured, got %v", got)
	}
	if got := storageFromProto(off, clusterStorage(types.StringValue("20Gi"), autoscaleObject(false, 0, 0, types.StringNull()))); storageAutoscaleOf(got).IsNull() {
		t.Error("autoscale spelled out as disabled must stay visible")
	}
}

func TestStorageStatusFromProto(t *testing.T) {
	at := func(sec int64) *timestamppb.Timestamp { return timestamppb.New(time.Unix(sec, 0)) }
	if got := storageStatusFromProto(nil); !got.IsNull() {
		t.Errorf("no entries must read as null, got %v", got)
	}
	cases := map[string]struct {
		entries []*serverv1.NodeGroupStorage
		size    string
		expand  string
	}{
		"single entry without a timestamp": {[]*serverv1.NodeGroupStorage{{NodeGroup: "writers", Size: "15Gi"}}, "15Gi", ""},
		"largest size and latest timestamp": {[]*serverv1.NodeGroupStorage{
			{NodeGroup: "a", Size: "15Gi", LastExpandedAt: at(1_700_003_600)},
			{NodeGroup: "b", Size: "20Gi", LastExpandedAt: at(1_700_000_000)},
			{NodeGroup: "c", Size: "20Gi"},
			{NodeGroup: "d", Size: "9Gi", LastExpandedAt: at(1_600_000_000)},
		}, "20Gi", "2023-11-14T23:13:20Z"},
		"sizes in other units":               {[]*serverv1.NodeGroupStorage{{Size: "10Gi"}, {Size: "20480Mi"}}, "20480Mi", ""},
		"unparsable size is kept when alone": {[]*serverv1.NodeGroupStorage{{Size: "lots"}, {Size: "5Gi"}}, "lots", ""},
	}
	for name, c := range cases {
		attrs := storageStatusFromProto(c.entries).Attributes()
		if got := attrs["size"].(types.String).ValueString(); got != c.size {
			t.Errorf("%s: size = %q, want %q", name, got, c.size)
		}
		expanded := attrs["last_expanded_at"].(types.String)
		if c.expand == "" && !expanded.IsNull() || c.expand != "" && expanded.ValueString() != c.expand {
			t.Errorf("%s: last_expanded_at = %v, want %q", name, expanded, c.expand)
		}
	}
}

func TestStorageHelpers(t *testing.T) {
	null := types.ObjectNull(storageAttrTypes)
	if !storageSizeOf(null).IsNull() || !storageSizeOf(types.ObjectUnknown(storageAttrTypes)).IsUnknown() {
		t.Error("a missing storage object has no size")
	}
	if !storageAutoscaleOf(null).IsNull() || !storageAutoscaleOf(types.ObjectUnknown(storageAttrTypes)).IsNull() {
		t.Error("a missing storage object has no autoscale")
	}
	if got := unknownComputedStorage(null); !got.IsNull() {
		t.Error("a null storage stays null")
	}
	pending := clusterStorage(types.StringValue("10Gi"), types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(true), "threshold_percent": types.Int64Null(),
		"increase_percent": types.Int64Value(30), "max_storage": types.StringValue("50Gi"),
	}))
	attrs := storageAutoscaleOf(unknownComputedStorage(pending)).Attributes()
	if !attrs["threshold_percent"].IsUnknown() || attrs["increase_percent"].IsUnknown() {
		t.Errorf("only the null computed fields become unknown, got %+v", attrs)
	}
	if got := unknownComputedStorageAutoscale(types.ObjectUnknown(storageAutoscaleAttrTypes)); !got.IsUnknown() {
		t.Error("an unknown autoscale stays unknown")
	}

	noAutoscale := clusterStorage(types.StringValue("10Gi"), types.ObjectNull(storageAutoscaleAttrTypes))
	if !autoscaleEnabledAnew(pending, noAutoscale) {
		t.Error("enabling autoscale on a cluster that had none is new")
	}
	if autoscaleEnabledAnew(pending, pending) || autoscaleEnabledAnew(noAutoscale, noAutoscale) {
		t.Error("an unchanged autoscale is not new")
	}
	if autoscaleEnabledAnew(clusterStorage(types.StringValue("10Gi"), types.ObjectUnknown(storageAutoscaleAttrTypes)), noAutoscale) {
		t.Error("an unknown autoscale is not new")
	}
}

func TestLargest(t *testing.T) {
	if got := largest(nil); !got.IsNull() {
		t.Errorf("nothing to compare is null, got %v", got)
	}
	got := largest([]types.String{types.StringNull(), types.StringUnknown(), types.StringValue("10Gi"), types.StringValue("5Gi"), types.StringValue("20480Mi"), types.StringValue("lots")})
	if got.ValueString() != "20480Mi" {
		t.Errorf("largest = %v", got)
	}
}

func TestUpgradedStorage(t *testing.T) {
	sizes := []types.String{types.StringValue("10Gi"), types.StringValue("30Gi"), types.StringValue("20Gi")}
	limits := []types.String{types.StringNull(), types.StringValue("100Gi"), types.StringValue("60Gi")}
	primary := autoscaleObject(true, 85, 25, types.StringValue("60Gi"))
	got := upgradedStorage(sizes, primary, limits)
	if storageSizeOf(got).ValueString() != "30Gi" {
		t.Errorf("size = %v, want the largest of the groups", storageSizeOf(got))
	}
	attrs := storageAutoscaleOf(got).Attributes()
	if attrs["threshold_percent"].(types.Int64).ValueInt64() != 85 || attrs["max_storage"].(types.String).ValueString() != "100Gi" {
		t.Errorf("autoscale = %+v, want the primary's settings with the largest ceiling", attrs)
	}
	noCeiling := upgradedStorage(sizes, primary, []types.String{types.StringNull()})
	if maxStorageOf(storageAutoscaleOf(noCeiling)).ValueString() != "60Gi" {
		t.Errorf("without any other ceiling the primary's stays, got %v", storageAutoscaleOf(noCeiling))
	}
	off := upgradedStorage(sizes, types.ObjectNull(storageAutoscaleAttrTypes), limits)
	if !storageAutoscaleOf(off).IsNull() {
		t.Errorf("a primary without autoscale upgrades to none, got %v", storageAutoscaleOf(off))
	}
}

func TestUpgradedStorageStatus(t *testing.T) {
	elem := types.ObjectType{AttrTypes: map[string]attr.Type{"node_group": types.StringType, "size": types.StringType}}
	entry := func(group, size string) attr.Value {
		return types.ObjectValueMust(elem.AttrTypes, map[string]attr.Value{"node_group": types.StringValue(group), "size": types.StringValue(size)})
	}
	if got := upgradedStorageStatus(types.ListValueMust(elem, nil)); !got.IsNull() {
		t.Errorf("an empty list upgrades to null, got %v", got)
	}
	got := upgradedStorageStatus(types.ListValueMust(elem, []attr.Value{entry("a", "15Gi"), entry("b", "25Gi")})).Attributes()
	if got["size"].(types.String).ValueString() != "25Gi" || !got["last_expanded_at"].(types.String).IsNull() {
		t.Errorf("status = %+v", got)
	}
}
