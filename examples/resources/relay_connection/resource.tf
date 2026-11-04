resource "relay_connection" "orders" {
  workspace_id        = relay_workspace.prod.id
  name                = "orders"
  engine              = "postgres"
  host                = "orders.internal:5432"
  agent_pool          = "default"
  user                = "app"
  database            = "orders"
  credential_ref      = "secret/data/orders#password"
  credential_source   = "vault-prod"
  deletion_protection = true

  redact_enabled = true
  redact_rules = [
    { column_pattern = "email", strategy = "hash" },
  ]
}

resource "relay_connection" "account_rw" {
  workspace_id        = relay_workspace.prod.id
  name                = "account-rw"
  engine              = "postgres"
  agent_pool          = "default"
  managed_database_id = relay_postgresql_cluster.billing.id
  user                = "usr_account_rw"
  database            = "account"
}
