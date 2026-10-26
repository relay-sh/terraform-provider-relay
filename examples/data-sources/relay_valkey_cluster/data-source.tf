data "relay_valkey_cluster" "sessions" {
  workspace_id = data.relay_workspace.prod.id
  name         = "sessions"
}
