package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	datasourceschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/protobuf/types/known/durationpb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

const maxComputeRules = 8

var (
	computeRuleAttrTypes        = map[string]attr.Type{"metric": types.StringType, "scale_up_above": types.Int64Type, "scale_down_below": types.Int64Type}
	computeRuleType             = types.ObjectType{AttrTypes: computeRuleAttrTypes}
	computeAutoscalingAttrTypes = map[string]attr.Type{
		"enabled":                    types.BoolType,
		"min_tier":                   types.StringType,
		"max_tier":                   types.StringType,
		"rules":                      types.ListType{ElemType: computeRuleType},
		"scale_up_window":            types.StringType,
		"scale_down_window":          types.StringType,
		"respect_maintenance_window": types.BoolType,
	}
)

func computeAutoscalingAttribute(metrics string) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Moves the group one tier at a time between `min_tier` and `max_tier`; the node count never changes. " +
			"`tier` keeps the configured value, so a move never shows as drift; the tier the group runs at is `current_tier`. " +
			"Omitting this block, or removing it from a config that had it, turns autoscaling off. Needs the `compute_autoscale` feature, and the Free plan cannot enable it.",
		Attributes: map[string]schema.Attribute{
			"enabled": schema.BoolAttribute{Required: true, MarkdownDescription: "Turn compute autoscaling on for the group."},
			"min_tier": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Lowest tier the group may move down to. Required when `enabled` is true, and `tier` must lie between `min_tier` and `max_tier`.",
			},
			"max_tier": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Highest tier the group may move up to. Required when `enabled` is true.",
			},
			"rules": schema.ListNestedAttribute{
				Optional:   true,
				Validators: []validator.List{listvalidator.SizeAtMost(maxComputeRules)},
				MarkdownDescription: "Metrics that move the group, at most 8, each metric once. Omit it for the Relay default policy: `cpu` and the engine's memory metric, up above 75 and down below 40. " +
					"The group moves up when any rule stays above `scale_up_above` for `scale_up_window`, and down when every rule stays below `scale_down_below` for `scale_down_window`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"metric": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Metric from the engine's autoscaling catalog: " + metrics + ". The platform rejects any other.",
					},
					"scale_up_above": schema.Int64Attribute{
						Required:            true,
						Validators:          []validator.Int64{int64validator.AtLeast(0)},
						MarkdownDescription: "Value above which the group moves one tier up, in the metric's unit. A percentage is at most 100.",
					},
					"scale_down_below": schema.Int64Attribute{
						Required:            true,
						Validators:          []validator.Int64{int64validator.AtLeast(0)},
						MarkdownDescription: "Value below which the group moves one tier down. Must be lower than `scale_up_above`.",
					},
				}},
			},
			"scale_up_window": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{windowValidator{}},
				MarkdownDescription: "How long a rule must stay above its threshold before the group moves up, a duration such as `5m`. Omitted, the platform waits 10 minutes.",
			},
			"scale_down_window": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{windowValidator{}},
				MarkdownDescription: "How long every rule must stay below its threshold before the group moves down, a duration such as `6h`. Omitted, the platform waits 24 hours.",
			},
			"respect_maintenance_window": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Hold moves up until the cluster's maintenance window too. Moves down always wait for it.",
			},
		},
	}
}

func currentTierAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Computed:            true,
		PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		MarkdownDescription: "Tier the group runs at after autoscaling moved it. Null while `autoscaling` is off or before the platform first reports it.",
	}
}

func computeAutoscalingDataAttribute() datasourceschema.SingleNestedAttribute {
	return datasourceschema.SingleNestedAttribute{
		Computed:            true,
		MarkdownDescription: "Compute autoscaling of the group. Null when it was never enabled.",
		Attributes: map[string]datasourceschema.Attribute{
			"enabled":  datasourceschema.BoolAttribute{Computed: true},
			"min_tier": datasourceschema.StringAttribute{Computed: true},
			"max_tier": datasourceschema.StringAttribute{Computed: true},
			"rules": datasourceschema.ListNestedAttribute{
				Computed: true,
				NestedObject: datasourceschema.NestedAttributeObject{Attributes: map[string]datasourceschema.Attribute{
					"metric":           datasourceschema.StringAttribute{Computed: true},
					"scale_up_above":   datasourceschema.Int64Attribute{Computed: true},
					"scale_down_below": datasourceschema.Int64Attribute{Computed: true},
				}},
			},
			"scale_up_window":            datasourceschema.StringAttribute{Computed: true},
			"scale_down_window":          datasourceschema.StringAttribute{Computed: true},
			"respect_maintenance_window": datasourceschema.BoolAttribute{Computed: true},
		},
	}
}

