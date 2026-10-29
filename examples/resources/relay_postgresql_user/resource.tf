resource "relay_postgresql_database" "billing" {
  workspace_id = relay_postgresql_cluster.billing.workspace_id
  cluster_id   = relay_postgresql_cluster.billing.id
  name         = "billing"
}

resource "relay_postgresql_user" "app" {
  workspace_id      = relay_postgresql_database.billing.workspace_id
  cluster_id        = relay_postgresql_database.billing.cluster_id
  username          = "billing_app"
  generate_password = true
  roles             = [{ role = "readWrite", database = relay_postgresql_database.billing.name }]
  connection_limit  = 50
  statement_timeout = "30s"
}

output "app_password" {
  value     = relay_postgresql_user.app.generated_password
  sensitive = true
}

resource "relay_postgresql_user" "migration" {
  workspace_id      = relay_postgresql_database.balance.workspace_id
  cluster_id        = relay_postgresql_database.balance.cluster_id
  username          = "balance_migration"
  generate_password = true
  roles             = [{ role = "dbAdmin", database = relay_postgresql_database.balance.name }]
}

resource "relay_postgresql_user" "rw" {
  workspace_id      = relay_postgresql_database.balance.workspace_id
  cluster_id        = relay_postgresql_database.balance.cluster_id
  username          = "balance_rw"
  generate_password = true
  roles             = [{ role = "readWrite", database = relay_postgresql_database.balance.name }]
}

resource "relay_postgresql_user" "ro" {
  workspace_id      = relay_postgresql_database.balance.workspace_id
  cluster_id        = relay_postgresql_database.balance.cluster_id
  username          = "balance_ro"
  generate_password = true
  roles             = [{ role = "readOnly", database = relay_postgresql_database.balance.name, schema = "balance" }]
}
