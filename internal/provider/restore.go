package provider

import (
	"context"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"google.golang.org/protobuf/types/known/timestamppb"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"

	"github.com/relay-sh/terraform-provider-relay/internal/client"
)

func restoreAttribute(fixed, targetTime string) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Creates the cluster from another cluster's backups instead of empty. The platform picks the backup and starts the cluster with the " +
			"source's settings; the provider then applies the configured nodes and settings. " + fixed + " Only read on create: changing it replaces the " +
			"cluster, while adding it to or removing it from an existing cluster changes nothing.",
		Attributes: map[string]schema.Attribute{
			"source_database_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "ID of the cluster to restore from, in the same workspace and with backups. For a destroyed cluster whose backups were kept, " +
					"the ID it had.",
			},
			"target_time": schema.StringAttribute{Optional: true, MarkdownDescription: targetTime},
		},
	}
}

func restoreChanged(plan, prior types.Object) bool {
	return !plan.IsNull() && !prior.IsNull() && !plan.Equal(prior)
}

func keepEquivalentRestore(plan, prior types.Object) types.Object {
	if !known(plan) || !known(prior) || !plan.Attributes()["source_database_id"].Equal(prior.Attributes()["source_database_id"]) {
		return plan
	}
	planned, perr := time.Parse(time.RFC3339, restoreTargetTime(plan).ValueString())
	stored, serr := time.Parse(time.RFC3339, restoreTargetTime(prior).ValueString())
	if perr != nil || serr != nil || !planned.Equal(stored) {
		return plan
	}
	return prior
}

func restoreTargetTime(restore types.Object) types.String {
	if !known(restore) {
		return types.StringNull()
	}
	return restore.Attributes()["target_time"].(types.String)
}

func restoreRequest(name string, restore types.Object) *serverv1.RestoreDatabaseRequest {
	req := &serverv1.RestoreDatabaseRequest{Name: name, SourceDatabaseId: restore.Attributes()["source_database_id"].(types.String).ValueString()}
	if t, err := time.Parse(time.RFC3339, restoreTargetTime(restore).ValueString()); err == nil {
		req.TargetTime = timestamppb.New(t)
	}
	return req
}

func checkRestorePlan(ctx context.Context, c *client.Client, workspaceID, version types.String, restore types.Object, engine string, diags *diag.Diagnostics) *serverv1.DatabaseSpec {
	if !known(restore) {
		return nil
	}
	if target := restoreTargetTime(restore); known(target) {
		at := path.Root("restore").AtName("target_time")
		if t, err := time.Parse(time.RFC3339, target.ValueString()); err != nil {
			diags.AddAttributeError(at, "Invalid target_time", "target_time must be an RFC 3339 timestamp such as 2026-10-31T12:00:00Z, got "+target.ValueString()+".")
		} else if t.After(time.Now()) {
			diags.AddAttributeError(at, "target_time is in the future", "A restore recovers up to a past instant; "+target.ValueString()+" has not happened yet.")
		}
	}
	source := restore.Attributes()["source_database_id"].(types.String)
	if !known(source) || !known(workspaceID) {
		return nil
	}
	out, err := c.API.GetDatabase(c.Workspace(ctx, workspaceID.ValueString()), &serverv1.GetDatabaseRequest{Id: source.ValueString()})
	if err != nil {
		return nil
	}
	spec := out.GetDatabase().GetSpec()
	at := path.Root("restore").AtName("source_database_id")
	switch {
	case !strings.EqualFold(spec.GetEngine(), engine):
		diags.AddAttributeError(at, "Source is another engine", "Cluster "+source.ValueString()+" is a "+spec.GetEngine()+" cluster; a restore stays on the source's engine.")
		return nil
	case !spec.GetBackupsEnabled():
		diags.AddAttributeError(at, "Source has no backups", "Cluster "+source.ValueString()+" does not have backups enabled, so there is nothing to restore from.")
		return nil
	}
	if known(version) && version.ValueString() != spec.GetVersion() {
		diags.AddAttributeError(path.Root("version"), "Version differs from the source",
			"The source runs "+spec.GetVersion()+" and a restore starts on it. Omit version or set it to "+spec.GetVersion()+".")
	}
	return spec
}

func checkRestoreLabel(nodes types.Map, label string, diags *diag.Diagnostics) {
	if !known(nodes) {
		return
	}
	if _, ok := nodes.Elements()[label]; !ok {
		diags.AddAttributeError(path.Root("nodes"), "Missing the source's node group",
			"A restore keeps the source's node group labels, and the source's primary group is "+label+". Add or rename a group to "+label+".")
	}
}

func driftedRoots(plan tfsdk.Plan, state tfsdk.State, roots []string) []string {
	var out []string
	for _, root := range roots {
		p, _ := plan.Raw.ApplyTerraform5AttributePathStep(tftypes.AttributeName(root))
		s, _ := state.Raw.ApplyTerraform5AttributePathStep(tftypes.AttributeName(root))
		if !satisfies(p.(tftypes.Value), s.(tftypes.Value)) {
			out = append(out, root)
		}
	}
	return out
}

func satisfies(planned, got tftypes.Value) bool {
	if !planned.IsKnown() {
		return true
	}
	var p, g map[string]tftypes.Value
	if planned.IsNull() || planned.As(&p) != nil || got.As(&g) != nil {
		return planned.Equal(got)
	}
	if len(p) != len(g) {
		return false
	}
	for k, v := range p {
		w, ok := g[k]
		if !ok || !satisfies(v, w) {
			return false
		}
	}
	return true
}
