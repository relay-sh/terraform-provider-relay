data "relay_valkey_cluster" "sessions" {
  workspace_id = data.relay_workspace.prod.id
  name         = "sessions"
}

output "sessions_regions" {
  value = [for r in data.relay_valkey_cluster.sessions.regions : "${r.cloud}/${r.region}: ${r.role}, ${r.phase}"]
}
