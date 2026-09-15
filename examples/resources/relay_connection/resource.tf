resource "relay_connection" "orders" {
  workspace_id      = relay_workspace.prod.id
  name              = "orders"
  engine            = "postgres"
  host              = "orders.internal:5432"
  agent_pool        = "default"
  user              = "app"
  database          = "orders"
  credential_ref    = "secret/data/orders#password"
  credential_source = "vault-prod"

  redact_enabled = true
  redact_rules = [
    { column_pattern = "email", strategy = "hash" },
  ]
}
