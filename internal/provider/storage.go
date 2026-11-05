package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

var (
	storageAutoscaleAttrTypes = map[string]attr.Type{
		"enabled":           types.BoolType,
		"threshold_percent": types.Int64Type,
		"increase_percent":  types.Int64Type,
		"max_storage":       types.StringType,
	}
	storageAttrTypes = map[string]attr.Type{
		"size":      types.StringType,
		"autoscale": types.ObjectType{AttrTypes: storageAutoscaleAttrTypes},
	}
	storageStatusAttrTypes = map[string]attr.Type{"size": types.StringType, "last_expanded_at": types.StringType}
)

func storageAttribute(sizeDescription string) schema.SingleNestedAttribute {
	keepInt := []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}
	return schema.SingleNestedAttribute{
		Optional: true, Computed: true,
		PlanModifiers: []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
		MarkdownDescription: "Volume size and autoscaling for the whole cluster: every node holds the same data, so every node's volume has this size. " +
			"Omit the block and the platform picks the size, which the next read reports; once it is set, removing the whole block keeps the current size and autoscaling, so remove `autoscale` alone to turn it off. Disk type, IOPS and throughput stay per node group.",
		Attributes: map[string]schema.Attribute{
			"size": schema.StringAttribute{
				Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: sizeDescription,
			},
			"autoscale": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Grows the cluster's volumes past `size`, up to `max_storage`, when disk usage crosses `threshold_percent`. " +
					"Omitting this block, or removing it from a config that had it, disables autoscaling.",
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{Required: true, MarkdownDescription: "Turn storage autoscaling on for the cluster."},
					"threshold_percent": schema.Int64Attribute{
						Optional: true, Computed: true, PlanModifiers: keepInt,
						Validators:          []validator.Int64{int64validator.Any(int64validator.OneOf(0), int64validator.Between(50, 95))},
						MarkdownDescription: "Disk-usage percent that triggers a grow. `0` or omitted uses the platform default of 80; otherwise 50-95.",
					},
					"increase_percent": schema.Int64Attribute{
						Optional: true, Computed: true, PlanModifiers: keepInt,
						Validators:          []validator.Int64{int64validator.Any(int64validator.OneOf(0), int64validator.Between(10, 100))},
						MarkdownDescription: "Percent to grow the volume by on each trigger. `0` or omitted uses the platform default of 20; otherwise 10-100.",
					},
					"max_storage": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "Ceiling the platform will not grow past, Kubernetes quantity. Required when `enabled` is true, and must be at least `size`.",
					},
				},
			},
		},
	}
}

func storageDataAttribute() datasourceschema.SingleNestedAttribute {
	return datasourceschema.SingleNestedAttribute{
		Computed:            true,
		MarkdownDescription: "Volume size and autoscaling of the whole cluster.",
		Attributes: map[string]datasourceschema.Attribute{
			"size": datasourceschema.StringAttribute{Computed: true},
			"autoscale": datasourceschema.SingleNestedAttribute{
				Computed: true,
				Attributes: map[string]datasourceschema.Attribute{
					"enabled":           datasourceschema.BoolAttribute{Computed: true},
					"threshold_percent": datasourceschema.Int64Attribute{Computed: true},
					"increase_percent":  datasourceschema.Int64Attribute{Computed: true},
					"max_storage":       datasourceschema.StringAttribute{Computed: true},
				},
			},
		},
	}
}

func storageStatusDataAttribute() datasourceschema.SingleNestedAttribute {
	return datasourceschema.SingleNestedAttribute{
		Computed:            true,
		MarkdownDescription: "Volume size the platform has grown to via autoscaling, when above `storage.size`. Null until it grows.",
		Attributes: map[string]datasourceschema.Attribute{
			"size":             datasourceschema.StringAttribute{Computed: true},
			"last_expanded_at": datasourceschema.StringAttribute{Computed: true},
		},
	}
}

func storageToProto(o types.Object) *serverv1.ClusterStorage {
	if o.IsNull() || o.IsUnknown() {
		return nil
	}
	attrs := o.Attributes()
	out := &serverv1.ClusterStorage{Autoscale: storageAutoscaleToProto(attrs["autoscale"].(types.Object))}
	if v := attrs["size"].(types.String); !v.IsNull() && !v.IsUnknown() {
		out.Size = v.ValueString()
	}
	return out
}

func storageFromProto(s *serverv1.ClusterStorage, prior types.Object) types.Object {
	if s == nil {
		return types.ObjectNull(storageAttrTypes)
	}
	priorAutoscale := types.ObjectNull(storageAutoscaleAttrTypes)
	if !prior.IsNull() && !prior.IsUnknown() {
		priorAutoscale = prior.Attributes()["autoscale"].(types.Object)
	}
	return types.ObjectValueMust(storageAttrTypes, map[string]attr.Value{
		"size":      types.StringValue(s.GetSize()),
		"autoscale": storageAutoscaleFromProto(s.GetAutoscale(), priorAutoscale),
	})
}

