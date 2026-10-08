data "relay_private_endpoint_service" "use1" {
  workspace_id = data.relay_workspace.prod.id
  cloud        = "aws"
  region       = "us-east-1"
}