type windowValidator struct{}

func (windowValidator) Description(context.Context) string {
	return "a positive Go duration"
}

func (v windowValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (windowValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, ok := parseWindow(req.ConfigValue); !ok {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid window", "Expected a positive duration such as 5m or 1h30m, got "+req.ConfigValue.ValueString()+".")
	}
}

func parseWindow(v types.String) (time.Duration, bool) {
	d, err := time.ParseDuration(v.ValueString())
	return d, err == nil && d > 0
}

func computeAutoscalingToProto(o types.Object) *serverv1.ComputeAutoscaling {
	out := &serverv1.ComputeAutoscaling{}
	if o.IsNull() || o.IsUnknown() {
		return out
	}
	attrs := o.Attributes()
	out.Enabled = attrs["enabled"].(types.Bool).ValueBool()
	out.MinTier = attrs["min_tier"].(types.String).ValueString()
	out.MaxTier = attrs["max_tier"].(types.String).ValueString()
	out.RespectMaintenanceWindow = attrs["respect_maintenance_window"].(types.Bool).ValueBool()
	out.ScaleUpWindow = windowToProto(attrs["scale_up_window"].(types.String))
	out.ScaleDownWindow = windowToProto(attrs["scale_down_window"].(types.String))
	for _, e := range attrs["rules"].(types.List).Elements() {
		rule := e.(types.Object).Attributes()
		out.Rules = append(out.Rules, &serverv1.ComputeRule{
			Metric:         rule["metric"].(types.String).ValueString(),
			ScaleUpAbove:   rule["scale_up_above"].(types.Int64).ValueInt64(),
			ScaleDownBelow: rule["scale_down_below"].(types.Int64).ValueInt64(),
		})
	}
	return out
}

func windowToProto(v types.String) *durationpb.Duration {
	d, ok := parseWindow(v)
	if !ok {
		return nil
	}
	return durationpb.New(d)
}

func computeAutoscalingFromProto(a *serverv1.ComputeAutoscaling, prior types.Object) types.Object {
	if a == nil || (!a.GetEnabled() && prior.IsNull()) {
		return types.ObjectNull(computeAutoscalingAttrTypes)
	}
	priorAttrs := map[string]attr.Value{}
	if !prior.IsNull() && !prior.IsUnknown() {
		priorAttrs = prior.Attributes()
	}
	priorRules, _ := priorAttrs["rules"].(types.List)
	priorUp, _ := priorAttrs["scale_up_window"].(types.String)
	priorDown, _ := priorAttrs["scale_down_window"].(types.String)
	return types.ObjectValueMust(computeAutoscalingAttrTypes, map[string]attr.Value{
		"enabled":                    types.BoolValue(a.GetEnabled()),
		"min_tier":                   optionalString(a.GetMinTier()),
		"max_tier":                   optionalString(a.GetMaxTier()),
		"rules":                      computeRulesFromProto(a.GetRules(), priorRules),
		"scale_up_window":            windowFromProto(a.GetScaleUpWindow(), priorUp),
		"scale_down_window":          windowFromProto(a.GetScaleDownWindow(), priorDown),
		"respect_maintenance_window": types.BoolValue(a.GetRespectMaintenanceWindow()),
	})
}

func computeRulesFromProto(rules []*serverv1.ComputeRule, prior types.List) types.List {
	if len(rules) == 0 && (prior.IsNull() || prior.IsUnknown()) {
		return types.ListNull(computeRuleType)
	}
	elems := make([]attr.Value, 0, len(rules))
	for _, r := range rules {
		elems = append(elems, types.ObjectValueMust(computeRuleAttrTypes, map[string]attr.Value{
			"metric":           types.StringValue(r.GetMetric()),
			"scale_up_above":   types.Int64Value(r.GetScaleUpAbove()),
			"scale_down_below": types.Int64Value(r.GetScaleDownBelow()),
		}))
	}
	return types.ListValueMust(computeRuleType, elems)
}

func windowFromProto(d *durationpb.Duration, prior types.String) types.String {
	if d == nil {
		return types.StringNull()
	}
	if configured, ok := parseWindow(prior); ok && configured == d.AsDuration() {
		return prior
	}
	return types.StringValue(formatLifetime(d.AsDuration()))
}

func currentTierFromProto(compute []*serverv1.NodeGroupCompute, group string) types.String {
	for _, c := range compute {
		if c.GetNodeGroup() == group {
			return optionalString(c.GetTier())
		}
	}
	return types.StringNull()
}

func autoscalingEnabled(o types.Object) bool {
	if o.IsNull() || o.IsUnknown() {
		return false
	}
	enabled := o.Attributes()["enabled"].(types.Bool)
	return enabled.IsUnknown() || enabled.ValueBool()
}

func validateComputeAutoscaling(nodes types.Map, diags *diag.Diagnostics) {
	if nodes.IsNull() || nodes.IsUnknown() {
		return
	}
	for label, element := range nodes.Elements() {
		group, _ := element.(types.Object)
		autoscaling, _ := group.Attributes()["autoscaling"].(types.Object)
		if autoscaling.IsNull() || autoscaling.IsUnknown() {
			continue
		}
		at := path.Root("nodes").AtMapKey(label).AtName("autoscaling")
		attrs := autoscaling.Attributes()
		if enabled, _ := attrs["enabled"].(types.Bool); enabled.ValueBool() {
			for _, bound := range []string{"min_tier", "max_tier"} {
				if v, _ := attrs[bound].(types.String); v.IsNull() {
					diags.AddAttributeError(at.AtName(bound), "Missing "+bound,
						"Compute autoscaling moves the group between min_tier and max_tier; set both when enabled is true.")
				}
			}
		}
		rules, _ := attrs["rules"].(types.List)
		validateComputeRules(rules, at.AtName("rules"), diags)
	}
}

func validateComputeRules(rules types.List, at path.Path, diags *diag.Diagnostics) {
	seen := map[string]bool{}
	for i, element := range rules.Elements() {
		rule, _ := element.(types.Object)
		attrs := rule.Attributes()
		metric, _ := attrs["metric"].(types.String)
		up, _ := attrs["scale_up_above"].(types.Int64)
		down, _ := attrs["scale_down_below"].(types.Int64)
		if known(metric) {
			if seen[metric.ValueString()] {
				diags.AddAttributeError(at.AtListIndex(i).AtName("metric"), "Duplicate metric",
					"Metric "+metric.ValueString()+" appears in more than one rule; give each metric one rule.")
			}
			seen[metric.ValueString()] = true
		}
		if known(up) && known(down) && down.ValueInt64() >= up.ValueInt64() {
			diags.AddAttributeError(at.AtListIndex(i).AtName("scale_down_below"), "scale_down_below is not below scale_up_above",
				"A rule needs a gap between its thresholds, or the group would move up and down on the same value: "+
					down.String()+" must be lower than "+up.String()+".")
		}
	}
}

type nodeGroup[T any] interface {
	spec() T
	released() (T, bool)
}

func (g nodeGroupModel) spec() nodeGroupModel {
	g.CurrentTier = types.StringNull()
	return g
}

func (g nodeGroupModel) released() (nodeGroupModel, bool) {
	if !autoscalingEnabled(g.Autoscaling) {
		return g, false
	}
	g.CurrentTier = types.StringUnknown()
	return g, true
}

func (g valkeyNodeGroupModel) spec() valkeyNodeGroupModel {
	g.CurrentTier = types.StringNull()
	return g
}

func (g valkeyNodeGroupModel) released() (valkeyNodeGroupModel, bool) {
	if !autoscalingEnabled(g.Autoscaling) {
		return g, false
	}
	g.CurrentTier = types.StringUnknown()
	return g, true
}

func nodeSpecs[T nodeGroup[T]](nodes map[string]T) map[string]T {
	out := make(map[string]T, len(nodes))
	for name, g := range nodes {
		out[name] = g.spec()
	}
	return out
}

func releaseCurrentTiers[T nodeGroup[T]](nodes map[string]T) bool {
	moved := false
	for name, g := range nodes {
		if released, ok := g.released(); ok {
			nodes[name] = released
			moved = true
		}
	}
	return moved
}
