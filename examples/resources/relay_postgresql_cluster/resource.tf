resource "relay_postgresql_cluster" "billing" {
  workspace_id = relay_workspace.prod.id
  name         = "billing"
  version      = "18"

  nodes = {
    writers = {
      role   = "electable"
      count  = 3
      cloud  = "aws"
      region = "us-east-1"
      tier   = "R30"
      resources = {
        storage = "100Gi"
        storage_autoscale = {
          enabled     = true
          max_storage = "500Gi"
        }
      }
    }

    readers = {
      role   = "read_only"
      count  = 2
      cloud  = "aws"
      region = "us-east-1"
      tier   = "R40"
      resources = {
        storage = "200Gi"
      }
    }
  }

  replication = {
    mode                 = "synchronous"
    synchronous_replicas = 1
  }

  parameters = {
    work_mem             = "64MB"
    max_connections      = "200"
    effective_cache_size = "6GB"
  }

  timeouts {
    create = "45m"
  }

  lifecycle {
    prevent_destroy = true
  }
}
