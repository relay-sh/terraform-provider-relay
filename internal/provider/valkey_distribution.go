package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var valkeyDistribution = distributionEngine{
	description: "Spreads the cluster across regions as a standby: one region serves reads and writes and the others replicate from it. " +
		"Only with `mode = \"replication\"`. Add the block when you create the cluster: moving a cluster between a single region and a standby spread is refused by the platform. " +
		"Move the primary by setting `primary = true` on another region and `false` on the current one, in an update that changes nothing else. " +
		"Add or remove one region per update. The platform refuses a promotion while the new primary has not synchronized; " +
		"forcing one can lose data and is not available from Terraform, so use the API, CLI or dashboard.",
	stretched: "Valkey runs across regions only as a standby: use mode = \"standby\".",
}

func validateValkeyDistributionConfig(d types.Object, mode types.String, nodes types.Map, restore types.Object, diags *diag.Diagnostics) {
	if !known(d) {
		return
	}
	root := path.Root("distribution")
	if mode.ValueString() == valkeyModeCluster {
		diags.AddAttributeError(root, "Standby needs replication mode",
			"A standby distribution replicates one primary to the other regions, which only mode \"replication\" does. Remove the distribution block or use mode = \"replication\".")
	}
	if !restore.IsNull() {
		diags.AddAttributeError(root, "No restore into a standby",
			"A backup cannot be restored into a standby distribution. Remove the distribution block to restore the backup into a single-region cluster.")
	}
	validateDistributionRegions(d, nodes, diags)
}
