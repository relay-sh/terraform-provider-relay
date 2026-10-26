package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
	"github.com/relay-sh/relay-go/relay"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

const (
	errorCodeTrailer = "x-relay-error-code"
	planLimitReached = "PLAN_LIMIT_REACHED"
)

type backupScheduleResource struct {
	client *client.Client
}

type backupScheduleModel struct {
	ID              types.String `tfsdk:"id"`
	WorkspaceID     types.String `tfsdk:"workspace_id"`
	ClusterID       types.String `tfsdk:"cluster_id"`
	SnapshotTime    types.Object `tfsdk:"snapshot_time"`
	Tiers           types.List   `tfsdk:"tiers"`
	MaxBackups      types.Int64  `tfsdk:"max_backups"`
	PITRWindowDays  types.Int64  `tfsdk:"pitr_window_days"`
	PlatformDefault types.Bool   `tfsdk:"platform_default"`
	LastBackupTime  types.String `tfsdk:"last_backup_time"`
	NextBackupTime  types.String `tfsdk:"next_backup_time"`
	ActiveBackups   types.Int64  `tfsdk:"active_backups"`
}

type tierRule struct {
	unit     string
	interval func(int64) bool
	hint     string
}

var (
	snapshotTimeAttrTypes = map[string]attr.Type{"hour": types.Int64Type, "minute": types.Int64Type}
	tierAttrTypes         = map[string]attr.Type{
		"frequency":       types.StringType,
		"interval":        types.Int64Type,
		"retention_value": types.Int64Type,
		"retention_unit":  types.StringType,
		"enabled":         types.BoolType,
	}
	tierRules = map[string]tierRule{
		"hourly":  {unit: "days", interval: func(i int64) bool { return i >= 1 && i <= 24 && 24%i == 0 }, hint: "a divisor of 24"},
		"daily":   {unit: "days", interval: func(int64) bool { return true }},
		"weekly":  {unit: "weeks", interval: func(i int64) bool { return i >= 1 && i <= 7 }, hint: "a weekday from 1 (Sunday) to 7 (Saturday)"},
		"monthly": {unit: "months", interval: func(i int64) bool { return (i >= 1 && i <= 31) || i == 40 }, hint: "a day of the month from 1 to 31, or 40 for the last day"},
	}
)

func newBackupScheduleResource() resource.Resource {
	return &backupScheduleResource{}
}

func (r *backupScheduleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_backup_schedule"
}

