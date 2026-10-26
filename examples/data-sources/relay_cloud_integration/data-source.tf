data "relay_cloud_integration" "backups" {
  workspace_id = data.relay_workspace.prod.id
  name         = "backups"
}
