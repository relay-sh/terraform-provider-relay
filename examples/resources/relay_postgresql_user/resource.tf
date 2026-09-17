resource "relay_postgresql_user" "app" {
  workspace_id      = relay_postgresql_cluster.billing.workspace_id
  cluster_id        = relay_postgresql_cluster.billing.id
  username          = "billing_app"
  generate_password = true
  roles             = [{ role = "readWrite", database = "billing" }]
  connection_limit  = 50
  statement_timeout = "30s"
}

output "app_password" {
  value     = relay_postgresql_user.app.generated_password
  sensitive = true
}