func (r *backupScheduleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keep := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "The backup schedule of a managed PostgreSQL or Valkey cluster. Every apply replaces the whole schedule, so an attribute left out takes the platform's value. " +
			"Destroying the resource resets the cluster to the platform default schedule; it does not turn backups off. " +
			"Only the attributes set in the configuration are tracked for drift. " +
			"The cluster must have backups enabled. A cluster still on the platform default has no schedule to import.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, PlanModifiers: keep, MarkdownDescription: "The cluster ID."},
			"workspace_id": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "Workspace of the cluster."},
			"cluster_id":   schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "`relay_postgresql_cluster` or `relay_valkey_cluster` ID."},
			"snapshot_time": schema.SingleNestedAttribute{
				Optional: true, Computed: true,
				Default: objectdefault.StaticValue(types.ObjectValueMust(snapshotTimeAttrTypes, map[string]attr.Value{
					"hour": types.Int64Value(2), "minute": types.Int64Value(0),
				})),
				MarkdownDescription: "UTC wall-clock time the daily, weekly and monthly tiers fire at. Defaults to 02:00.",
				Attributes: map[string]schema.Attribute{
					"hour":   schema.Int64Attribute{Required: true, Validators: []validator.Int64{int64validator.Between(0, 23)}, MarkdownDescription: "Hour, 0 to 23."},
					"minute": schema.Int64Attribute{Required: true, Validators: []validator.Int64{int64validator.Between(0, 59)}, MarkdownDescription: "Minute, 0 to 59."},
				},
			},
			"tiers": schema.ListNestedAttribute{
				Optional:   true,
				Validators: []validator.List{listvalidator.SizeAtLeast(1)},
				MarkdownDescription: "Retention ladder, at most one tier per frequency. Every enabled tier is rendered into the cluster's backup crons and retention counts; " +
					"the weekly tier takes the full backup and the daily tier the incremental one. Left out, the cluster takes a weekly full and a daily incremental backup, each kept `max_backups` times.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"frequency": schema.StringAttribute{
							Required:            true,
							Validators:          []validator.String{stringvalidator.OneOf("hourly", "daily", "weekly", "monthly")},
							MarkdownDescription: "`hourly`, `daily`, `weekly` or `monthly`.",
						},
						"interval": schema.Int64Attribute{
							Required: true,
							MarkdownDescription: "For `hourly`, the spacing in hours, a divisor of 24. For `weekly`, the weekday, 1 (Sunday) to 7 (Saturday). " +
								"For `monthly`, the day of the month, 1 to 31, or 40 for the last day, which currently runs on the 28th. Ignored for `daily`.",
						},
						"retention_value": schema.Int64Attribute{Required: true, MarkdownDescription: "How many backups of this tier are kept, at least 1."},
						"retention_unit": schema.StringAttribute{
							Optional:            true,
							Validators:          []validator.String{stringvalidator.OneOf("days", "weeks", "months")},
							MarkdownDescription: "Implied by the frequency: `days` for hourly and daily, `weeks` for weekly, `months` for monthly.",
						},
						"enabled": schema.BoolAttribute{
							Optional:            true,
							MarkdownDescription: "Defaults to true. A disabled tier is stored but takes no backups.",
						},
					},
				},
			},
			"max_backups": schema.Int64Attribute{
				Optional:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1), int64validator.ConflictsWith(path.MatchRoot("tiers"))},
				MarkdownDescription: "Full backups kept when `tiers` is not set; the platform default is 4. With tiers, the weekly tier's retention decides and the API ignores this value, so the two conflict.",
			},
			"pitr_window_days": schema.Int64Attribute{
				Optional:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				MarkdownDescription: "Requested point-in-time-restore window in days, PostgreSQL only; the platform default is 7. Valkey restores are snapshot-only, so a Valkey cluster rejects it.",
			},
			"platform_default": schema.BoolAttribute{
				Computed: true, PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Always false while Terraform manages the schedule. A schedule reset to the default outside Terraform is removed from state.",
			},
			"last_backup_time": schema.StringAttribute{Computed: true, MarkdownDescription: "Completion time of the last backup, RFC 3339, empty before the first one."},
			"next_backup_time": schema.StringAttribute{Computed: true, MarkdownDescription: "Next scheduled backup, RFC 3339."},
			"active_backups":   schema.Int64Attribute{Computed: true, MarkdownDescription: "Backups currently retained."},
		},
	}
}

func (r *backupScheduleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var tiers types.List
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("tiers"), &tiers)...)
	seen := map[string]bool{}
	for i, elem := range tiers.Elements() {
		if obj := elem.(types.Object); known(obj) {
			validateTier(path.Root("tiers").AtListIndex(i), obj.Attributes(), seen, &resp.Diagnostics)
		}
	}
}

func validateTier(at path.Path, attrs map[string]attr.Value, seen map[string]bool, diags *diag.Diagnostics) {
	frequency := attrs["frequency"].(types.String)
	rule, ok := tierRules[frequency.ValueString()]
	if !ok {
		return
	}
	if seen[frequency.ValueString()] {
		diags.AddAttributeError(at.AtName("frequency"), "Duplicate tier", "There can be only one "+frequency.ValueString()+" tier.")
	}
	seen[frequency.ValueString()] = true
	if enabled := attrs["enabled"].(types.Bool); !enabled.IsNull() && !enabled.IsUnknown() && !enabled.ValueBool() {
		return
	}
	if interval := attrs["interval"].(types.Int64); !interval.IsUnknown() && !rule.interval(interval.ValueInt64()) {
		diags.AddAttributeError(at.AtName("interval"), "Invalid interval", fmt.Sprintf("The %s interval must be %s, got %d.", frequency.ValueString(), rule.hint, interval.ValueInt64()))
	}
	if retention := attrs["retention_value"].(types.Int64); !retention.IsUnknown() && retention.ValueInt64() < 1 {
		diags.AddAttributeError(at.AtName("retention_value"), "Invalid retention", "An enabled tier keeps at least 1 backup.")
	}
	if unit := attrs["retention_unit"].(types.String); !unit.IsNull() && !unit.IsUnknown() && unit.ValueString() != rule.unit {
		diags.AddAttributeError(at.AtName("retention_unit"), "Invalid retention unit", fmt.Sprintf("The %s tier is counted in %s, got %s.", frequency.ValueString(), rule.unit, unit.ValueString()))
	}
}

