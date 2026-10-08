data "relay_private_endpoint" "app" {
  workspace_id    = data.relay_workspace.prod.id
  service_id      = data.relay_private_endpoint_service.use1.id
  vpc_endpoint_id = "vpce-0123456789abcdef0"
}
