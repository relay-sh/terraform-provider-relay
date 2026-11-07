package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/durationpb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func computeRule(metric string, up, down int64) attr.Value {
	return types.ObjectValueMust(computeRuleAttrTypes, map[string]attr.Value{
		"metric": types.StringValue(metric), "scale_up_above": types.Int64Value(up), "scale_down_below": types.Int64Value(down),
	})
}

func computeAutoscaling(enabled bool, rules types.List, up, down types.String) types.Object {
	return types.ObjectValueMust(computeAutoscalingAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(enabled), "min_tier": types.StringValue("R5"), "max_tier": types.StringValue("R20"),
		"rules": rules, "scale_up_window": up, "scale_down_window": down, "respect_maintenance_window": types.BoolValue(true),
	})
}

func TestComputeAutoscalingToProto(t *testing.T) {
	for name, o := range map[string]types.Object{"null": types.ObjectNull(computeAutoscalingAttrTypes), "unknown": types.ObjectUnknown(computeAutoscalingAttrTypes)} {
		if got := computeAutoscalingToProto(o); got == nil || got.GetEnabled() {
			t.Errorf("a %s block must still send an explicit disabled ComputeAutoscaling, got %+v", name, got)
		}
	}
	rules := types.ListValueMust(computeRuleType, []attr.Value{computeRule("cpu", 80, 30)})
	got := computeAutoscalingToProto(computeAutoscaling(true, rules, types.StringValue("5m"), types.StringNull()))
	if !got.GetEnabled() || got.GetMinTier() != "R5" || got.GetMaxTier() != "R20" || !got.GetRespectMaintenanceWindow() {
		t.Errorf("autoscaling = %+v", got)
	}
	if got.GetScaleUpWindow().AsDuration() != 5*time.Minute || got.GetScaleDownWindow() != nil {
		t.Errorf("windows = %v / %v, want 5m and unset", got.GetScaleUpWindow(), got.GetScaleDownWindow())
	}
	if len(got.GetRules()) != 1 || got.GetRules()[0].GetMetric() != "cpu" || got.GetRules()[0].GetScaleUpAbove() != 80 || got.GetRules()[0].GetScaleDownBelow() != 30 {
		t.Errorf("rules = %+v", got.GetRules())
	}
	if w := windowToProto(types.StringUnknown()); w != nil {
		t.Errorf("an unknown window must not be sent, got %v", w)
	}
}

func TestComputeAutoscalingFromProto(t *testing.T) {
	null := types.ObjectNull(computeAutoscalingAttrTypes)
	if got := computeAutoscalingFromProto(nil, null); !got.IsNull() {
		t.Error("no autoscaling on the wire must read as null")
	}
	off := &serverv1.ComputeAutoscaling{}
	if got := computeAutoscalingFromProto(off, null); !got.IsNull() {
		t.Error("a disabled block with nothing configured must read as null")
	}
	spelled := computeAutoscaling(false, types.ListNull(computeRuleType), types.StringNull(), types.StringNull())
	if got := computeAutoscalingFromProto(off, spelled); got.IsNull() || got.Attributes()["enabled"].(types.Bool).ValueBool() {
		t.Errorf("a disabled block the config spelled out must stay visible, got %v", got)
	}

	on := &serverv1.ComputeAutoscaling{
		Enabled: true, MinTier: "R5", MaxTier: "R20", RespectMaintenanceWindow: true,
		Rules:         []*serverv1.ComputeRule{{Metric: "cpu", ScaleUpAbove: 80, ScaleDownBelow: 30}},
		ScaleUpWindow: durationpb.New(5 * time.Minute), ScaleDownWindow: durationpb.New(90 * time.Minute),
	}
	attrs := computeAutoscalingFromProto(on, types.ObjectUnknown(computeAutoscalingAttrTypes)).Attributes()
	if attrs["min_tier"].(types.String).ValueString() != "R5" || attrs["max_tier"].(types.String).ValueString() != "R20" || !attrs["respect_maintenance_window"].(types.Bool).ValueBool() {
		t.Errorf("enabled read = %v", attrs)
	}
	if up, down := attrs["scale_up_window"].(types.String).ValueString(), attrs["scale_down_window"].(types.String).ValueString(); up != "5m" || down != "1h30m" {
		t.Errorf("windows without a prior spelling = %q / %q, want 5m and 1h30m", up, down)
	}
	if want := types.ListValueMust(computeRuleType, []attr.Value{computeRule("cpu", 80, 30)}); !attrs["rules"].Equal(want) {
		t.Errorf("rules = %v, want %v", attrs["rules"], want)
	}

	prior := computeAutoscaling(true, types.ListNull(computeRuleType), types.StringValue("300s"), types.StringValue("2h"))
	attrs = computeAutoscalingFromProto(on, prior).Attributes()
	if up, down := attrs["scale_up_window"].(types.String).ValueString(), attrs["scale_down_window"].(types.String).ValueString(); up != "300s" || down != "1h30m" {
		t.Errorf("windows = %q / %q, want the configured 300s kept and the changed one formatted", up, down)
	}

	bare := &serverv1.ComputeAutoscaling{Enabled: true, MinTier: "R5", MaxTier: "R20"}
	attrs = computeAutoscalingFromProto(bare, null).Attributes()
	if !attrs["rules"].IsNull() || !attrs["scale_up_window"].IsNull() || !attrs["scale_down_window"].IsNull() {
		t.Errorf("no rules or windows on the wire must read as null, got %v", attrs)
	}
	empty := computeAutoscaling(true, types.ListValueMust(computeRuleType, nil), types.StringNull(), types.StringNull())
	if got := computeAutoscalingFromProto(bare, empty).Attributes()["rules"].(types.List); got.IsNull() || len(got.Elements()) != 0 {
		t.Errorf("an empty rules list the config spelled out must stay empty, got %v", got)
	}
}