func (r *backupScheduleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (r *backupScheduleResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan backupScheduleModel
	var pitr types.Int64
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("pitr_window_days"), &pitr)...)
	if resp.Diagnostics.HasError() || !known(pitr) || plan.WorkspaceID.IsUnknown() || plan.ClusterID.IsUnknown() {
		return
	}
	clusterID := plan.ClusterID
	out, err := r.client.API.GetDatabase(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), &serverv1.GetDatabaseRequest{Id: clusterID.ValueString()})
	if err != nil {
		return
	}
	if strings.EqualFold(out.GetDatabase().GetSpec().GetEngine(), engineValkey) {
		resp.Diagnostics.AddAttributeError(path.Root("pitr_window_days"), "No point-in-time restore on Valkey",
			"Cluster "+clusterID.ValueString()+" is a Valkey cluster, whose restores are snapshot-only. Remove pitr_window_days.")
	}
}

func (r *backupScheduleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan backupScheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.upsert(ctx, plan, &resp.State, &resp.Diagnostics)
}

func (r *backupScheduleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state backupScheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.GetBackupSchedule(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.GetBackupScheduleRequest{DatabaseId: state.ClusterID.ValueString()})
	if relay.IsNotFound(err) || (err == nil && out.GetBackupSchedule().GetPlatformDefault()) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read backup schedule", err.Error())
		return
	}
	state = backupScheduleFromProto(out.GetBackupSchedule(), state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *backupScheduleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan backupScheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.upsert(ctx, plan, &resp.State, &resp.Diagnostics)
}

func (r *backupScheduleResource) upsert(ctx context.Context, plan backupScheduleModel, state *tfsdk.State, diags *diag.Diagnostics) {
	var trailer metadata.MD
	out, err := r.client.API.UpsertBackupSchedule(r.client.Workspace(ctx, plan.WorkspaceID.ValueString()), &serverv1.UpsertBackupScheduleRequest{
		DatabaseId: plan.ClusterID.ValueString(),
		Spec:       backupScheduleSpecToProto(plan),
	}, grpc.Trailer(&trailer))
	if err != nil {
		if slices.Contains(trailer.Get(errorCodeTrailer), planLimitReached) {
			diags.AddError("Backup retention exceeds the plan", status.Convert(err).Message()+"\n\nShorten the tiers' retention, max_backups or pitr_window_days, or upgrade the organization's plan.")
			return
		}
		diags.AddError("Cannot set backup schedule", err.Error())
		return
	}
	plan = backupScheduleFromProto(out.GetBackupSchedule(), plan)
	diags.Append(state.Set(ctx, &plan)...)
}

func (r *backupScheduleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state backupScheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.API.ResetBackupSchedule(r.client.Workspace(ctx, state.WorkspaceID.ValueString()), &serverv1.ResetBackupScheduleRequest{DatabaseId: state.ClusterID.ValueString()})
	if err != nil && !relay.IsNotFound(err) {
		resp.Diagnostics.AddError("Cannot reset backup schedule", err.Error())
	}
}

func (r *backupScheduleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <workspace_id>/<cluster_id>, got "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workspace_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[1])...)
}

