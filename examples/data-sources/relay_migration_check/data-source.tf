variable "source_database_url" {
  type      = string
  sensitive = true
}

check "orders_database_can_migrate" {
  data "relay_migration_check" "orders" {
    workspace_id   = relay_workspace.prod.id
    cloud          = "aws"
    region         = "us-east-1"
    target_version = "17"
    mode           = "online"

    source = {
      connection_string = var.source_database_url
    }
  }

  assert {
    condition     = !data.relay_migration_check.orders.has_blockers
    error_message = "The orders database cannot be migrated to Relay online: ${join("; ", [for r in data.relay_migration_check.orders.results : r.message if r.level == "blocker"])}"
  }
}
