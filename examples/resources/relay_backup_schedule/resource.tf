resource "relay_backup_schedule" "orders" {
  workspace_id     = relay_workspace.prod.id
  cluster_id       = relay_postgresql_cluster.orders.id
  snapshot_time    = { hour = 3, minute = 30 }
  pitr_window_days = 14

  tiers = [
    { frequency = "hourly", interval = 6, retention_value = 8 },
    { frequency = "daily", interval = 1, retention_value = 7 },
    { frequency = "weekly", interval = 1, retention_value = 4 },
    { frequency = "monthly", interval = 1, retention_value = 12 },
  ]
}

resource "relay_backup_schedule" "sessions" {
  workspace_id  = relay_workspace.prod.id
  cluster_id    = relay_valkey_cluster.sessions.id
  snapshot_time = { hour = 4, minute = 0 }
  max_backups   = 7
}
