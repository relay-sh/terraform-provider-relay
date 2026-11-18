data "relay_documentdb_cluster" "catalog" {
  workspace_id = data.relay_workspace.prod.id
  name         = "catalog"
}
