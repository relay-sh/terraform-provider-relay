package provider

import (
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var postgresqlDistribution = distributionEngine{
	description: "Spreads the cluster across regions as a standby: one region serves reads and writes, and every other region runs a full copy of the cluster " +
		"that replicates from it and can be promoted. Add the block when you create the cluster: the platform cannot spread an existing cluster, " +
		"and a cluster restored from a backup cannot have it. " +
		"The regions share one credentials Secret, so users and passwords are the same in all of them. " +
		"Move the primary by setting `primary = true` on another region and `false` on the current one. " +
		"Add or remove one region per update, with nothing else changed in it and without moving the primary. " +
		"The primary region and the region the node groups run in cannot be removed. " +
		"The platform refuses a promotion while the new primary has not synchronized; " +
		"forcing one can lose data and is not available from Terraform, so use the API, CLI or dashboard. " +
		"This is unrelated to a node group's `role = \"standby\"`. " +
		"The `stretched` mode, one cluster across regions on a shared etcd, is not available from Terraform.",
	stretched: "A stretched distribution runs one PostgreSQL cluster across regions on a shared etcd, with a member count per region " +
		"and optionally a priority per region. Terraform does not model those, and sending the mode without them would be refused or half applied. " +
		"Use mode = \"standby\", or manage a stretched cluster with the API, CLI or dashboard.",
}

func validatePostgreSQLDistributionConfig(d types.Object, nodes types.Map, restore types.Object, diags *diag.Diagnostics) {
	if !known(d) {
		return
	}
	if !restore.IsNull() {
		diags.AddAttributeError(path.Root("distribution"), "No restore into a standby",
			"A restore creates a single-region cluster, and the platform cannot spread an existing cluster across regions afterwards. "+
				"Remove the distribution block to restore the backup.")
	}
	validateDistributionRegions(d, nodes, diags)
}

func rejectDistributionChange(plan, prior types.Object, diags *diag.Diagnostics) {
	root := path.Root("distribution")
	switch {
	case plan.IsUnknown() || plan.IsNull() && prior.IsNull():
	case prior.IsNull():
		diags.AddAttributeError(root, "Distribution cannot be added",
			"The platform cannot spread an existing PostgreSQL cluster across regions. Create the cluster with the distribution block, or remove the block.")
	case plan.IsNull():
		diags.AddAttributeError(root, "Distribution cannot be removed",
			"The platform cannot turn a PostgreSQL cluster spread across regions back into a single-region one. Keep the distribution block.")
	}
}

func rejectGroupMove(plan, prior map[string]nodeGroupModel, distribution types.Object, diags *diag.Diagnostics) {
	if distribution.IsNull() {
		return
	}
	for label, planned := range plan {
		current, ok := prior[label]
		if !ok || planned.Cloud.IsUnknown() || planned.Region.IsUnknown() {
			continue
		}
		if !strings.EqualFold(planned.Cloud.ValueString(), current.Cloud.ValueString()) || !strings.EqualFold(planned.Region.ValueString(), current.Region.ValueString()) {
			diags.AddAttributeError(path.Root("nodes").AtMapKey(label), "Node group cannot move",
				"The platform cannot move a node group of a cluster that is spread across regions. "+
					"To change where the primary runs, move `primary` in the distribution instead.")
		}
	}
}
