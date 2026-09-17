data "relay_postgresql_cluster" "billing" {
  workspace_id = data.relay_workspace.prod.id
  name         = "billing"
}
