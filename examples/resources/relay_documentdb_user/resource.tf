resource "relay_documentdb_user" "catalog_api" {
  workspace_id      = relay_workspace.prod.id
  cluster_id        = relay_documentdb_cluster.catalog.id
  username          = "catalog_api"
  role              = "admin"
  generate_password = true
}

resource "relay_documentdb_user" "reports" {
  workspace_id = relay_workspace.prod.id
  cluster_id   = relay_documentdb_cluster.catalog.id
  username     = "reports"
  role         = "readOnly"
  password     = var.reports_password
}

output "catalog_api_uri" {
  value     = "mongodb://${relay_documentdb_user.catalog_api.username}:${urlencode(relay_documentdb_user.catalog_api.generated_password)}@${relay_documentdb_cluster.catalog.endpoint_host}:${relay_documentdb_cluster.catalog.endpoint_port}/?tls=true"
  sensitive = true
}
