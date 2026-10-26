resource "relay_valkey_user" "app" {
  workspace_id      = relay_workspace.prod.id
  cluster_id        = relay_valkey_cluster.sessions.id
  username          = "app"
  role              = "readWrite"
  generate_password = true
}

output "app_password" {
  value     = relay_valkey_user.app.generated_password
  sensitive = true
}
