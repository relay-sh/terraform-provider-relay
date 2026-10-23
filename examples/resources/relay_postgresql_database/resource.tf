resource "relay_postgresql_database" "billing" {
  workspace_id = relay_postgresql_cluster.billing.workspace_id
  cluster_id   = relay_postgresql_cluster.billing.id
  name         = "billing"
  description  = "Invoices and payments"
}
