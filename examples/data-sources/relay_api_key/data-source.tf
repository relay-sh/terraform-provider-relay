data "relay_api_key" "deployer" {
  name         = "deployer"
  workspace_id = data.relay_workspace.prod.id
}
