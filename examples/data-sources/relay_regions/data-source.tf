data "relay_regions" "private_postgresql" {
  cloud             = "aws"
  engine            = "postgresql"
  private_endpoints = true
}

resource "relay_private_endpoint_service" "this" {
  for_each = { for r in data.relay_regions.private_postgresql.regions : r.region => r }

  workspace_id = relay_workspace.prod.id
  cloud        = each.value.cloud
  region       = each.value.region
}
