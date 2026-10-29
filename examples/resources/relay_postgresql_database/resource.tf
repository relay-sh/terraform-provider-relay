resource "relay_postgresql_database" "billing" {
  workspace_id = relay_postgresql_cluster.billing.workspace_id
  cluster_id   = relay_postgresql_cluster.billing.id
  name         = "billing"
  description  = "Invoices and payments"
}

resource "relay_postgresql_database" "balance" {
  workspace_id = relay_postgresql_cluster.billing.workspace_id
  cluster_id   = relay_postgresql_cluster.billing.id
  name         = "balance"
  schemas      = ["balance"]
  extensions   = [{ name = "pg_trgm", schema = "balance" }]
  lock_public  = true
}
