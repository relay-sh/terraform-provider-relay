data "relay_cloud_integration" "backups" {
  workspace_id = relay_workspace.prod.id
  name         = "backups"
}

resource "relay_backup_auto_export" "prod" {
  workspace_id   = relay_workspace.prod.id
  integration_id = data.relay_cloud_integration.backups.id
  bucket         = "acme-relay-backups"
  prefix         = "prod/"
  which          = "scheduled"
  backfill       = true

  scope = {
    mode = "selected"
    ids  = [relay_postgresql_cluster.orders.id, relay_valkey_cluster.sessions.id]
  }
}
