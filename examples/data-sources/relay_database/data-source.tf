data "relay_database" "billing" {
  workspace_id = data.relay_workspace.prod.id
  name         = "billing"
}