func storageAutoscaleToProto(o types.Object) *serverv1.StorageAutoscale {
	out := &serverv1.StorageAutoscale{}
	if o.IsNull() || o.IsUnknown() {
		return out
	}
	attrs := o.Attributes()
	out.Enabled = attrs["enabled"].(types.Bool).ValueBool()
	if v := attrs["threshold_percent"].(types.Int64); !v.IsNull() && !v.IsUnknown() {
		out.ThresholdPercent = int32(v.ValueInt64())
	}
	if v := attrs["increase_percent"].(types.Int64); !v.IsNull() && !v.IsUnknown() {
		out.IncreasePercent = int32(v.ValueInt64())
	}
	if v := attrs["max_storage"].(types.String); !v.IsNull() && !v.IsUnknown() {
		out.MaxStorage = v.ValueString()
	}
	return out
}

func storageAutoscaleFromProto(sa *serverv1.StorageAutoscale, prior types.Object) types.Object {
	if sa == nil || (!sa.GetEnabled() && prior.IsNull()) {
		return types.ObjectNull(storageAutoscaleAttrTypes)
	}
	return types.ObjectValueMust(storageAutoscaleAttrTypes, map[string]attr.Value{
		"enabled":           types.BoolValue(sa.GetEnabled()),
		"threshold_percent": types.Int64Value(int64(sa.GetThresholdPercent())),
		"increase_percent":  types.Int64Value(int64(sa.GetIncreasePercent())),
		"max_storage":       optionalString(sa.GetMaxStorage()),
	})
}

func storageStatusFromProto(entries []*serverv1.NodeGroupStorage) types.Object {
	if len(entries) == 0 {
		return types.ObjectNull(storageStatusAttrTypes)
	}
	size := types.StringValue(entries[0].GetSize())
	var expanded *serverv1.NodeGroupStorage
	for _, e := range entries {
		if order, ok := compareQuantity(types.StringValue(e.GetSize()), size); ok && order > 0 {
			size = types.StringValue(e.GetSize())
		}
		if e.GetLastExpandedAt().IsValid() && (expanded == nil || e.GetLastExpandedAt().AsTime().After(expanded.GetLastExpandedAt().AsTime())) {
			expanded = e
		}
	}
	lastExpanded := types.StringNull()
	if expanded != nil {
		lastExpanded = timestampValue(expanded.GetLastExpandedAt())
	}
	return types.ObjectValueMust(storageStatusAttrTypes, map[string]attr.Value{"size": size, "last_expanded_at": lastExpanded})
}

func storageSizeOf(o types.Object) types.String {
	switch {
	case o.IsNull():
		return types.StringNull()
	case o.IsUnknown():
		return types.StringUnknown()
	}
	return o.Attributes()["size"].(types.String)
}

func storageAutoscaleOf(o types.Object) types.Object {
	if o.IsNull() || o.IsUnknown() {
		return types.ObjectNull(storageAutoscaleAttrTypes)
	}
	return o.Attributes()["autoscale"].(types.Object)
}

func withStorageSize(o types.Object, size types.String) types.Object {
	attrs := o.Attributes()
	attrs["size"] = size
	return types.ObjectValueMust(storageAttrTypes, attrs)
}

func withStorageAutoscale(o types.Object, autoscale types.Object) types.Object {
	attrs := o.Attributes()
	attrs["autoscale"] = autoscale
	return types.ObjectValueMust(storageAttrTypes, attrs)
}

func maxStorageOf(autoscale types.Object) types.String {
	if autoscale.IsNull() || autoscale.IsUnknown() {
		return types.StringNull()
	}
	return autoscale.Attributes()["max_storage"].(types.String)
}

func unknownComputedStorage(o types.Object) types.Object {
	if o.IsNull() || o.IsUnknown() {
		return o
	}
	return withStorageAutoscale(o, unknownComputedStorageAutoscale(storageAutoscaleOf(o)))
}

func unknownComputedStorageAutoscale(o types.Object) types.Object {
	if o.IsNull() || o.IsUnknown() {
		return o
	}
	attrs := o.Attributes()
	if attrs["threshold_percent"].(types.Int64).IsNull() {
		attrs["threshold_percent"] = types.Int64Unknown()
	}
	if attrs["increase_percent"].(types.Int64).IsNull() {
		attrs["increase_percent"] = types.Int64Unknown()
	}
	return types.ObjectValueMust(storageAutoscaleAttrTypes, attrs)
}

func autoscaleEnabledAnew(planned, current types.Object) bool {
	autoscale := storageAutoscaleOf(planned)
	return storageAutoscaleOf(current).IsNull() && !autoscale.IsNull() && !autoscale.IsUnknown()
}

