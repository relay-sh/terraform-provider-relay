resource "relay_database_user" "app" {
  workspace_id      = relay_database.billing.workspace_id
  database_id       = relay_database.billing.id
  username          = "app"
  generate_password = true
  roles             = [{ role = "readWrite", db = "billing" }]
  connection_limit  = 50
}

output "app_password" {
  value     = relay_database_user.app.generated_password
  sensitive = true
}
