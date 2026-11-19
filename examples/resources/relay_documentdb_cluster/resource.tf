resource "relay_documentdb_cluster" "catalog" {
  workspace_id        = relay_workspace.prod.id
  name                = "catalog"
  version             = "18"
  deletion_protection = true

  backups_enabled         = true
  keep_backups_on_destroy = true

  storage = {
    size = "40Gi"
  }

  nodes = {
    main = {
      count     = 3
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "R20"
      resources = {}
    }
  }

  default_connections = {
    readonly = true
    admin    = false
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "relay_documentdb_cluster" "catalog_copy" {
  workspace_id = relay_workspace.prod.id
  name         = "catalog-copy"

  restore = {
    source_database_id = relay_documentdb_cluster.catalog.id
    target_time        = "2026-10-31T12:00:00Z"
  }

  nodes = {
    main = {
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "R20"
      resources = {}
    }
  }
}