func validateStorage(ctx context.Context, config tfsdk.Config, diags *diag.Diagnostics) {
	var storage types.Object
	diags.Append(config.GetAttribute(ctx, path.Root("storage"), &storage)...)
	autoscale := storageAutoscaleOf(storage)
	if autoscale.IsNull() || autoscale.IsUnknown() {
		return
	}
	attrs := autoscale.Attributes()
	if enabled, _ := attrs["enabled"].(types.Bool); !enabled.ValueBool() {
		return
	}
	target := path.Root("storage").AtName("autoscale").AtName("max_storage")
	maxStorage, _ := attrs["max_storage"].(types.String)
	if maxStorage.IsUnknown() {
		return
	}
	if maxStorage.ValueString() == "" {
		diags.AddAttributeError(target, "Missing max_storage",
			"Storage autoscaling needs a ceiling: set max_storage to the largest size the platform may grow the cluster's volumes to.")
		return
	}
	size := storageSizeOf(storage)
	if order, ok := compareQuantity(maxStorage, size); ok && order < 0 {
		diags.AddAttributeError(target, "max_storage is below size",
			"The cluster has storage size "+size.ValueString()+" but max_storage "+maxStorage.ValueString()+" is smaller. max_storage must be at least size.")
	}
}

func rejectShrink(planned, current types.Object, diags *diag.Diagnostics) {
	if shrinks(storageSizeOf(planned), storageSizeOf(current)) {
		diags.AddAttributeError(path.Root("storage").AtName("size"), "Storage cannot shrink",
			"Volumes only grow. The cluster is at "+storageSizeOf(current).ValueString()+" and the plan asks for "+storageSizeOf(planned).ValueString()+".")
	}
}

func compareQuantity(a, b types.String) (int, bool) {
	if a.IsUnknown() || b.IsUnknown() || a.IsNull() || b.IsNull() {
		return 0, false
	}
	x, xok := parseQuantity(a.ValueString())
	y, yok := parseQuantity(b.ValueString())
	switch {
	case !xok || !yok:
		return 0, false
	case x < y:
		return -1, true
	case x > y:
		return 1, true
	}
	return 0, true
}

func keepPlannedStorage(state, plan types.Object) types.Object {
	if size := storageSizeOf(plan); !size.IsUnknown() && !size.IsNull() {
		state = withStorageSize(state, size)
	}
	have := storageAutoscaleOf(state)
	if want := maxStorageOf(storageAutoscaleOf(plan)); !want.IsNull() && !want.IsUnknown() && !have.IsNull() {
		attrs := have.Attributes()
		attrs["max_storage"] = want
		state = withStorageAutoscale(state, types.ObjectValueMust(storageAutoscaleAttrTypes, attrs))
	}
	return state
}

func keepRaisedMaxStorage(planned, current types.Object) types.Object {
	size := storageSizeOf(planned)
	want, have := maxStorageOf(storageAutoscaleOf(planned)), maxStorageOf(storageAutoscaleOf(current))
	if want.IsNull() || want.IsUnknown() || have.IsNull() {
		return types.ObjectNull(storageAutoscaleAttrTypes)
	}
	below, ok := compareQuantity(want, size)
	kept, kok := compareQuantity(have, want)
	held, hok := compareQuantity(have, size)
	if !ok || !kok || !hok || below > 0 || held < 0 || (below == 0 && kept != 0) {
		return types.ObjectNull(storageAutoscaleAttrTypes)
	}
	attrs := storageAutoscaleOf(planned).Attributes()
	attrs["max_storage"] = have
	return types.ObjectValueMust(storageAutoscaleAttrTypes, attrs)
}

func largest(sizes []types.String) types.String {
	best := types.StringNull()
	for _, s := range sizes {
		if s.IsNull() || s.IsUnknown() {
			continue
		}
		if best.IsNull() {
			best = s
		} else if order, ok := compareQuantity(s, best); ok && order > 0 {
			best = s
		}
	}
	return best
}

func upgradedStorage(sizes []types.String, primary types.Object, maxStorages []types.String) types.Object {
	autoscale := primary
	if !autoscale.IsNull() && !autoscale.IsUnknown() {
		attrs := autoscale.Attributes()
		if ceiling := largest(maxStorages); !ceiling.IsNull() {
			attrs["max_storage"] = ceiling
		}
		autoscale = types.ObjectValueMust(storageAutoscaleAttrTypes, attrs)
	}
	return types.ObjectValueMust(storageAttrTypes, map[string]attr.Value{"size": largest(sizes), "autoscale": autoscale})
}

func upgradedStorageStatus(old types.List) types.Object {
	var sizes []types.String
	for _, e := range old.Elements() {
		sizes = append(sizes, e.(types.Object).Attributes()["size"].(types.String))
	}
	size := largest(sizes)
	if size.IsNull() {
		return types.ObjectNull(storageStatusAttrTypes)
	}
	return types.ObjectValueMust(storageStatusAttrTypes, map[string]attr.Value{"size": size, "last_expanded_at": types.StringNull()})
}
