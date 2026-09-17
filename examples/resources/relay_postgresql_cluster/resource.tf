resource "relay_postgresql_cluster" "billing" {
  workspace_id = relay_workspace.prod.id
  name         = "billing"
  version      = "18"

  nodes = {
    primary = {
      count  = 3
      cloud  = "aws"
      region = "us-east-1"
      resources = {
        cpu     = "2"
        memory  = "8Gi"
        storage = "100Gi"
      }
    }

    analytics = {
      count  = 2
      cloud  = "aws"
      region = "us-east-1"
      resources = {
        cpu     = "4"
        memory  = "16Gi"
        storage = "200Gi"
      }
      synchronous = false
      failover    = false
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
