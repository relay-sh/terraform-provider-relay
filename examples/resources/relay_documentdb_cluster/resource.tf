resource "relay_documentdb_cluster" "catalog" {
  workspace_id        = relay_workspace.prod.id
  name                = "catalog"
  version             = "18"
  deletion_protection = true

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
