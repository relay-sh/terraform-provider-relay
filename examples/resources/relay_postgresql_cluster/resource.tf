resource "relay_postgresql_cluster" "billing" {
  workspace_id = relay_workspace.prod.id
  name         = "billing"
  version      = "18"

  deletion_protection = true

  storage = {
    size = "200Gi"
    autoscale = {
      enabled     = true
      max_storage = "1000Gi"
    }
  }

  nodes = {
    writers = {
      role   = "electable"
      count  = 3
      cloud  = "aws"
      region = "us-east-1"
      tier   = "R30"
      resources = {
        disk_type = "gp3"
        iops      = 6000
      }
      autoscaling = {
        enabled  = true
        min_tier = "R20"
        max_tier = "R40"
        rules = [
          { metric = "cpu", scale_up_above = 75, scale_down_below = 30 },
          { metric = "postgres.connections", scale_up_above = 80, scale_down_below = 40 },
        ]
        scale_up_window            = "5m"
        scale_down_window          = "30m"
        respect_maintenance_window = true
      }
    }

    readers = {
      role      = "read_only"
      count     = 2
      cloud     = "aws"
      region    = "us-east-1"
      tier      = "R40"
      resources = {}
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
