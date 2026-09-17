resource "relay_database" "billing" {
  workspace_id = relay_workspace.prod.id
  name         = "billing"
  engine       = "postgresql"
  version      = "17"

  nodes = [{
    role     = "data"
    count    = 2
    provider = "aws"
    region   = "us-east-1"
    resources = {
      cpu     = "2"
      memory  = "8Gi"
      storage = "100Gi"
    }
  }]

  postgresql_parameters = {
    work_mem = "64MB"
  }

  timeouts {
    create = "45m"
  }

  lifecycle {
    prevent_destroy = true
  }
}
