data "relay_postgresql_cluster" "billing" {
  workspace_id = data.relay_workspace.prod.id
  name         = "billing"
}

output "billing_regions" {
  value = [for r in data.relay_postgresql_cluster.billing.regions : "${r.cloud}/${r.region}: ${r.role}, ${r.phase}"]
}