func TestWindowValidator(t *testing.T) {
	v := windowValidator{}
	if v.Description(context.Background()) == "" || v.MarkdownDescription(context.Background()) != v.Description(context.Background()) {
		t.Error("the validator must describe itself")
	}
	cases := map[string]struct {
		value types.String
		fails bool
	}{
		"null":     {types.StringNull(), false},
		"unknown":  {types.StringUnknown(), false},
		"valid":    {types.StringValue("1h30m"), false},
		"zero":     {types.StringValue("0s"), true},
		"negative": {types.StringValue("-5m"), true},
		"garbage":  {types.StringValue("soon"), true},
	}
	for name, c := range cases {
		resp := &validator.StringResponse{}
		v.ValidateString(context.Background(), validator.StringRequest{Path: path.Root("w"), ConfigValue: c.value}, resp)
		if resp.Diagnostics.HasError() != c.fails {
			t.Errorf("%s: error = %v, want %v", name, resp.Diagnostics, c.fails)
		}
	}
}

func TestCurrentTierFromProto(t *testing.T) {
	compute := []*serverv1.NodeGroupCompute{{NodeGroup: "writers", Tier: "R20"}, {NodeGroup: "readers"}}
	if got := currentTierFromProto(compute, "writers"); got.ValueString() != "R20" {
		t.Errorf("writers = %v, want R20", got)
	}
	if got := currentTierFromProto(compute, "readers"); !got.IsNull() {
		t.Errorf("an empty reported tier must read as null, got %v", got)
	}
	if got := currentTierFromProto(compute, "analytics"); !got.IsNull() {
		t.Errorf("a group the platform does not report must read as null, got %v", got)
	}
}

func TestAutoscalingEnabled(t *testing.T) {
	pending := types.ObjectValueMust(computeAutoscalingAttrTypes, map[string]attr.Value{
		"enabled": types.BoolUnknown(), "min_tier": types.StringNull(), "max_tier": types.StringNull(), "rules": types.ListNull(computeRuleType),
		"scale_up_window": types.StringNull(), "scale_down_window": types.StringNull(), "respect_maintenance_window": types.BoolValue(false),
	})
	cases := map[string]struct {
		o    types.Object
		want bool
	}{
		"null":             {types.ObjectNull(computeAutoscalingAttrTypes), false},
		"unknown":          {types.ObjectUnknown(computeAutoscalingAttrTypes), false},
		"enabled unknown":  {pending, true},
		"enabled":          {computeAutoscaling(true, types.ListNull(computeRuleType), types.StringNull(), types.StringNull()), true},
		"explicitly off":   {computeAutoscaling(false, types.ListNull(computeRuleType), types.StringNull(), types.StringNull()), false},
		"enabled no rules": {computeAutoscaling(true, types.ListValueMust(computeRuleType, nil), types.StringNull(), types.StringNull()), true},
	}
	for name, c := range cases {
		if got := autoscalingEnabled(c.o); got != c.want {
			t.Errorf("%s: autoscalingEnabled = %v, want %v", name, got, c.want)
		}
	}
}