func backupScheduleSpecToProto(m backupScheduleModel) *serverv1.BackupScheduleSpec {
	spec := &serverv1.BackupScheduleSpec{}
	if known(m.SnapshotTime) {
		attrs := m.SnapshotTime.Attributes()
		spec.SnapshotTime = &serverv1.SnapshotTime{
			Hour:   int32(attrs["hour"].(types.Int64).ValueInt64()),
			Minute: int32(attrs["minute"].(types.Int64).ValueInt64()),
		}
	}
	if known(m.Tiers) {
		for _, elem := range m.Tiers.Elements() {
			attrs := elem.(types.Object).Attributes()
			frequency := attrs["frequency"].(types.String).ValueString()
			unit := attrs["retention_unit"].(types.String)
			enabled := attrs["enabled"].(types.Bool)
			tier := &serverv1.PolicyTier{
				Frequency:      frequency,
				Interval:       int32(attrs["interval"].(types.Int64).ValueInt64()),
				RetentionValue: int32(attrs["retention_value"].(types.Int64).ValueInt64()),
				RetentionUnit:  unit.ValueString(),
				Enabled:        !known(enabled) || enabled.ValueBool(),
			}
			if !known(unit) {
				tier.RetentionUnit = tierRules[frequency].unit
			}
			spec.PolicyTiers = append(spec.PolicyTiers, tier)
		}
	}
	if known(m.MaxBackups) {
		spec.Retention = &serverv1.RetentionPolicy{MaxBackups: int32(m.MaxBackups.ValueInt64())}
	}
	if known(m.PITRWindowDays) {
		spec.PitrWindowDays = int32(m.PITRWindowDays.ValueInt64())
	}
	return spec
}

func known(v attr.Value) bool {
	return !v.IsNull() && !v.IsUnknown()
}

func backupScheduleFromProto(s *serverv1.BackupSchedule, prior backupScheduleModel) backupScheduleModel {
	out := prior
	out.ID = prior.ClusterID
	spec := s.GetSpec()
	out.SnapshotTime = types.ObjectValueMust(snapshotTimeAttrTypes, map[string]attr.Value{
		"hour":   types.Int64Value(int64(spec.GetSnapshotTime().GetHour())),
		"minute": types.Int64Value(int64(spec.GetSnapshotTime().GetMinute())),
	})
	priorTiers := prior.Tiers.Elements()
	tiers := make([]attr.Value, 0, len(spec.GetPolicyTiers()))
	for i, t := range spec.GetPolicyTiers() {
		implied := tierRules[t.GetFrequency()].unit
		unit, enabled := types.StringValue(t.GetRetentionUnit()), types.BoolValue(t.GetEnabled())
		if t.GetRetentionUnit() == "" {
			unit = types.StringValue(implied)
		}
		if i < len(priorTiers) {
			configured := priorTiers[i].(types.Object).Attributes()
			if configured["retention_unit"].IsNull() && unit.ValueString() == implied {
				unit = types.StringNull()
			}
			if configured["enabled"].IsNull() && t.GetEnabled() {
				enabled = types.BoolNull()
			}
		}
		tiers = append(tiers, types.ObjectValueMust(tierAttrTypes, map[string]attr.Value{
			"frequency":       types.StringValue(t.GetFrequency()),
			"interval":        types.Int64Value(int64(t.GetInterval())),
			"retention_value": types.Int64Value(int64(t.GetRetentionValue())),
			"retention_unit":  unit,
			"enabled":         enabled,
		}))
	}
	imported := prior.PlatformDefault.IsNull()
	if imported || !prior.Tiers.IsNull() {
		out.Tiers = types.ListValueMust(types.ObjectType{AttrTypes: tierAttrTypes}, tiers)
	}
	if imported && len(tiers) == 0 && spec.GetRetention().GetMaxBackups() > 0 || !prior.MaxBackups.IsNull() {
		out.MaxBackups = types.Int64Value(int64(spec.GetRetention().GetMaxBackups()))
	}
	if imported && spec.GetPitrWindowDays() > 0 || !prior.PITRWindowDays.IsNull() {
		out.PITRWindowDays = types.Int64Value(int64(spec.GetPitrWindowDays()))
	}
	out.PlatformDefault = types.BoolValue(s.GetPlatformDefault())
	out.LastBackupTime = timestampValue(s.GetStatus().GetLastBackupTime())
	out.NextBackupTime = timestampValue(s.GetStatus().GetNextBackupTime())
	out.ActiveBackups = types.Int64Value(int64(s.GetStatus().GetActiveBackups()))
	return out
}