func TestValidateComputeAutoscaling(t *testing.T) {
	groupType := types.ObjectType{AttrTypes: map[string]attr.Type{"autoscaling": types.ObjectType{AttrTypes: computeAutoscalingAttrTypes}}}
	nodes := func(groups map[string]types.Object) types.Map {
		elems := map[string]attr.Value{}
		for label, a := range groups {
			elems[label] = types.ObjectValueMust(groupType.AttrTypes, map[string]attr.Value{"autoscaling": a})
		}
		return types.MapValueMust(groupType, elems)
	}
	withBounds := func(enabled types.Bool, lo, hi types.String, rules types.List) types.Object {
		return types.ObjectValueMust(computeAutoscalingAttrTypes, map[string]attr.Value{
			"enabled": enabled, "min_tier": lo, "max_tier": hi, "rules": rules,
			"scale_up_window": types.StringNull(), "scale_down_window": types.StringNull(), "respect_maintenance_window": types.BoolNull(),
		})
	}
	noRules := types.ListNull(computeRuleType)
	unknownRule := types.ObjectValueMust(computeRuleAttrTypes, map[string]attr.Value{
		"metric": types.StringUnknown(), "scale_up_above": types.Int64Unknown(), "scale_down_below": types.Int64Value(90),
	})
	cases := map[string]struct {
		nodes types.Map
		want  []string
	}{
		"null map":    {types.MapNull(groupType), nil},
		"unknown map": {types.MapUnknown(groupType), nil},
		"no block":    {nodes(map[string]types.Object{"writers": types.ObjectNull(computeAutoscalingAttrTypes)}), nil},
		"unknown":     {nodes(map[string]types.Object{"writers": types.ObjectUnknown(computeAutoscalingAttrTypes)}), nil},
		"off without bounds": {nodes(map[string]types.Object{
			"writers": withBounds(types.BoolValue(false), types.StringNull(), types.StringNull(), noRules),
		}), nil},
		"unknown bounds": {nodes(map[string]types.Object{
			"writers": withBounds(types.BoolValue(true), types.StringUnknown(), types.StringUnknown(), noRules),
		}), nil},
		"missing bounds": {nodes(map[string]types.Object{
			"writers": withBounds(types.BoolValue(true), types.StringNull(), types.StringNull(), noRules),
		}), []string{"Missing min_tier", "Missing max_tier"}},
		"duplicate metric and inverted thresholds": {nodes(map[string]types.Object{
			"writers": withBounds(types.BoolValue(true), types.StringValue("R5"), types.StringValue("R20"),
				types.ListValueMust(computeRuleType, []attr.Value{computeRule("cpu", 80, 30), computeRule("cpu", 50, 50), unknownRule})),
		}), []string{"Duplicate metric", "scale_down_below is not below scale_up_above"}},
	}
	for name, c := range cases {
		var diags diag.Diagnostics
		validateComputeAutoscaling(c.nodes, &diags)
		var got []string
		for _, d := range diags {
			got = append(got, d.Summary())
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: diagnostics = %v, want %v", name, got, c.want)
		}
	}
}

func TestNodeGroupCurrentTierHelpers(t *testing.T) {
	on := computeAutoscaling(true, types.ListNull(computeRuleType), types.StringNull(), types.StringNull())
	off := types.ObjectNull(computeAutoscalingAttrTypes)
	pg := map[string]nodeGroupModel{
		"writers": {Tier: types.StringValue("R10"), Autoscaling: on, CurrentTier: types.StringValue("R20")},
		"readers": {Tier: types.StringValue("R10"), Autoscaling: off, CurrentTier: types.StringNull()},
	}
	if specs := nodeSpecs(pg); !specs["writers"].CurrentTier.IsNull() || pg["writers"].CurrentTier.ValueString() != "R20" {
		t.Errorf("nodeSpecs must drop current_tier from a copy, got %v and %v", specs["writers"].CurrentTier, pg["writers"].CurrentTier)
	}
	if !releaseCurrentTiers(pg) || !pg["writers"].CurrentTier.IsUnknown() || !pg["readers"].CurrentTier.IsNull() {
		t.Errorf("only the autoscaled group's current_tier is released, got %v and %v", pg["writers"].CurrentTier, pg["readers"].CurrentTier)
	}
	if releaseCurrentTiers(map[string]nodeGroupModel{"readers": {Autoscaling: off}}) {
		t.Error("no autoscaled group means nothing to release")
	}

	vk := map[string]valkeyNodeGroupModel{"cache": {Autoscaling: on, CurrentTier: types.StringValue("RC20")}}
	if !nodeSpecs(vk)["cache"].CurrentTier.IsNull() {
		t.Error("nodeSpecs must drop current_tier for Valkey groups")
	}
	if !releaseCurrentTiers(vk) || !vk["cache"].CurrentTier.IsUnknown() {
		t.Errorf("the autoscaled Valkey group's current_tier is released, got %v", vk["cache"].CurrentTier)
	}
	if releaseCurrentTiers(map[string]valkeyNodeGroupModel{"cache": {Autoscaling: off}}) {
		t.Error("a Valkey group without autoscaling has nothing to release")
	}
}
